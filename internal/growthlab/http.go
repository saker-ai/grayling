package growthlab

import (
	"crypto/subtle"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"

	"time"

	"github.com/saker-ai/grayling/pkg/model"
)

type evaluated struct {
	FlagRevision      int64        `json:"flagRevision"`
	Result            model.Result `json:"result"`
	ExposureToken     string       `json:"exposureToken"`
	ManifestRevision  int64        `json:"manifestRevision"`
	RandomizationUnit string       `json:"randomizationUnit"`
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, v)
}
func decode(r *http.Request, v any) bool {
	if r.Header.Get("Content-Type") != "application/json" {
		return false
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, 16385))
	return e == nil && len(b) <= 16384 && json.Unmarshal(b, v, json.RejectUnknownMembers(true)) == nil
}
func (l *Lab) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /idp/session", l.login)
	mux.HandleFunc("POST /session/evaluate", l.evaluate)
	mux.HandleFunc("POST /session/event", l.event)
	mux.HandleFunc("POST /operator/advance", l.advance)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		name := "index.html"
		kind := "text/html; charset=utf-8"
		if r.URL.Path == "/app.js" {
			name = "app.js"
			kind = "text/javascript"
		} else if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := pages.ReadFile("ui/" + name)
		w.Header().Set("Content-Type", kind)
		_, _ = w.Write(b)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		// Literal host binding prevents DNS rebinding. Cookies never authorize the operator.
		if r.Host != l.appListener.Addr().String() || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != l.URL()) {
			reply(w, 403, map[string]string{"error": "origin_denied"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func (l *Lab) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Subject string `json:"subject"`
	}
	if !decode(r, &in) || !subjectPattern.MatchString(in.Subject) {
		reply(w, 400, map[string]string{"error": "synthetic_subject_required"})
		return
	}
	session := token()
	l.mu.Lock()
	if len(l.sessions) >= 2048 {
		l.mu.Unlock()
		reply(w, 429, map[string]string{"error": "session_limit"})
		return
	}
	l.sessions[session] = in.Subject
	l.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "growth_lab_session", Value: session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 3600})
	reply(w, 200, map[string]any{"synthetic": true, "subject": in.Subject})
}
func (l *Lab) identity(r *http.Request) (string, bool) {
	c, e := r.Cookie("growth_lab_session")
	if e != nil {
		return "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[c.Value]
	return s, ok
}
func (l *Lab) scenario(key string) (Scenario, bool) {
	for _, s := range l.State.Scenarios {
		if s.Key == key {
			return s, true
		}
	}
	return Scenario{}, false
}
func (l *Lab) evaluate(w http.ResponseWriter, r *http.Request) {
	subject, ok := l.identity(r)
	var in struct {
		Scenario string `json:"scenario"`
	}
	if !ok {
		reply(w, 401, map[string]string{"error": "session_required"})
		return
	}
	if !decode(r, &in) {
		reply(w, 400, map[string]string{"error": "invalid_input"})
		return
	}
	sc, ok := l.scenario(in.Scenario)
	if !ok {
		reply(w, 400, map[string]string{"error": "invalid_scenario"})
		return
	}
	var out evaluated
	if e := l.request(r.Context(), "POST", "/api/v1/sdk/eval", l.State.SDK, map[string]any{"flagKey": sc.Flag, "context": map[string]any{"attributes": map[string]any{"user_id": subject}}}, &out); e != nil || out.ExposureToken == "" {
		reply(w, 409, map[string]string{"error": "experiment_not_running"})
		return
	}
	ticket, err := l.Store.VerifyExposureTicket(out.ExposureToken)
	if err != nil {
		reply(w, 422, map[string]string{"error": "invalid_assignment"})
		return
	}
	out.FlagRevision = ticket.FlagRevision
	reply(w, 200, out)
}
func (l *Lab) event(w http.ResponseWriter, r *http.Request) {
	subject, ok := l.identity(r)
	if !ok {
		reply(w, 401, map[string]string{"error": "session_required"})
		return
	}
	var in struct {
		Scenario string    `json:"scenario"`
		Kind     string    `json:"kind"`
		Decision evaluated `json:"decision"`
	}
	if !decode(r, &in) {
		reply(w, 400, map[string]string{"error": "invalid_input"})
		return
	}
	sc, ok := l.scenario(in.Scenario)
	if !ok || in.Decision.Result.Experiment == nil || in.Decision.Result.FlagKey != sc.Flag || in.Decision.RandomizationUnit != subject {
		reply(w, 400, map[string]string{"error": "invalid_assignment"})
		return
	}
	d := in.Decision
	e := d.Result.Experiment
	metric := ""
	kind := "conversion"
	switch in.Kind {
	case "render":
		kind = "exposure"
	case "complete":
		metric = sc.Primary
	case "return":
		metric = sc.Guard
	default:
		reply(w, 400, map[string]string{"error": "invalid_event"})
		return
	}
	ev := map[string]any{"eventId": "synthetic-" + d.Result.DecisionID + "-" + in.Kind, "decisionId": d.Result.DecisionID, "type": kind, "experimentId": e.ExperimentID, "epoch": e.Epoch, "flagKey": sc.Flag, "flagRevision": d.ManifestRevision, "variant": e.Variant, "unit": subject, "source": e.Source, "metricKey": metric, "value": 1, "clientTime": l.Clock.Now(), "policyVersion": e.PolicyVersion, "stateVersion": e.StateVersion, "allocationProbability": e.AllocationProbability}
	// Preserve the exact assignment revision, including legacy zero-version flags.
	ev["flagRevision"] = d.FlagRevision
	if kind == "exposure" {
		ev["exposureToken"] = d.ExposureToken
	}
	var result map[string]any
	if err := l.request(r.Context(), "POST", "/api/v1/collect", l.State.Collect, map[string]any{"events": []any{ev}}, &result); err != nil {
		reply(w, 422, map[string]string{"error": "collection_rejected"})
		return
	}
	reply(w, 200, result)
}
func (l *Lab) advance(w http.ResponseWriter, r *http.Request) {
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(provided), []byte(l.State.Operator)) != 1 {
		reply(w, 403, map[string]string{"error": "operator_required"})
		return
	}
	var in struct {
		Hours int64 `json:"hours"`
	}
	if !decode(r, &in) || in.Hours < 1 || in.Hours > 24*30 {
		reply(w, 400, map[string]string{"error": "bounded_hours_required"})
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Clock.mu.Lock()
	l.Clock.at = l.Clock.at.Add(time.Duration(in.Hours) * time.Hour)
	l.Clock.mu.Unlock()
	if l.persist() != nil {
		reply(w, 500, map[string]string{"error": "clock_checkpoint_failed"})
		return
	}
	reply(w, 200, map[string]any{"synthetic": true, "at": l.Clock.Now(), "limitation": "Virtual elapsed time; no real-user retention outcome."})
}
