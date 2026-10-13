package evaluation_test

import (
	"fmt"
	"testing"

	"github.com/godeps/grayling/pkg/bucketing"
	"github.com/godeps/grayling/pkg/evaluation"
	"github.com/godeps/grayling/pkg/model"
)

// buildBenchSnapshot creates n flags, each with 3 targeting rules (constraint
// + segment) ahead of a percentage default — a realistic mid-size document.
func buildBenchSnapshot(n int) *model.Snapshot {
	segments := map[string]*model.Segment{
		"beta": {
			SchemaVersion: model.SchemaVersion, Key: "beta", Version: "1",
			Constraints: []model.Constraint{{Field: "user_id", Op: model.OpRegex, Value: `^beta-`}},
		},
	}
	flags := map[string]*model.Flag{}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("flag-%d", i)
		flags[key] = &model.Flag{
			SchemaVersion: model.SchemaVersion, Key: key, Type: model.FlagString,
			Fields:    []string{"user_id", "tenant", "version"},
			Bucketing: &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: key},
			Variations: map[string]any{
				"control": "c", "candidate": "t", "extra": "e",
			},
			DefaultRule: &model.Rule{Percentage: map[string]float64{
				"control": 80, "candidate": 15, "extra": 5,
			}},
			Targeting: []model.Rule{
				{Name: "tenant", Constraints: []model.Constraint{
					{Field: "tenant", Op: model.OpIn, Values: []any{"acme", "globex"}},
				}, Variation: "extra"},
				{Name: "old", Constraints: []model.Constraint{
					{Field: "version", Op: model.OpSemverLt, Value: "2.0.0"},
				}, Variation: "control"},
				{Name: "beta", Segments: []string{"beta"}, Variation: "candidate"},
			},
		}
	}
	return &model.Snapshot{
		SchemaVersion: model.SchemaVersion,
		Workspace:     "bench-ws", Environment: "bench-env",
		ManifestRevision: 1,
		Flags:            flags, Segments: segments,
	}
}

// BenchmarkLocalEvaluation measures the hot path (§13 P1 performance target:
// local evaluation P99 < 1ms — this benchmark provides the mean/op evidence
// on the development machine; formal P99 requires the load harness described
// in docs/实施状态.md).
func BenchmarkLocalEvaluation(b *testing.B) {
	ev, err := evaluation.NewEvaluator(buildBenchSnapshot(500))
	if err != nil {
		b.Fatalf("NewEvaluator: %v", err)
	}
	ctx := model.EvaluationContext{
		TargetingKey: "user-1234",
		Attributes: map[string]any{
			"user_id": "user-1234", "tenant": "initech", "version": "2.3.4",
		},
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("flag-%d", i%500)
		res := ev.Evaluate(key, ctx)
		if res.Failed {
			b.Fatalf("eval failed: %+v", res)
		}
	}
}

// BenchmarkSnapshotBuild measures evaluator construction (the cost paid once
// per manifest revision, not per evaluation).
func BenchmarkSnapshotBuild(b *testing.B) {
	snap := buildBenchSnapshot(500)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := evaluation.NewEvaluator(snap); err != nil {
			b.Fatalf("NewEvaluator: %v", err)
		}
	}
}
