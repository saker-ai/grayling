package experiment

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

func TestBanditReplayFeedbackAndInferenceIsolation(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	start := now.Add(-10 * 24 * time.Hour)
	exp := &platform.Experiment{ID: "e", Epoch: 1, FlagKey: "f", CreatedAt: start, StartedAt: &start,
		State: model.ExperimentRunning, Variants: []string{"a", "b"}, Proportions: map[string]float64{"a": 50, "b": 50},
		PrimaryMetric:  platform.MetricSpec{Key: "reward", Type: "proportion", Direction: "increase"},
		AnalysisWindow: 24 * time.Hour, Bandit: &platform.BanditSpec{PolicyVersion: "p1", Strategy: "epsilon-greedy",
			PriorAlpha: 1, PriorBeta: 1, Epsilon: 0.2, FeedbackDelay: 96 * time.Hour}}
	events := []*platform.Event{
		{EventID: "b", Type: platform.EventExposure, Variant: "b", UnitHash: "u2", Source: "bandit",
			PolicyVersion: "p1", StateVersion: 2, AllocationProbability: 0.5, ClientTime: now.Add(-8 * 24 * time.Hour)},
		{EventID: "a", Type: platform.EventExposure, Variant: "a", UnitHash: "u1", Source: "bandit-fallback",
			PolicyVersion: "p1", AllocationProbability: 0.5, ClientTime: now.Add(-8 * 24 * time.Hour)},
		{EventID: "c", Type: platform.EventConversion, Variant: "a", UnitHash: "u1", MetricKey: "reward", Value: 1,
			ClientTime: now.Add(-8*24*time.Hour + time.Hour)},
		{EventID: "late", Type: platform.EventExposure, Variant: "b", UnitHash: "u3", Source: "bandit",
			PolicyVersion: "p1", StateVersion: 2, AllocationProbability: 0.5, ClientTime: now.Add(-time.Hour)},
	}
	rep := replayBanditReport(exp, events, now)
	if rep.Primary != nil || rep.Sequential != nil || rep.Bayesian != nil || rep.Decision.Verdict == "ship" {
		t.Fatalf("bandit must not produce fixed-sample inference: %+v", rep)
	}
	if got := rep.Bandit; got.Trials[0] != 1 || got.Trials[1] != 1 || got.Successes[0] != 1 ||
		got.Successes[1] != 0 || got.Pending != 1 || got.MissingFeedback != 1 || got.FallbackAssignments != 1 || got.Frozen {
		t.Fatalf("feedback accounting: %+v", got)
	}
	if rep.Bandit.PosteriorMean[0] != 2.0/3 || rep.Bandit.PosteriorMean[1] != 1.0/3 {
		t.Fatalf("posterior: %+v", rep.Bandit.PosteriorMean)
	}
	state := PublishedBanditState(exp, rep.Bandit)
	if state.StateVersion == 0 || state.PolicyVersion != "p1" || state.Watermark != now.Add(-96*time.Hour) {
		t.Fatalf("published state: %+v", state)
	}
	slices.Reverse(events)
	replayed := replayBanditReport(exp, events, now)
	if replayed.Bandit.StateVersion != state.StateVersion || !slices.Equal(replayed.Bandit.Trials, state.Trials) {
		t.Fatalf("event order changed policy state: %+v", replayed.Bandit)
	}
}

func TestBanditReplayFreezesOnSRMAndArmChange(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	start := now.Add(-10 * 24 * time.Hour)
	exp := &platform.Experiment{ID: "e", Epoch: 1, CreatedAt: start, StartedAt: &start,
		Variants: []string{"a", "b"}, Proportions: map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: platform.MetricSpec{Key: "reward", Type: "proportion"}, AnalysisWindow: time.Hour,
		Bandit: &platform.BanditSpec{PolicyVersion: "p1", Strategy: "thompson", PriorAlpha: 1, PriorBeta: 1,
			FeedbackDelay: 73 * time.Hour}}
	events := make([]*platform.Event, 0, 102)
	for i := range 100 {
		events = append(events, &platform.Event{EventID: fmt.Sprint(i), Type: platform.EventExposure,
			Variant: "b", UnitHash: fmt.Sprint(i), Source: "bandit", PolicyVersion: "p1",
			StateVersion: 1, AllocationProbability: 0.5, ClientTime: start.Add(time.Duration(i) * time.Second)})
	}
	rep := replayBanditReport(exp, events, now)
	if !rep.Bandit.SRMAlarm || !rep.Bandit.Frozen || rep.Healthy {
		t.Fatalf("biased allocation must freeze: %+v", rep.Bandit)
	}
	events = append(events, &platform.Event{EventID: "switch", Type: platform.EventExposure,
		Variant: "a", UnitHash: "0", Source: "bandit", PolicyVersion: "p1",
		StateVersion: 2, AllocationProbability: 0.5, ClientTime: start.Add(time.Minute)})
	rep = replayBanditReport(exp, events, now)
	if len(rep.HealthIssues) < 2 || !rep.Bandit.Frozen {
		t.Fatalf("arm change not detected: %+v", rep)
	}
}

func TestBanditOnRenderFreezesOnDeliveryGap(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	start := now.Add(-10 * 24 * time.Hour)
	exp := &platform.Experiment{ID: "e", Epoch: 1, FlagKey: "f", CreatedAt: start, StartedAt: &start,
		ExposureMode: "onRender", AllocationMode: "bandit", State: model.ExperimentRunning,
		Variants: []string{"a", "b"}, Proportions: map[string]float64{"a": 50, "b": 50},
		PrimaryMetric:  platform.MetricSpec{Key: "reward", Type: "proportion"},
		AnalysisWindow: 24 * time.Hour, Bandit: &platform.BanditSpec{PolicyVersion: "p1",
			Strategy: "epsilon-greedy", PriorAlpha: 1, PriorBeta: 1, Epsilon: 0.2,
			FeedbackDelay: 96 * time.Hour}}
	// A bandit policy runs across many feedback windows. Diagnostics must
	// cover its full runtime, not stop at the first analysis window.
	assignedAt := start.Add(2 * 24 * time.Hour)
	events := []*platform.Event{
		{EventID: "asg-seen", Type: platform.EventAssignment, Variant: "a", UnitHash: "seen",
			FlagKey: "f", FlagRevision: 1, Source: "bandit", PolicyVersion: "p1",
			StateVersion: 1, AllocationProbability: 0.5, ClientTime: assignedAt},
		{EventID: "exp-seen", Type: platform.EventExposure, Variant: "a", UnitHash: "seen",
			FlagKey: "f", FlagRevision: 1, Source: "bandit", PolicyVersion: "p1",
			StateVersion: 1, AllocationProbability: 0.5, ClientTime: assignedAt.Add(time.Second)},
		{EventID: "asg-unseen", Type: platform.EventAssignment, Variant: "b", UnitHash: "unseen",
			FlagKey: "f", FlagRevision: 1, Source: "bandit", PolicyVersion: "p1",
			StateVersion: 1, AllocationProbability: 0.5, ClientTime: assignedAt},
	}
	rep := replayBanditReport(exp, events, now)
	if rep.Diagnostics == nil || rep.Diagnostics.PerVariant[1].Unexposed != 1 {
		t.Fatalf("bandit delivery gap missing: %+v", rep.Diagnostics)
	}
	if rep.Healthy || !rep.Bandit.Frozen || rep.Bandit.SRMAlarm {
		t.Fatalf("delivery gap must freeze without fixed split SRM: %+v", rep.Bandit)
	}
	if rep.Primary != nil || rep.Decision.Verdict != "hold" {
		t.Fatalf("bandit must retain independent inference semantics: %+v", rep)
	}
}

func TestBanditReplayFreezesOnGuardrail(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	start := now.Add(-10 * 24 * time.Hour)
	exp := &platform.Experiment{ID: "e", Epoch: 1, CreatedAt: start, StartedAt: &start,
		Variants: []string{"a", "b"}, Proportions: map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: platform.MetricSpec{Key: "reward", Type: "proportion"}, AnalysisWindow: time.Hour,
		Bandit: &platform.BanditSpec{PolicyVersion: "p1", Strategy: "epsilon-greedy", PriorAlpha: 1,
			PriorBeta: 1, Epsilon: 0.2, FeedbackDelay: 73 * time.Hour},
		Guardrails: []platform.GuardrailSpec{{MetricKey: "errors", Type: "proportion", Direction: "decrease",
			MaxRelativeRegression: 0.1, MinSample: 5}}}
	events := make([]*platform.Event, 0, 40)
	for arm, variant := range exp.Variants {
		for i := range 10 {
			unit := fmt.Sprintf("%s-%d", variant, i)
			at := start.Add(time.Duration(i) * time.Second)
			events = append(events, &platform.Event{EventID: "exp-" + unit, Type: platform.EventExposure,
				Variant: variant, UnitHash: unit, Source: "bandit", PolicyVersion: "p1",
				StateVersion: 1, AllocationProbability: 0.5, ClientTime: at})
			if (arm == 0 && i == 0) || (arm == 1 && i < 5) {
				events = append(events, &platform.Event{EventID: "conv-" + unit, Type: platform.EventConversion,
					Variant: variant, UnitHash: unit, MetricKey: "errors", Value: 1, ClientTime: at.Add(time.Minute)})
			}
		}
	}
	for _, ev := range events {
		ev.ExperimentID, ev.Epoch = exp.ID, exp.Epoch
	}
	rep := replayBanditReport(exp, events, now)
	if rep.Healthy || !rep.Bandit.Frozen || len(rep.Guardrails) != 1 || !rep.Guardrails[0].Breached {
		t.Fatalf("guardrail did not freeze policy: %+v", rep)
	}
}
