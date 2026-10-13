package evaluation_test

import (
	"testing"
	"time"

	"github.com/saker-ai/grayling/pkg/evaluation"
	"github.com/saker-ai/grayling/pkg/model"
)

func TestBanditReplicaAndOfflineFallback(t *testing.T) {
	f := expFlag(t, map[string]float64{"control": 50, "candidate": 50}, func(f *model.Flag) {
		f.Experiment.Bandit = &model.BanditBinding{PolicyVersion: "p1", Strategy: "epsilon-greedy",
			Variants: []string{"control", "candidate"}, PriorAlpha: 1, PriorBeta: 1, Epsilon: 0.2,
			StateVersion: 7, Trials: []int64{100, 100}, Successes: []int64{10, 70}}
	})
	s := snap(map[string]*model.Flag{"exp-flag": f}, nil)
	s.GeneratedAt = time.Now().UTC()
	a := mustEvaluator(t, s)
	b := mustEvaluator(t, s)
	a.EnableBandit()
	b.EnableBandit()
	var adaptive, fixed int
	for i := range 1000 {
		unit := "unit-" + string(rune(i+1000))
		context := ctx(unit, map[string]any{"user_id": unit})
		ra, rb := a.Evaluate("exp-flag", context), b.Evaluate("exp-flag", context)
		if ra.VariationType != rb.VariationType || ra.Experiment == nil || rb.Experiment == nil ||
			ra.Experiment.AllocationProbability != rb.Experiment.AllocationProbability ||
			ra.Experiment.StateVersion != 7 || ra.Experiment.Source != "bandit" {
			t.Fatalf("replicas disagree: %+v / %+v", ra, rb)
		}
		if ra.VariationType == "candidate" {
			adaptive++
		}
		local := mustEvaluator(t, s).Evaluate("exp-flag", context)
		if local.Experiment == nil || local.Experiment.Source != "split" ||
			local.Experiment.AllocationProbability != 0.5 {
			t.Fatalf("local fallback: %+v", local)
		}
		if local.VariationType == "candidate" {
			fixed++
		}
	}
	if adaptive < 800 || fixed < 400 || fixed > 600 {
		t.Fatalf("adaptive=%d fixed=%d", adaptive, fixed)
	}
	stale := *s
	stale.GeneratedAt = time.Now().Add(-time.Hour)
	offline, err := evaluation.NewEvaluator(&stale)
	if err != nil {
		t.Fatal(err)
	}
	offline.EnableBandit()
	res := offline.Evaluate("exp-flag", ctx("unit", map[string]any{"user_id": "unit"}))
	if res.Experiment == nil || res.Experiment.Source != "split" || res.Experiment.AllocationProbability != 0.5 {
		t.Fatalf("stale snapshot did not fall back: %+v", res)
	}
}

func BenchmarkBanditEvaluation(b *testing.B) {
	f := &model.Flag{SchemaVersion: model.SchemaVersion, Key: "bandit", Type: model.FlagString,
		Fields: []string{"user_id"}, Bucketing: &model.Bucketing{Algorithm: "golab-fnv1a-10000-v1", Unit: "user_id"},
		Variations: map[string]any{"a": "a", "b": "b"}, DefaultRule: &model.Rule{Variation: "a"},
		Targeting: []model.Rule{{Percentage: map[string]float64{"a": 50, "b": 50}}},
		Experiment: &model.ExperimentBinding{ID: "e", Epoch: 1, State: model.ExperimentRunning,
			Bandit: &model.BanditBinding{PolicyVersion: "p1", Strategy: "epsilon-greedy",
				Variants: []string{"a", "b"}, PriorAlpha: 1, PriorBeta: 1, Epsilon: 0.2,
				StateVersion: 2, Trials: []int64{100, 100}, Successes: []int64{10, 50}}}}
	s := snap(map[string]*model.Flag{"bandit": f}, nil)
	s.GeneratedAt = time.Now().UTC()
	ev, err := evaluation.NewEvaluator(s)
	if err != nil {
		b.Fatal(err)
	}
	ev.EnableBandit()
	context := ctx("u", map[string]any{"user_id": "u"})
	b.ReportAllocs()
	for b.Loop() {
		if res := ev.Evaluate("bandit", context); res.Experiment == nil {
			b.Fatal("missing allocation")
		}
	}
}
