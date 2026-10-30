package growth

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func rolloutManifestFixture() RolloutManifest {
	return RolloutManifest{"ws1", "env1", "exp1", 1, hashA}
}
func rolloutResponseFixture() map[string]any {
	return map[string]any{"id": "exp1", "workspaceId": "ws1", "environmentId": "env1", "epoch": 1, "registrationVersion": hashA, "updatedAt": "2026-01-01T00:00:00Z", "releasePlan": map[string]any{
		"state": "armed", "currentStep": 0, "steps": []any{map[string]any{"name": "secret publish immediately", "ratio": 100, "minRuntime": 1, "autoAdvance": false}},
		"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z", "createdBy": "secret", "pausedReason": "secret", "history": []any{map[string]any{"at": "2026-01-01T00:00:00Z", "action": "plan.create", "step": 0, "actor": "secret", "detail": "secret publish immediately"}},
	}}
}
func TestRolloutManifest(t *testing.T) {
	b, _ := json.Marshal(rolloutManifestFixture())
	for _, raw := range []string{string(b), `{}`, `null`, `[]`, `{} {}`, `{"epoch":1,"\u0065poch":1}`, `{"unknown":true}`, strings.Repeat(" ", MaxRolloutManifestBytes+1), strings.Replace(string(b), "exp1", "../exp1", 1), strings.Replace(string(b), `"epoch":1`, `"epoch":0`, 1), strings.Replace(string(b), hashA, "bad", 1)} {
		got, err := DecodeRolloutManifest(strings.NewReader(raw))
		if (err == nil) != (raw == string(b)) {
			t.Fatalf("decode %q: %+v %v", raw, got, err)
		}
	}
}
func TestRolloutHTTP(t *testing.T) {
	cases := []string{"armed", "running", "paused", "completed", "rolled_back", "absent", "missing", "denied", "unauthorized", "redirect", "null", "duplicate", "trailing", "malformed", "oversized", "id", "workspaceId", "environmentId", "epoch", "registrationVersion", "updatedAt", "bad state", "bad action", "bad step", "missing step", "armed step", "completed step", "negative ratio", "ratio over", "runtime", "window", "overflow", "step cap", "history cap", "history step", "history time", "missing auto advance", "null manual override", "plan after snapshot", "step before plan", "cancel", "scope"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			response := rolloutResponseFixture()
			plan := response["releasePlan"].(map[string]any)
			status := 200
			raw := ""
			good := false
			want := ErrResponse
			switch name {
			case "armed", "running", "paused", "completed", "rolled_back":
				good = true
				plan["state"] = name
				if name != "armed" {
					plan["stepStartedAt"] = "2026-01-01T00:00:00Z"
				}
			case "absent":
				good = true
				delete(response, "releasePlan")
			case "missing":
				good = true
				status = 404
			case "denied":
				status = 403
				want = ErrAuthorization
			case "unauthorized":
				status = 401
				want = ErrAuthorization
			case "redirect":
				status = 302
			case "null":
				raw = "null"
			case "duplicate":
				raw = `{"id":"exp1","id":"exp1"}`
			case "trailing":
				raw = `{} {}`
			case "malformed":
				raw = "{"
			case "oversized":
				raw = strings.Repeat(" ", 2<<20+1)
			case "id", "workspaceId", "environmentId", "registrationVersion":
				response[name] = "wrong"
				want = ErrMismatch
			case "epoch":
				response[name] = 2
				want = ErrMismatch
			case "updatedAt":
				delete(response, name)
			case "bad state":
				plan["state"] = "secret"
			case "bad action":
				plan["history"].([]any)[0].(map[string]any)["action"] = "secret"
			case "bad step":
				plan["currentStep"] = -1
			case "missing step":
				delete(plan, "currentStep")
			case "armed step", "completed step":
				plan["steps"] = append(plan["steps"].([]any), map[string]any{"ratio": 100, "minRuntime": 1, "autoAdvance": false})
				plan["steps"].([]any)[0].(map[string]any)["ratio"] = 10
				if name == "armed step" {
					plan["currentStep"] = 1
				} else {
					plan["state"] = "completed"
					plan["stepStartedAt"] = "2026-01-01T00:00:00Z"
				}
			case "negative ratio":
				plan["steps"].([]any)[0].(map[string]any)["ratio"] = -1
			case "ratio over":
				plan["steps"].([]any)[0].(map[string]any)["ratio"] = 101
			case "runtime":
				plan["steps"].([]any)[0].(map[string]any)["minRuntime"] = 0
			case "window":
				plan["steps"].([]any)[0].(map[string]any)["observationWindow"] = -1
			case "overflow":
				s := plan["steps"].([]any)[0].(map[string]any)
				s["minRuntime"] = int64(9223372036854775807)
				s["observationWindow"] = 1
			case "step cap":
				plan["steps"] = make([]any, MaxRolloutSteps+1)
			case "history cap":
				plan["history"] = make([]any, MaxRolloutHistory+1)
			case "history step":
				plan["history"].([]any)[0].(map[string]any)["step"] = 1
			case "history time":
				plan["history"].([]any)[0].(map[string]any)["at"] = "bad"
			case "missing auto advance":
				delete(plan["steps"].([]any)[0].(map[string]any), "autoAdvance")
			case "null manual override":
				plan["manualOverride"] = nil
			case "plan after snapshot":
				plan["updatedAt"] = "2026-01-02T00:00:00Z"
			case "step before plan":
				plan["state"] = "running"
				plan["stepStartedAt"] = "2025-12-31T23:59:59Z"
			case "cancel":
				want = context.Canceled
			case "scope":
				want = ErrMismatch
			}

			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.RequestURI() != "/api/v1/admin/experiments/exp1" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(status)
				if raw != "" {
					_, _ = w.Write([]byte(raw))
				} else {
					_ = json.MarshalWrite(w, response)
				}
			}))
			defer srv.Close()
			r, err := NewReader(Config{BaseURL: srv.URL, WorkspaceID: "ws1", EnvironmentID: "env1", BearerToken: "secret"})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := rolloutManifestFixture()
			if name == "cancel" {
				cancel()
			}
			if name == "scope" {
				m.EnvironmentID = "other"
			}
			got, err := r.Rollout(ctx, m)
			if good {
				if err != nil || got == nil || got.PublicationAuthorized {
					t.Fatalf("%+v %v", got, err)
				}
				if (got.Availability == "experiment_missing") != (name == "missing") || got.PlanPresent != (name != "missing" && name != "absent") {
					t.Fatalf("availability: %+v", got)
				}
				b, _ := json.Marshal(got)
				if strings.Contains(string(b), "secret") {
					t.Fatal("untrusted prose exported")
				}
			} else if got != nil || !errors.Is(err, want) {
				t.Fatalf("got %+v %v want %v", got, err, want)
			}
			expected := int32(1)
			if name == "cancel" || name == "scope" {
				expected = 0
			}
			if calls.Load() != expected {
				t.Fatalf("calls %d", calls.Load())
			}
		})
	}
}
func TestRolloutCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	r, _ := NewReader(Config{BaseURL: srv.URL, WorkspaceID: "ws1", EnvironmentID: "env1", BearerToken: "secret"})
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got, err := r.Rollout(ctx, rolloutManifestFixture())
	if got != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRolloutSupportedActionsAndBounds(t *testing.T) {
	response := rolloutResponseFixture()
	b, _ := json.Marshal(response["releasePlan"])
	var p RolloutPlan
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"plan.create", "plan.start", "step.advance", "plan.complete", "plan.rollback", "plan.pause", "manual.pause", "manual.resume", "manual.takeover_on", "manual.takeover_off", "manual.complete", "manual.advance", "repair_publish"} {
		p.History[0].Action = action
		if !validRolloutPlan(&p) {
			t.Fatalf("supported action %s rejected", action)
		}
	}
	p.History = make([]RolloutEvent, MaxRolloutHistory)
	step := 0
	for i := range p.History {
		p.History[i] = RolloutEvent{At: p.CreatedAt, Action: "plan.create", Step: &step}
	}
	p.Steps = make([]RolloutStep, MaxRolloutSteps)
	for i := range p.Steps {
		p.Steps[i] = RolloutStep{Ratio: float64(i+1) * 100 / MaxRolloutSteps, MinRuntime: 1}
	}
	if !validRolloutPlan(&p) {
		t.Fatal("maximum bounded plan rejected")
	}
	p.State = "completed"
	step = MaxRolloutSteps - 1
	p.CurrentStep = &step
	p.StepStartedAt = &p.CreatedAt
	if !validRolloutPlan(&p) {
		t.Fatal("completed final step rejected")
	}
	p.Steps[1].Ratio = p.Steps[0].Ratio
	if validRolloutPlan(&p) {
		t.Fatal("non-increasing ratios accepted")
	}
	for _, raw := range []string{`{"ratio":1,"minRuntime":1}`, `{"ratio":1,"minRuntime":1,"autoAdvance":null}`, `{"ratio":1,"minRuntime":1,"autoAdvance":false,"observationWindow":null}`, `{"ratio":1,"ratio":2,"minRuntime":1,"autoAdvance":false}`} {
		var s RolloutStep
		if json.Unmarshal([]byte(raw), &s) == nil {
			t.Fatalf("malformed configuration accepted: %s", raw)
		}
	}
}
