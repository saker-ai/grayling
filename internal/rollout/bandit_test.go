package rollout_test

import (
	"slices"
	"testing"
	"time"

	"github.com/godeps/golab/internal/experiment"
	"github.com/godeps/golab/internal/platform"
)

func TestBanditTickPublishesFeedbackAndRemoteRouting(t *testing.T) {
	h := newHarness(t)
	id := h.runningExperiment(t, "bandit-live", func(req map[string]any) {
		req["allocationMode"] = "bandit"
		req["bandit"] = map[string]any{"policyVersion": "p1", "strategy": "epsilon-greedy",
			"priorAlpha": 1, "priorBeta": 1, "epsilon": 0.2,
			"feedbackDelay": int64(11 * 24 * time.Hour)}
	})
	if sum := h.api.Rollout.Tick(); !slices.Contains(sum.BanditFrozen, id) {
		t.Fatalf("initial policy should freeze until feedback: %+v", sum)
	}
	now := time.Now().UTC()
	base := platform.Event{WorkspaceID: h.ws.ID, EnvironmentID: h.env.ID,
		ExperimentID: id, Epoch: 1, FlagKey: "bandit-live", PolicyVersion: "p1",
		AllocationProbability: 0.5, ClientTime: now}
	control, treatment, conversion := base, base, base
	control.EventID, control.Type, control.Variant, control.UnitHash, control.Source =
		"b-a", platform.EventExposure, "control", h.store.HashUnit(h.ws.ID, "first-a"), "bandit-fallback"
	treatment.EventID, treatment.Type, treatment.Variant, treatment.UnitHash, treatment.Source =
		"b-b", platform.EventExposure, "treatment", h.store.HashUnit(h.ws.ID, "first-b"), "bandit-fallback"
	conversion.EventID, conversion.Type, conversion.Variant, conversion.UnitHash, conversion.MetricKey, conversion.Value =
		"b-c", platform.EventConversion, "treatment", treatment.UnitHash, "conversion", 1
	if got, err := h.store.IngestEvents([]*platform.Event{&control, &treatment, &conversion}); err != nil || got.Accepted != 3 {
		t.Fatalf("ingest: %+v %v", got, err)
	}
	h.advance(12 * 24 * time.Hour)
	sum := h.api.Rollout.Tick()
	if !slices.Contains(sum.BanditUpdated, id) || slices.Contains(sum.BanditFrozen, id) || len(sum.Errors) > 0 {
		t.Fatalf("feedback should activate policy: %+v", sum)
	}
	current := h.store.CurrentFlagContents(h.ws.ID, h.env.ID)["bandit-live"]
	if current == nil || current.Content.Experiment.Bandit.Frozen || current.Content.Experiment.Bandit.StateVersion == 0 {
		t.Fatalf("no active published policy: %+v", current)
	}
	var response map[string]any
	if code := h.do("POST", "/api/v1/sdk/eval", h.sdkTok,
		map[string]any{"flagKey": "bandit-live", "context": map[string]any{"attributes": map[string]any{"user_id": "new-user"}}},
		&response); code != 200 {
		t.Fatalf("remote eval: %d %+v", code, response)
	}
	result := response["result"].(map[string]any)
	exposure := result["experiment"].(map[string]any)
	if exposure["source"] != "bandit" || exposure["policyVersion"] != "p1" ||
		exposure["allocationProbability"].(float64) <= 0 {
		t.Fatalf("remote routing ignored published policy: %+v", response)
	}
	rep, err := experiment.BuildReport(h.store, id, h.fakeNow)
	if err != nil || rep.Bandit == nil || rep.Primary != nil || rep.Decision.Verdict == "ship" {
		t.Fatalf("wrong report inference: %+v %v", rep, err)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+id+"/transition", h.owner,
		map[string]any{"to": "stopped", "reason": "rollback"}, nil); code != 200 {
		t.Fatalf("rollback: %d", code)
	}
	response = nil
	if code := h.do("POST", "/api/v1/sdk/eval", h.sdkTok,
		map[string]any{"flagKey": "bandit-live", "context": map[string]any{"attributes": map[string]any{"user_id": "after-stop"}}},
		&response); code != 200 {
		t.Fatalf("eval after stop: %d", code)
	}
	if response["result"].(map[string]any)["experiment"] != nil {
		t.Fatalf("stopped experiment still allocates: %+v", response)
	}
}
