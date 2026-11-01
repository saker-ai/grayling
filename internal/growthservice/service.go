package growthservice

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
	"golang.org/x/time/rate"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)

type Principal struct{ Name, Token, Role string }
type Config struct {
	Evidence   growth.Config
	Principals []Principal
	// Model is nil by default. Providers receive only pinned aggregate identity,
	// never goal prose, tokens, identities, descriptions, reports or tool access.
	Model         Planner
	MaxModelCalls int
	MaxTasks      int
}
type Aggregate struct {
	Arm            int      `json:"arm"`
	ExposedUnits   *int64   `json:"exposedUnits"`
	ConvertedUnits *int64   `json:"convertedUnits"`
	PrimaryValue   *float64 `json:"primaryValue"`
}
type PlannerInput struct {
	Aggregates      []Aggregate      `json:"aggregates"`
	Primary         growth.MetricRef `json:"primary"`
	EvidenceVersion string           `json:"evidenceVersion"`
}
type Suggestion struct {
	Hypothesis      string `json:"hypothesis"`
	Mechanism       string `json:"mechanism"`
	Falsification   string `json:"falsification"`
	Risk            string `json:"risk"`
	Effort          string `json:"effort"`
	EvidenceVersion string `json:"evidenceVersion"`
}

// Plan must honor context cancellation and return at most 16 KiB strict JSON.
// Only trusted in-process adapters can be configured; there is no arbitrary URL/tool.
type Planner interface {
	Plan(context.Context, PlannerInput) ([]byte, error)
}

type Service struct {
	limits  map[string]*rate.Limiter
	store   *Store
	cfg     Config
	reader  *growth.Reader
	wake    chan struct{}
	work    sync.Mutex
	actions sync.Mutex
}

func New(ctx context.Context, store *Store, c Config) (*Service, error) {
	if store == nil || len(c.Principals) == 0 || len(c.Evidence.BearerToken) < 32 {
		return nil, ErrInput
	}
	if len(c.Principals) > 64 {
		return nil, ErrInput
	}
	seen := map[string]bool{}
	names := map[string]bool{}
	for _, p := range c.Principals {
		if !identifier.MatchString(p.Name) || len(p.Token) < 32 || strings.ContainsAny(p.Token, "\r\n") || seen[p.Token] || names[p.Name] || p.Token == c.Evidence.BearerToken || strings.TrimSpace(p.Token) != p.Token || (p.Role != "viewer" && p.Role != "editor" && p.Role != "reviewer") {
			return nil, ErrInput
		}
		seen[p.Token] = true
		names[p.Name] = true
	}
	if c.MaxTasks == 0 {
		c.MaxTasks = 64
	}
	if c.MaxTasks < 1 || c.MaxTasks > 64 || c.MaxModelCalls < 0 || c.MaxModelCalls > 10 {
		return nil, ErrInput
	}
	c.Principals = append([]Principal(nil), c.Principals...)
	r, err := growth.NewReader(c.Evidence)
	if err != nil {
		return nil, err
	}
	if err = store.bind(ctx, c.Evidence.WorkspaceID, c.Evidence.EnvironmentID); err != nil {
		r.Close()
		return nil, err
	}
	limits := map[string]*rate.Limiter{}
	for _, p := range c.Principals {
		limits[p.Name] = rate.NewLimiter(rate.Every(time.Second), 30)
	}
	return &Service{store: store, cfg: c, reader: r, wake: make(chan struct{}, 1), limits: limits}, nil
}
func (s *Service) Close() { s.reader.Close() }
func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run recovers durable queued/analyzing tasks on every start. Completion uses
// revision CAS: cancel/edit always wins over an in-flight provider response.
func (s *Service) Run(ctx context.Context) {
	defer s.Close()
	s.signal()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
		s.process(ctx)
	}
}
func (s *Service) process(ctx context.Context) {
	s.work.Lock()
	defer s.work.Unlock()
	tasks, err := s.store.list(ctx)
	if err != nil {
		return
	}
	for _, t := range tasks {
		if ctx.Err() != nil {
			return
		}
		if t.SubmissionConfirmed && t.State != "CANCELED" && t.State != "LEARNING_RECORDED" && time.Now().After(t.NextObservationAt) {
			s.observe(ctx, t)
			continue
		}
		if t.State != "QUEUED" && t.State != "ANALYZING" {
			continue
		}
		s.analyze(ctx, t)
	}
}
func (s *Service) analyze(ctx context.Context, t *Task) {
	t, err := s.store.change(ctx, t.ID, t.Revision, "service", "analysis_started", func(t *Task) error {
		t.State = "ANALYZING"
		t.Attempts++
		if t.Attempts > 3 {
			t.State = "HOLD"
			t.ErrorCode = "retry_budget_exhausted"
		}
		return nil
	})
	if err != nil || t.State != "ANALYZING" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	q := growth.Query{Metrics: []growth.MetricRef{t.Goal.PrimaryMetric.Ref}}
	for _, g := range t.Goal.Guardrails {
		q.Metrics = append(q.Metrics, g.Ref)
	}
	if t.Manifest != nil {
		q.Experiments = []growth.ExperimentRef{{ID: t.Manifest.ExperimentID, Epoch: t.Manifest.Epoch, EvidenceVersion: t.Manifest.EvidenceVersion}}
	}
	evidence, readErr := s.reader.Read(ctx, q)
	state, code := "NEEDS_DATA", "baseline_missing"
	var suggestions []Suggestion
	var observation *growth.MonitorSummary
	if readErr != nil {
		code = "evidence_unavailable"
		if errors.Is(readErr, growth.ErrMismatch) || errors.Is(readErr, growth.ErrResponse) {
			state, code = "HOLD", "evidence_conflict"
		}
		if errors.Is(readErr, growth.ErrAuthorization) {
			state, code = "HOLD", "evidence_permission_denied"
		}
	} else if t.Manifest != nil {
		check, e := s.reader.CheckGoal(ctx, t.Goal, *t.Manifest)
		if e != nil {
			state, code = "HOLD", "evidence_conflict"
		} else {
			observation = check.Observation
			state = string(check.Status)
			code = ""
			if check.Status != growth.Ready {
				code = "review_evidence_blockers"
			}
		}
	}
	// The envelope and goal monitor are separate reads. Never join healthy
	// metadata from a newer report to older unhealthy facts or model inputs.
	if state == "READY" && (evidence.Status != growth.Ready || len(evidence.Evidence) != 1 || observation == nil || observation.EvidenceVersion != evidence.Evidence[0].Report.EvidenceVersion || observation.RegistrationVersion != evidence.Evidence[0].Report.RegistrationVersion || observation.ServerVerdict != evidence.Evidence[0].Report.Decision.Verdict) {
		state, code = "HOLD", "evidence_snapshot_changed"
		observation = nil
	}
	if state == "READY" && s.cfg.Model != nil && !modelAggregatesReady(evidence) {
		state, code = "HOLD", "aggregate_privacy_threshold"
	}
	if state == "READY" && s.cfg.Model != nil && s.cfg.MaxModelCalls > t.ModelCalls {
		// Reserve budget before calling the provider: crashes cannot reset spending.
		t, err = s.store.change(ctx, t.ID, t.Revision, "service", "model_budget_reserved", func(t *Task) error { t.ModelCalls++; return nil })
		if err != nil {
			return
		}
		pin := ""
		if len(evidence.Evidence) > 0 {
			pin = evidence.Evidence[0].Report.EvidenceVersion
		}
		aggregates := []Aggregate{}
		if len(evidence.Evidence) > 0 {
			for i, v := range evidence.Evidence[0].Report.PerVariant {
				aggregates = append(aggregates, Aggregate{Arm: i, ExposedUnits: v.ExposedUnits, ConvertedUnits: v.ConvertedUnits, PrimaryValue: v.PrimaryValue})
			}
		}
		raw, e := s.cfg.Model.Plan(ctx, PlannerInput{Primary: t.Goal.PrimaryMetric.Ref, EvidenceVersion: pin, Aggregates: aggregates})
		if e != nil || len(raw) > 16<<10 || json.Unmarshal(raw, &suggestions, json.RejectUnknownMembers(true)) != nil || s.containsSecret(raw) || !validSuggestions(suggestions, pin) {
			state, code = "HOLD", "invalid_model_output"
			suggestions = nil
		}
	}
	if state == "READY" && s.cfg.Model != nil && len(suggestions) == 0 {
		state, code = "HOLD", "model_budget_exhausted"
	}
	if state == "READY" {
		state = "DIAGNOSED"
	}
	_, checkpointErr := s.store.change(ctx, t.ID, t.Revision, "service", "analysis_completed", func(t *Task) error {
		t.Observation = observation
		t.Evidence = sanitize(evidence)
		t.Suggestions = suggestions
		t.State = state
		t.ErrorCode = code
		return nil
	})
	checkpointFailure(checkpointErr)
}
func validSuggestions(items []Suggestion, pin string) bool {
	if len(items) < 2 || len(items) > 3 {
		return false
	}
	seen := map[string]bool{}
	for _, v := range items {
		normalized := strings.ToLower(strings.Join(strings.Fields(v.Hypothesis), " "))
		if seen[normalized] {
			return false
		}
		seen[normalized] = true
		if v.EvidenceVersion != pin || (v.Risk != "low" && v.Risk != "medium" && v.Risk != "high") || (v.Effort != "small" && v.Effort != "medium" && v.Effort != "large") {
			return false
		}
		for _, text := range []string{v.Hypothesis, v.Mechanism, v.Falsification} {
			if strings.TrimSpace(text) == "" || len(text) > 2000 || strings.ContainsAny(text, "0123456789%$") {
				return false
			}
		}
	}
	return true
}

func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serve) }
func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	if r.Method == "GET" && (r.URL.Path == "/" || r.URL.Path == "/app.js" || r.URL.Path == "/style.css") {
		s.page(w, r)
		return
	}
	fail := func(status int, code string) { write(w, status, map[string]string{"error": code}) }
	// Cookie authentication is deliberately unsupported. Browser requests require
	// explicit bearer plus JSON, and cross-origin requests are never permitted.
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
		fail(403, "origin_denied")
		return
	}
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var principal *Principal
	h := sha256.Sum256([]byte(secret))
	for _, p := range s.cfg.Principals {
		expected := sha256.Sum256([]byte(p.Token))
		if subtle.ConstantTimeCompare(h[:], expected[:]) == 1 {
			copy := p
			principal = &copy
		}
	}
	if principal == nil {
		fail(401, "unauthorized")
		return
	}
	if !s.limits[principal.Name].Allow() {
		fail(429, "rate_limited")
		return
	}
	if r.Method == "GET" && r.URL.Path == "/v1/learning" {
		records, e := s.store.journal(r.Context())
		if e != nil {
			fail(500, "storage_failed")
			return
		}
		query := growth.LearningSearchQuery{WorkspaceID: s.cfg.Evidence.WorkspaceID, EnvironmentID: s.cfg.Evidence.EnvironmentID, Text: r.URL.Query().Get("text")}
		if query.Validate() != nil {
			fail(400, "invalid_search")
			return
		}
		result := &growth.LearningSearchResult{SchemaVersion: "growth.learning-search.v1", WorkspaceID: query.WorkspaceID, EnvironmentID: query.EnvironmentID, Matches: []growth.LearningRecord{}}
		if len(records) > 0 {
			result, e = growth.SearchLearning(records, query)
			if e != nil {
				fail(400, "invalid_search")
				return
			}
		}
		write(w, 200, struct {
			*growth.LearningSearchResult
			Groups []LearningGroup `json:"groups"`
		}{result, groupLearning(result.Matches)})
		return
	}
	if r.Method == "GET" && r.URL.Path == "/v1/tasks" {
		tasks, err := s.store.list(r.Context())
		if err != nil {
			fail(500, "storage_failed")
			return
		}
		type Summary struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
			State    string `json:"state"`
			Goal     struct {
				Objective string `json:"objective"`
			} `json:"goal"`
		}
		out := []Summary{}
		for _, t := range tasks {
			summary := Summary{ID: t.ID, Revision: t.Revision, State: t.State}
			summary.Goal.Objective = t.Goal.Objective
			out = append(out, summary)
		}
		write(w, 200, out)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" || parts[1] != "tasks" {
		fail(404, "not_found")
		return
	}
	if r.Method == "POST" && len(parts) == 2 {
		if principal.Role == "viewer" {
			fail(403, "permission_denied")
			return
		}
		var in struct {
			Goal     growth.GrowthGoal       `json:"goal"`
			Manifest *growth.MonitorManifest `json:"manifest,omitempty"`
		}
		if !s.decode(w, r, &in) || growth.ValidateGoal(in.Goal) != nil || in.Goal.WorkspaceID != s.cfg.Evidence.WorkspaceID || in.Goal.EnvironmentID != s.cfg.Evidence.EnvironmentID || (!identifier.MatchString(r.Header.Get("Idempotency-Key")) || s.containsSecret([]byte(r.Header.Get("Idempotency-Key")))) {
			fail(400, "invalid_goal_or_key")
			return
		}
		if in.Manifest != nil {
			b, _ := json.Marshal(in.Manifest)
			m, e := growth.DecodeMonitorManifest(strings.NewReader(string(b)))
			if e != nil || m.WorkspaceID != in.Goal.WorkspaceID || m.EnvironmentID != in.Goal.EnvironmentID || m.PrimaryMetric != in.Goal.PrimaryMetric.Ref {
				fail(400, "invalid_manifest")
				return
			}
		}
		// Serialize count + insert to enforce the service resource bound.
		s.actions.Lock()
		defer s.actions.Unlock()
		tasks, err := s.store.list(r.Context())
		if err != nil {
			fail(500, "storage_failed")
			return
		}
		var priorID, priorHash string
		replayErr := s.store.db.QueryRowContext(r.Context(), `SELECT id,input_hash FROM tasks WHERE request_key=?`, r.Header.Get("Idempotency-Key")).Scan(&priorID, &priorHash)
		if replayErr == nil {
			inputHash := digest(struct {
				Goal     growth.GrowthGoal
				Manifest *growth.MonitorManifest
			}{in.Goal, in.Manifest})
			if inputHash != priorHash {
				fail(409, "idempotency_conflict")
				return
			}
			prior, e := s.store.get(r.Context(), priorID)
			if e != nil {
				fail(500, "storage_failed")
				return
			}
			write(w, 202, prior)
			return
		}
		if len(tasks) >= s.cfg.MaxTasks {
			fail(429, "task_budget_exhausted")
			return
		}
		idBytes := make([]byte, 16)
		if _, err = rand.Read(idBytes); err != nil {
			fail(500, "entropy_failed")
			return
		}
		id := "task_" + hex.EncodeToString(idBytes)
		p, err := growth.InitProposal(in.Goal, id, 1, in.Goal.WorkspaceID, in.Goal.EnvironmentID)
		if err != nil {
			fail(400, "invalid_goal")
			return
		}
		t, err := s.store.create(r.Context(), &Task{ID: id, Goal: in.Goal, Proposal: *p, Manifest: in.Manifest, State: "QUEUED", Suggestions: []Suggestion{}}, r.Header.Get("Idempotency-Key"), principal.Name)
		if err != nil {
			fail(409, "idempotency_conflict")
			return
		}
		s.signal()
		write(w, 202, t)
		return
	}
	if len(parts) < 3 || !identifier.MatchString(parts[2]) {
		fail(404, "not_found")
		return
	}
	t, err := s.store.get(r.Context(), parts[2])
	if err != nil {
		fail(404, "not_found")
		return
	}
	if r.Method == "GET" && len(parts) == 3 {
		write(w, 200, t)
		return
	}
	if r.Method == "GET" && len(parts) == 4 && parts[3] == "versions" {
		v, e := s.store.versions(r.Context(), t.ID)
		if e != nil {
			fail(500, "storage_failed")
			return
		}
		write(w, 200, v)
		return
	}
	if r.Method == "GET" && len(parts) == 4 && parts[3] == "audit" {
		a, e := s.store.history(r.Context(), t.ID)
		if e != nil {
			fail(500, "storage_failed")
			return
		}
		write(w, 200, a)
		return
	}
	if r.Method != "POST" || len(parts) != 4 {
		fail(404, "not_found")
		return
	}
	if principal.Role == "viewer" {
		fail(403, "permission_denied")
		return
	}
	action := parts[3]
	switch action {
	case "edit", "review", "prepare", "submit", "platform-review", "bind", "monitor", "learning", "retry", "cancel":
	default:
		fail(404, "not_found")
		return
	}
	var in Action
	if !s.decode(w, r, &in) {
		fail(400, "invalid_json")
		return
	}
	if (action == "review" || action == "submit" || action == "platform-review" || action == "bind") && principal.Role != "reviewer" {
		fail(403, "reviewer_required")
		return
	}
	s.actions.Lock()
	defer s.actions.Unlock()
	t, err = s.store.get(r.Context(), t.ID)
	if err != nil {
		fail(500, "storage_failed")
		return
	}
	if t.Revision != in.Revision {
		fail(409, "stale_revision")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := s.action(ctx, t, *principal, action, in, r.Header.Get("X-Grayling-Token"))
	if err != nil {
		code, remediation := "action_blocked", "Review the task evidence and required inputs, then refresh and retry the explicit action."
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrFlagChanged) {
			code = "state_or_evidence_conflict"
			remediation = "Refresh the task; resolve flag revision, metric pins or competing experiments. Prepare and review a new proposal when its contract changes."
		}
		if errors.Is(err, growth.ErrAuthorization) {
			code = "platform_permission_denied"
			remediation = "Request an authorized scoped human credential; service/model permissions cannot override Grayling RBAC."
		}
		var readiness *readinessError
		var review *growth.ProposalReview
		if errors.As(err, &readiness) {
			review = &readiness.Review
		}
		latest, e := s.store.get(r.Context(), t.ID)
		if e == nil {
			_, checkpointErr := s.store.change(r.Context(), latest.ID, latest.Revision, principal.Name, action+"_blocked", func(t *Task) error {
				t.ErrorCode = code
				t.Remediation = remediation
				t.Readiness = review
				if errors.Is(err, ErrFlagChanged) {
					t.Prepared = nil
					t.PreparedFlagRevision = 0
					t.Review = nil
					t.State = "HUMAN_REVIEW"
				}
				return nil
			})
			checkpointFailure(checkpointErr)
		}
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrFlagChanged) {
			fail(409, "state_or_evidence_conflict")
		} else {
			fail(422, code)
		}
		return
	}
	write(w, 200, out)
}
func (s *Service) decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if r.Header.Get("Content-Type") != "application/json" {
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if err != nil || s.containsSecret(raw) || (len(r.Header.Get("X-Grayling-Token")) > 0 && strings.Contains(string(raw), r.Header.Get("X-Grayling-Token"))) {
		return false
	}
	if json.Unmarshal(raw, out, json.RejectUnknownMembers(true)) != nil {
		return false
	}
	canonical, err := json.Marshal(out)
	return err == nil && !s.containsSecret(canonical) && (r.Header.Get("X-Grayling-Token") == "" || !strings.Contains(string(canonical), r.Header.Get("X-Grayling-Token")))
}
func write(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encoding_failed", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (s *Service) containsSecret(raw []byte) bool {
	if strings.Contains(string(raw), s.cfg.Evidence.BearerToken) {
		return true
	}
	for _, p := range s.cfg.Principals {
		if strings.Contains(string(raw), p.Token) {
			return true
		}
	}
	return false
}

// This is a conservative model-disclosure floor, never a statistical/sample
// adequacy rule. A real deployment still needs its approved privacy policy.
func modelAggregatesReady(e *growth.Envelope) bool {
	if e == nil || len(e.Evidence) != 1 || len(e.Evidence[0].Report.PerVariant) < 2 {
		return false
	}
	for _, v := range e.Evidence[0].Report.PerVariant {
		if v.ExposedUnits == nil || *v.ExposedUnits < 20 {
			return false
		}
	}
	return true
}
func checkpointFailure(err error) {
	if err != nil && !errors.Is(err, ErrConflict) {
		slog.Error("growth checkpoint unavailable", "code", "storage_failed")
	}
}
