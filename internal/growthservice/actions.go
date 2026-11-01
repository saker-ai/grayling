package growthservice

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

var ErrFlagChanged = errors.New("growth service: prepared flag revision changed")

type Action struct {
	Revision  int64                     `json:"revision"`
	Proposal  *growth.Proposal          `json:"proposal,omitempty"`
	Decision  growth.ReviewDecision     `json:"decision,omitempty"`
	Rationale string                    `json:"rationale,omitempty"`
	Settings  *growth.PreparationInputs `json:"settings,omitempty"`
	Followups []string                  `json:"followups,omitempty"`
}

func (s *Service) evidence(ctx context.Context, p growth.Proposal) (*growth.Envelope, error) {
	q := growth.Query{Metrics: []growth.MetricRef{p.PrimaryMetric.Ref}}
	for _, g := range p.Guardrails {
		q.Metrics = append(q.Metrics, g.Ref)
	}
	for _, r := range p.EvidenceRefs {
		q.Experiments = append(q.Experiments, growth.ExperimentRef{ID: r.ExperimentID, Epoch: r.Epoch, EvidenceVersion: r.EvidenceVersion})
	}
	return s.reader.Read(ctx, q)
}
func (s *Service) action(ctx context.Context, t *Task, p Principal, action string, in Action, upstreamToken string) (*Task, error) {
	save := func(fn func(*Task) error) (*Task, error) {
		return s.store.change(ctx, t.ID, t.Revision, p.Name, action, func(task *Task) error {
			if err := fn(task); err != nil {
				return err
			}
			task.ErrorCode = ""
			task.Remediation = ""
			task.Readiness = nil
			return nil
		})
	}
	if t.State == "CANCELED" {
		return nil, ErrConflict
	}
	switch action {
	case "cancel":
		return save(func(t *Task) error { t.State = "CANCELED"; t.ErrorCode = ""; return nil })
	case "retry":
		if t.State != "HOLD" && t.State != "NEEDS_DATA" {
			return nil, ErrConflict
		}
		if t.ReservedEpoch != 0 {
			return nil, ErrConflict
		}
		return save(func(t *Task) error { t.State = "QUEUED"; t.ErrorCode = ""; return nil })
	case "edit":
		if in.Proposal == nil || in.Proposal.GoalPin == nil || t.ReservedEpoch != 0 {
			return nil, ErrInput
		}
		candidate := *in.Proposal
		if candidate.ID != t.Proposal.ID || candidate.Version != t.Proposal.Version+1 {
			return nil, ErrConflict
		}
		if _, err := growth.VerifyGoal(t.Goal, candidate, s.cfg.Evidence.WorkspaceID, s.cfg.Evidence.EnvironmentID); err != nil {
			return nil, err
		}
		return save(func(t *Task) error {
			t.Proposal = candidate
			t.Review = nil
			t.Prepared = nil
			t.State = "IMPLEMENTATION_CHECK"
			t.ErrorCode = ""
			return nil
		})
	case "review":
		if t.ReservedEpoch != 0 {
			return nil, ErrConflict
		}
		evidence, err := s.evidence(ctx, t.Proposal)
		if err != nil && in.Decision == growth.ReviewApprove {
			return nil, err
		}
		fp, err := growth.ProposalFingerprint(t.Proposal)
		if err != nil {
			return nil, err
		}
		readiness := growth.ReviewProposal(t.Proposal, evidence)
		if in.Decision == growth.ReviewApprove && readiness.State != growth.ProposalReadyForReview {
			return nil, &readinessError{readiness}
		}
		record, err := growth.RecordReview(t.Proposal, evidence, growth.ReviewInputs{ExpectedProposalFingerprint: fp, Decision: in.Decision, Reviewer: p.Name, ReviewedAt: time.Now().UTC(), Rationale: in.Rationale})
		if err != nil {
			return nil, err
		}
		return save(func(t *Task) error {
			t.Review = record
			t.Evidence = sanitize(evidence)
			t.Prepared = nil
			t.State = map[growth.ReviewDecision]string{growth.ReviewApprove: "APPROVED", growth.ReviewReject: "REJECTED", growth.ReviewRequestChanges: "CHANGES_REQUESTED"}[record.Decision]
			return nil
		})
	case "prepare":
		if t.State != "APPROVED" && t.State != "PREPARED" {
			return nil, ErrConflict
		}
		if t.Review == nil || in.Settings == nil || t.ReservedEpoch != 0 {
			return nil, ErrInput
		}
		evidence, err := s.evidence(ctx, t.Proposal)
		if err != nil {
			return nil, err
		}
		prepared, err := growth.CompileReviewedCreateRequest(t.Proposal, *t.Review, s.cfg.Evidence.WorkspaceID, s.cfg.Evidence.EnvironmentID, evidence, *in.Settings)
		if err != nil {
			return nil, err
		}
		revision, err := s.flagRevision(ctx, t.Proposal.FlagKey)
		if err != nil {
			return nil, err
		}
		return save(func(t *Task) error {
			t.PreparedFlagRevision = revision
			t.Prepared = &prepared.PreparedExperiment
			t.Evidence = sanitize(evidence)
			t.State = "PREPARED"
			return nil
		})
	case "submit":
		return s.submit(ctx, t, p, upstreamToken)
	case "platform-review", "bind":
		if t.Manifest == nil || !t.SubmissionConfirmed {
			return nil, ErrConflict
		}
		if upstreamToken == "" {
			return nil, ErrInput
		}
		// Every downstream action uses the human's ephemeral credential and existing
		// RBAC. There are no publish, transition, rollout-advance or rollback tools.
		var current experimentSnapshot
		if err := s.request(ctx, "GET", "/api/v1/admin/experiments/"+t.Manifest.ExperimentID, upstreamToken, nil, &current); err != nil {
			return nil, err
		}
		if !current.matches(*t.Manifest) {
			return nil, ErrConflict
		}
		if action == "platform-review" && current.State != "draft" && current.State != "reviewed" {
			return nil, ErrConflict
		}
		// Reconcile state before retrying an interrupted side effect. Binding changes
		// reviewed -> scheduled atomically in the existing core.
		completed := (action == "platform-review" && current.State == "reviewed") || (action == "bind" && current.State == "scheduled")
		if !completed {
			if action == "bind" {
				m := rolloutManifest(*t.Manifest)
				obs, e := s.reader.BindCheck(ctx, m)
				if e != nil || !obs.Ready {
					return nil, ErrConflict
				}
			}
			endpoint := "review"
			if action == "bind" {
				endpoint = "bind"
			}
			if err := s.request(ctx, "POST", "/api/v1/admin/experiments/"+t.Manifest.ExperimentID+"/"+endpoint, upstreamToken, map[string]string{"note": "Explicit Growth Copilot human action"}, nil); err != nil {
				return nil, err
			}
		}
		return save(func(t *Task) error {
			if action == "bind" {
				t.State = "BOUND"
			} else {
				t.State = "PLATFORM_REVIEWED"
			}
			return nil
		})
	case "monitor", "learning":
		if t.Manifest == nil || !t.SubmissionConfirmed {
			return nil, ErrConflict
		}
		if action == "learning" {
			fp, e := growth.ProposalFingerprint(t.Proposal)
			if e != nil {
				return nil, e
			}
			record, e := s.reader.RecordLearning(ctx, t.Proposal, *t.Manifest, growth.LearningInputs{ExpectedProposalFingerprint: fp, Recorder: p.Name, RecordedAt: time.Now().UTC(), Followups: in.Followups})
			if e != nil {
				return nil, e
			}
			return save(func(t *Task) error { t.Learning = record; t.State = "LEARNING_RECORDED"; return nil })
		}
		observation, e := s.reader.Monitor(ctx, *t.Manifest)
		if e != nil {
			return nil, e
		}
		rollout, e := s.reader.Rollout(ctx, rolloutManifest(*t.Manifest))
		if e != nil {
			return nil, e
		}
		return save(func(t *Task) error {
			t.Observation = observation
			t.NextObservationAt = time.Now().UTC().Add(30 * time.Second)
			t.Rollout = rollout
			t.State = string(observation.Status)
			if observation.Status == growth.Ready {
				t.State = "REPORT_READY"
			}
			t.ErrorCode = ""
			return nil
		})
	default:
		return nil, ErrInput
	}
}
func rolloutManifest(m growth.MonitorManifest) growth.RolloutManifest {
	return growth.RolloutManifest{WorkspaceID: m.WorkspaceID, EnvironmentID: m.EnvironmentID, ExperimentID: m.ExperimentID, Epoch: m.Epoch, RegistrationVersion: m.RegistrationVersion}
}

type experimentSnapshot struct {
	FlagRevision int64 `json:"flagRevision"`
	growth.CreateRequest
	ID                  string `json:"id"`
	Epoch               int64  `json:"epoch"`
	State               string `json:"state"`
	RegistrationVersion string `json:"registrationVersion"`
}

func (e experimentSnapshot) matches(m growth.MonitorManifest) bool {
	return e.ID == m.ExperimentID && e.Epoch == m.Epoch && e.WorkspaceID == m.WorkspaceID && e.EnvironmentID == m.EnvironmentID && e.RegistrationVersion == m.RegistrationVersion
}
func (s *Service) submit(ctx context.Context, t *Task, p Principal, token string) (*Task, error) {
	if token == "" || t.Prepared == nil || t.Review == nil || (!t.SubmissionConfirmed && t.State != "PREPARED" && t.State != "SUBMITTING") {
		return nil, ErrConflict
	}
	path := "/api/v1/admin/workspaces/" + s.cfg.Evidence.WorkspaceID + "/environments/" + s.cfg.Evidence.EnvironmentID + "/experiments"
	var experiments []experimentSnapshot
	if err := s.request(ctx, "GET", path, token, nil, &experiments); err != nil {
		return nil, err
	}
	request := t.Prepared.Request
	if t.ReservedEpoch == 0 {
		// Refresh all pinned evidence immediately before freezing the request.
		evidence, err := s.evidence(ctx, t.Proposal)
		if err != nil || growth.ReviewProposal(t.Proposal, evidence).State != growth.ProposalReadyForReview {
			return nil, ErrConflict
		}
		revision, err := s.flagRevision(ctx, request.FlagKey)
		if err != nil {
			return nil, err
		}
		if revision != t.PreparedFlagRevision {
			return nil, ErrFlagChanged
		}
		next := int64(1)
		for _, e := range experiments {
			if e.FlagKey == request.FlagKey && e.State != "stopped" && e.State != "analyzed" && e.State != "archived" {
				return nil, ErrConflict
			}
			if e.FlagKey == request.FlagKey && e.Epoch >= next {
				next = e.Epoch + 1
			}
		}
		if next <= 0 {
			return nil, ErrConflict
		}
		t, err = s.store.change(ctx, t.ID, t.Revision, p.Name, "submission_intent", func(t *Task) error {
			t.ReservedEpoch = next
			t.SubmissionHash = digest(request)
			t.State = "SUBMITTING"
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if t.SubmissionHash != digest(request) {
		return nil, ErrConflict
	}
	var created *experimentSnapshot
	for _, e := range experiments {
		if e.FlagKey == request.FlagKey && e.Epoch == t.ReservedEpoch {
			if digest(e.CreateRequest) != t.SubmissionHash {
				return nil, ErrConflict
			}
			copy := e
			created = &copy
			break
		}
	}
	if created == nil && t.SubmissionConfirmed {
		return nil, ErrConflict
	}
	if created == nil {
		var out experimentSnapshot
		body := struct {
			growth.CreateRequest
			Epoch        int64 `json:"epoch"`
			FlagRevision int64 `json:"flagRevision"`
		}{request, t.ReservedEpoch, t.PreparedFlagRevision}
		if err := s.request(ctx, "POST", path, token, body, &out); err != nil {
			return nil, err
		}
		created = &out
	}
	if created.ID == "" || created.FlagRevision != t.PreparedFlagRevision || created.Epoch != t.ReservedEpoch || created.RegistrationVersion == "" || digest(created.CreateRequest) != t.SubmissionHash {
		return nil, ErrConflict
	}
	if t.SubmissionConfirmed && t.Manifest != nil && created.matches(*t.Manifest) {
		return t, nil
	}
	return s.store.change(ctx, t.ID, t.Revision, p.Name, "submission_confirmed", func(t *Task) error {
		guards := []growth.MetricRef{}
		for _, g := range t.Proposal.Guardrails {
			guards = append(guards, g.Ref)
		}
		t.Manifest = &growth.MonitorManifest{WorkspaceID: t.Goal.WorkspaceID, EnvironmentID: t.Goal.EnvironmentID, ExperimentID: created.ID, Epoch: created.Epoch, RegistrationVersion: created.RegistrationVersion, PrimaryMetric: t.Proposal.PrimaryMetric.Ref, Guardrails: guards}
		t.State = "SUBMITTED"
		t.SubmissionConfirmed = true
		return nil
	})
}

// request never returns server bodies or transport errors to callers or logs.
func (s *Service) request(ctx context.Context, method, path, token string, in, out any) error {
	if len(token) < 1 {
		return ErrInput
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return ErrInput
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.cfg.Evidence.BaseURL+path, body)
	if err != nil {
		return ErrInput
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("growth service: platform request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return growth.ErrAuthorization
	}
	if resp.StatusCode == 409 {
		return ErrConflict
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("growth service: platform status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil || len(b) > 2<<20 {
		return ErrInput
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return ErrInput
	}
	return nil
}

// observe is GET-only and scheduled independently of HTTP requests. Canceled
// and completed learning tasks are excluded by the worker, and revision CAS
// prevents stale observations from overwriting a human action.
func (s *Service) observe(parent context.Context, t *Task) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	observation, err := s.reader.Monitor(ctx, *t.Manifest)
	var rollout *growth.RolloutObservation
	if err == nil {
		rollout, err = s.reader.Rollout(ctx, rolloutManifest(*t.Manifest))
	}
	_, checkpointErr := s.store.change(ctx, t.ID, t.Revision, "service", "readonly_monitor", func(t *Task) error {
		t.NextObservationAt = time.Now().UTC().Add(30 * time.Second)
		if err != nil {
			t.State = "HOLD"
			t.ErrorCode = "monitor_evidence_unavailable"
			return nil
		}
		t.Observation = observation
		t.Rollout = rollout
		t.State = string(observation.Status)
		if observation.Status == growth.Ready {
			t.State = "REPORT_READY"
		}
		t.ErrorCode = ""
		return nil
	})
	checkpointFailure(checkpointErr)
}

func (s *Service) flagRevision(ctx context.Context, key string) (int64, error) {
	if !identifier.MatchString(key) {
		return 0, ErrInput
	}
	var flag struct {
		Key             string `json:"key"`
		CurrentRevision int64  `json:"currentRevision"`
		Current         *struct {
			WorkspaceID   string `json:"workspaceId"`
			EnvironmentID string `json:"environmentId"`
			Key           string `json:"key"`
			Revision      int64  `json:"revision"`
		} `json:"current"`
	}
	path := "/api/v1/admin/workspaces/" + s.cfg.Evidence.WorkspaceID + "/environments/" + s.cfg.Evidence.EnvironmentID + "/flags/" + key
	if err := s.request(ctx, "GET", path, s.cfg.Evidence.BearerToken, nil, &flag); err != nil {
		return 0, err
	}
	if flag.Key != key || flag.CurrentRevision < 1 || flag.Current == nil || flag.Current.Key != key || flag.Current.Revision != flag.CurrentRevision || flag.Current.WorkspaceID != s.cfg.Evidence.WorkspaceID || flag.Current.EnvironmentID != s.cfg.Evidence.EnvironmentID {
		return 0, ErrConflict
	}
	return flag.CurrentRevision, nil
}

type readinessError struct{ Review growth.ProposalReview }

func (e *readinessError) Error() string { return "growth service: proposal not ready" }
