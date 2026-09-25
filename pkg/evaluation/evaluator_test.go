package evaluation_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/godeps/golab/pkg/bucketing"
	"github.com/godeps/golab/pkg/evaluation"
	"github.com/godeps/golab/pkg/model"
)

func snap(flags map[string]*model.Flag, segments map[string]*model.Segment) *model.Snapshot {
	return &model.Snapshot{
		SchemaVersion: model.SchemaVersion,
		Workspace:     "ws", Environment: "prod",
		ManifestRevision: 1,
		Flags:            flags, Segments: segments,
	}
}

func boolFlag(key string) *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           key,
		Type:          model.FlagBool,
		Variations:    map[string]any{"on": true, "off": false},
		DefaultRule:   &model.Rule{Variation: "off"},
	}
}

func mustEvaluator(t *testing.T, s *model.Snapshot) *evaluation.Evaluator {
	t.Helper()
	e, err := evaluation.NewEvaluator(s)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return e
}

func ctx(key string, attrs map[string]any) model.EvaluationContext {
	return model.EvaluationContext{TargetingKey: key, Attributes: attrs}
}

func TestFlagNotFound(t *testing.T) {
	e := mustEvaluator(t, snap(map[string]*model.Flag{"a": boolFlag("a")}, nil))
	res := e.Evaluate("missing", ctx("u1", nil))
	if !res.Failed || res.ErrorCode != model.ErrFlagNotFound || res.Reason != model.ReasonError {
		t.Fatalf("got %+v, want FLAG_NOT_FOUND error", res)
	}
	v, res2 := e.BoolVariation("missing", ctx("u1", nil), true)
	if v != true || !res2.Failed {
		t.Fatalf("BoolVariation must return caller default on missing flag: %v %+v", v, res2)
	}
}

func TestDefaultAndKillSwitch(t *testing.T) {
	f := boolFlag("killme")
	e := mustEvaluator(t, snap(map[string]*model.Flag{"killme": f}, nil))

	v, res := e.BoolVariation("killme", ctx("u1", nil), true)
	if v != false || res.Reason != model.ReasonDefault || res.VariationType != "off" {
		t.Fatalf("default: got %v %+v", v, res)
	}

	f.Disable = true
	e = mustEvaluator(t, snap(map[string]*model.Flag{"killme": f}, nil))
	v, res = e.BoolVariation("killme", ctx("u1", nil), true)
	if v != false || res.Reason != model.ReasonDisabled {
		t.Fatalf("disabled: got %v %+v", v, res)
	}
	if res.TrackEvents || res.Experiment != nil {
		t.Fatal("disabled flags must not emit exposures")
	}
}

func TestTargetingOrderAndConstraints(t *testing.T) {
	f := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           "router", Type: model.FlagString,
		Fields:      []string{"tenant", "country", "version", "ip", "signup", "count"},
		Variations:  map[string]any{"internal": "internal", "eu": "eu", "old": "old", "std": "std"},
		DefaultRule: &model.Rule{Variation: "std"},
		Targeting: []model.Rule{
			{Name: "first", Constraints: []model.Constraint{{Field: "tenant", Op: model.OpEq, Value: "acme"}}, Variation: "internal"},
			{Name: "second", Constraints: []model.Constraint{{Field: "country", Op: model.OpIn, Values: []any{"de", "fr"}}}, Variation: "eu"},
			{Name: "third", Constraints: []model.Constraint{{Field: "version", Op: model.OpSemverLt, Value: "2.0.0"}}, Variation: "old"},
		},
	}
	e := mustEvaluator(t, snap(map[string]*model.Flag{"router": f}, nil))

	cases := []struct {
		attrs map[string]any
		want  string
		why   string
	}{
		{map[string]any{"tenant": "acme", "country": "de"}, "internal", "first match wins"},
		{map[string]any{"tenant": "other", "country": "fr"}, "eu", "set in"},
		{map[string]any{"version": "v1.9.3"}, "old", "semver lt with v prefix"},
		{map[string]any{"version": "2.0.1"}, "std", "semver no match → default"},
		{map[string]any{"tenant": "acme"}, "internal", "eq"},
		{nil, "std", "empty context → default"},
	}
	for _, c := range cases {
		v, res := e.StringVariation("router", ctx("u1", c.attrs), "?")
		if v != c.want {
			t.Errorf("%s: got %q want %q (%+v)", c.why, v, c.want, res)
		}
		if c.want != "std" && res.Reason != model.ReasonTargetingMatch {
			t.Errorf("%s: reason = %s, want TARGETING_MATCH", c.why, res.Reason)
		}
		if c.want == "std" && res.Reason != model.ReasonDefault {
			t.Errorf("%s: reason = %s, want DEFAULT", c.why, res.Reason)
		}
	}
}

func TestConstraintOperators(t *testing.T) {
	mk := func(c model.Constraint) *model.Flag {
		return &model.Flag{
			SchemaVersion: model.SchemaVersion,
			Key:           "f", Type: model.FlagString,
			Fields:      []string{"x"},
			Variations:  map[string]any{"yes": "y", "no": "n"},
			DefaultRule: &model.Rule{Variation: "no"},
			Targeting:   []model.Rule{{Constraints: []model.Constraint{c}, Variation: "yes"}},
		}
	}
	cases := []struct {
		name  string
		c     model.Constraint
		value any
		want  bool
	}{
		{"eq string", model.Constraint{Field: "x", Op: model.OpEq, Value: "a"}, "a", true},
		{"eq numeric cross-type", model.Constraint{Field: "x", Op: model.OpEq, Value: float64(3)}, 3, true},
		{"eq string vs number never", model.Constraint{Field: "x", Op: model.OpEq, Value: "1"}, 1, false},
		{"ne present", model.Constraint{Field: "x", Op: model.OpNe, Value: "a"}, "b", true},
		{"ne missing field never matches", model.Constraint{Field: "x", Op: model.OpNe, Value: "a"}, nil, false},
		{"not_in", model.Constraint{Field: "x", Op: model.OpNotIn, Values: []any{"a", "b"}}, "c", true},
		{"gt", model.Constraint{Field: "x", Op: model.OpGt, Value: 10}, 11, true},
		{"gte boundary", model.Constraint{Field: "x", Op: model.OpGte, Value: 10}, 10, true},
		{"lt wrong type", model.Constraint{Field: "x", Op: model.OpLt, Value: 10}, "5", false},
		{"time_before rfc3339", model.Constraint{Field: "x", Op: model.OpTimeBefore, Value: "2026-01-01T00:00:00Z"}, "2025-06-01T00:00:00Z", true},
		{"time_after epoch", model.Constraint{Field: "x", Op: model.OpTimeAfter, Value: float64(1700000000)}, 1800000000, true},
		{"regex", model.Constraint{Field: "x", Op: model.OpRegex, Value: `^acm.*`}, "acme-corp", true},
		{"regex no partial anchor", model.Constraint{Field: "x", Op: model.OpRegex, Value: `^acm$`}, "acme", false},
		{"cidr v4", model.Constraint{Field: "x", Op: model.OpInCIDR, Value: "10.0.0.0/8"}, "10.1.2.3", true},
		{"cidr v4 outside", model.Constraint{Field: "x", Op: model.OpInCIDR, Value: "10.0.0.0/8"}, "11.1.2.3", false},
		{"cidr v4-mapped", model.Constraint{Field: "x", Op: model.OpInCIDR, Value: "10.0.0.0/8"}, "::ffff:10.1.2.3", true},
		{"semver_gte prerelease", model.Constraint{Field: "x", Op: model.OpSemverGte, Value: "1.2.3"}, "1.2.3-alpha", false},
		{"semver_eq", model.Constraint{Field: "x", Op: model.OpSemverEq, Value: "1.2.3"}, "v1.2.3", true},
	}
	for _, c := range cases {
		f := mk(c.c)
		e, err := evaluation.NewEvaluator(snap(map[string]*model.Flag{"f": f}, nil))
		if err != nil {
			t.Fatalf("%s: NewEvaluator: %v", c.name, err)
		}
		attrs := map[string]any{}
		if c.value != nil {
			attrs["x"] = c.value
		}
		v, _ := e.StringVariation("f", ctx("u", attrs), "?")
		got := v == "y"
		if got != c.want {
			t.Errorf("%s: matched=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestRegexInputLengthBound(t *testing.T) {
	f := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           "f", Type: model.FlagString, Fields: []string{"x"},
		Variations:  map[string]any{"yes": "y", "no": "n"},
		DefaultRule: &model.Rule{Variation: "no"},
		Targeting: []model.Rule{{Constraints: []model.Constraint{
			{Field: "x", Op: model.OpRegex, Value: "^u.*"},
		}, Variation: "yes"}},
	}
	e := mustEvaluator(t, snap(map[string]*model.Flag{"f": f}, nil))
	long := "u" + string(make([]byte, evaluation.MaxRegexInputBytes))
	v, _ := e.StringVariation("f", ctx("u", map[string]any{"x": long}), "?")
	if v != "n" {
		t.Fatalf("oversized regex input must not match, got %q", v)
	}
}

func TestSegmentsNestedAndCycles(t *testing.T) {
	// Composition semantics (normative): a rule's segment list is OR; inside
	// one segment, constraints AND together and nested segments OR among
	// themselves, ANDed with the own constraints.
	segments := map[string]*model.Segment{
		"beta": {
			SchemaVersion: model.SchemaVersion, Key: "beta", Version: "3",
			Constraints: []model.Constraint{{Field: "beta_opt_in", Op: model.OpEq, Value: true}},
		},
		"staff": {
			SchemaVersion: model.SchemaVersion, Key: "staff", Version: "1",
			Constraints: []model.Constraint{{Field: "tenant", Op: model.OpIn, Values: []any{"acme"}}},
		},
		"eu-beta": { // region=eu AND (beta OR staff)
			SchemaVersion: model.SchemaVersion, Key: "eu-beta", Version: "1",
			Constraints: []model.Constraint{{Field: "region", Op: model.OpEq, Value: "eu"}},
			Segments:    []string{"beta", "staff"},
		},
	}
	f := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           "f", Type: model.FlagString, Fields: []string{"beta_opt_in", "tenant", "region"},
		Variations:  map[string]any{"new": "new", "old": "old"},
		DefaultRule: &model.Rule{Variation: "old"},
		Targeting: []model.Rule{
			{Name: "eu-beta-first", Segments: []string{"eu-beta"}, Variation: "new"},
		},
	}
	e := mustEvaluator(t, snap(map[string]*model.Flag{"f": f}, segments))

	if v, _ := e.StringVariation("f", ctx("u", map[string]any{"beta_opt_in": true, "region": "eu"}), "?"); v != "new" {
		t.Error("eu AND beta should match")
	}
	if v, _ := e.StringVariation("f", ctx("u", map[string]any{"tenant": "acme", "region": "eu"}), "?"); v != "new" {
		t.Error("eu AND staff should match")
	}
	if v, _ := e.StringVariation("f", ctx("u", map[string]any{"beta_opt_in": true, "region": "us"}), "?"); v != "old" {
		t.Error("beta without eu must not match (nested segments AND with own constraints)")
	}
	if v, _ := e.StringVariation("f", ctx("u", map[string]any{"tenant": "other", "region": "eu"}), "?"); v != "old" {
		t.Error("eu without beta/staff must not match")
	}

	// Cycle: rejected at construction, not at evaluation.
	segments["beta"].Segments = []string{"eu-beta"}
	if _, err := evaluation.NewEvaluator(snap(map[string]*model.Flag{"f": f}, segments)); err == nil {
		t.Fatal("segment cycle must be rejected by NewEvaluator")
	}
}

func splitFlag(t *testing.T, key string, pct map[string]float64, extra func(*model.Flag)) *model.Flag {
	t.Helper()
	f := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           key, Type: model.FlagString, Fields: []string{"user_id"},
		Bucketing:   &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: "salt-1"},
		Variations:  map[string]any{"control": "c", "candidate": "t"},
		DefaultRule: &model.Rule{Percentage: pct},
	}
	if extra != nil {
		extra(f)
	}
	return f
}

func TestSplitDeterministicAndDistributed(t *testing.T) {
	f := splitFlag(t, "split", map[string]float64{"control": 90, "candidate": 10}, nil)
	e := mustEvaluator(t, snap(map[string]*model.Flag{"split": f}, nil))

	// Same unit → same variant every time, and across evaluator rebuilds.
	first := map[string]string{}
	for i := 0; i < 500; i++ {
		unit := fmt.Sprintf("user-%d", i)
		v1, r1 := e.StringVariation("split", ctx(unit, map[string]any{"user_id": unit}), "?")
		v2, _ := e.StringVariation("split", ctx(unit, map[string]any{"user_id": unit}), "?")
		if v1 != v2 {
			t.Fatalf("non-deterministic for %s: %s vs %s", unit, v1, v2)
		}
		if r1.Reason != model.ReasonSplit {
			t.Fatalf("reason = %s, want SPLIT", r1.Reason)
		}
		first[unit] = v1
	}
	e2 := mustEvaluator(t, snap(map[string]*model.Flag{"split": f}, nil))
	candidates := 0
	for unit, want := range first {
		got, _ := e2.StringVariation("split", ctx(unit, map[string]any{"user_id": unit}), "?")
		if got != want {
			t.Fatalf("rebuild changed assignment for %s: %s → %s", unit, want, got)
		}
		if got == "t" { // variation "candidate" has value "t"
			candidates++
		}
	}
	share := float64(candidates) / float64(len(first)) * 100
	if math.Abs(share-10) > 2.5 {
		t.Errorf("candidate share %.2f%%, want ~10%%", share)
	}
}
func TestMissingBucketKey(t *testing.T) {
	f := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           "split", Type: model.FlagString, Fields: []string{"user_id"},
		Bucketing:   &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id"},
		Variations:  map[string]any{"control": "c", "candidate": "t"},
		DefaultRule: &model.Rule{Percentage: map[string]float64{"control": 50, "candidate": 50}},
	}
	e := mustEvaluator(t, snap(map[string]*model.Flag{"split": f}, nil))

	// No unit at all.
	v, res := e.StringVariation("split", ctx("", nil), "safe")
	if v != "safe" || !res.Failed || res.ErrorCode != model.ErrMissingBucketKey {
		t.Fatalf("missing unit: got %q %+v", v, res)
	}
	if res.Experiment != nil || res.TrackEvents {
		t.Fatal("missing unit must not emit exposure")
	}
	// Empty string unit.
	_, res = e.StringVariation("split", ctx("", map[string]any{"user_id": ""}), "safe")
	if res.ErrorCode != model.ErrMissingBucketKey {
		t.Fatalf("empty unit: %+v", res)
	}
	// Non-scalar unit (map) must not be stringified into an assignment.
	_, res = e.StringVariation("split", ctx("x", map[string]any{"user_id": map[string]any{"a": 1}}), "safe")
	if res.ErrorCode != model.ErrMissingBucketKey {
		t.Fatalf("map unit: %+v", res)
	}
	// Deterministic targeting rules still work without a unit…
	f2 := splitFlag(t, "t2", nil, nil)
	f2.Targeting = []model.Rule{{Constraints: []model.Constraint{{Field: "user_id", Op: model.OpEq, Value: "x"}}, Variation: "control"}}
	f2.DefaultRule = &model.Rule{Variation: "control"}
	f2.Fields = []string{"user_id"}
	e2 := mustEvaluator(t, snap(map[string]*model.Flag{"t2": f2}, nil))
	v, res = e2.StringVariation("t2", ctx("", nil), "safe")
	if v != "c" || res.Failed {
		t.Fatalf("no-percentage flag must not need a unit: %q %+v", v, res)
	}
}

func TestNumericUnitCanonicalForm(t *testing.T) {
	f := splitFlag(t, "num-unit", map[string]float64{"control": 50, "candidate": 50}, nil)
	e := mustEvaluator(t, snap(map[string]*model.Flag{"num-unit": f}, nil))

	// int, int64 and float64 encodings of the same ID must land in the same
	// bucket (canonical decimal string hashing).
	v1, r1 := e.StringVariation("num-unit", ctx("", map[string]any{"user_id": 42}), "?")
	v2, _ := e.StringVariation("num-unit", ctx("", map[string]any{"user_id": int64(42)}), "?")
	v3, _ := e.StringVariation("num-unit", ctx("", map[string]any{"user_id": float64(42)}), "?")
	if v1 != v2 || v2 != v3 {
		t.Fatalf("numeric encodings diverged: %s %s %s", v1, v2, v3)
	}
	if r1.Bucket < 0 {
		t.Fatalf("bucket not reported: %+v", r1)
	}
}

// expFlag builds an experiment-bound flag: the randomized split lives in an
// always-matching targeting rule, and defaultRule is the deterministic
// fallback required for non-enrolled units (§6).
func expFlag(t *testing.T, pct map[string]float64, mutate func(*model.Flag)) *model.Flag {
	t.Helper()
	f := splitFlag(t, "exp-flag", nil, func(f *model.Flag) {
		f.Targeting = []model.Rule{{Name: "randomized", Percentage: pct}}
		f.DefaultRule = &model.Rule{Variation: "control"}
		f.Experiment = &model.ExperimentBinding{
			ID: "exp-1", Epoch: 1, State: model.ExperimentRunning,
		}
	})
	if mutate != nil {
		mutate(f)
	}
	return f
}

func TestExperimentExposure(t *testing.T) {
	f := expFlag(t, map[string]float64{"control": 50, "candidate": 50}, nil)
	e := mustEvaluator(t, snap(map[string]*model.Flag{"exp-flag": f}, nil))

	unit := "user-7"
	v, res := e.StringVariation("exp-flag", ctx(unit, map[string]any{"user_id": unit}), "?")
	if !res.TrackEvents || res.Experiment == nil {
		t.Fatalf("running experiment must emit exposure: %q %+v", v, res)
	}
	if res.Experiment.ExperimentID != "exp-1" || res.Experiment.Epoch != 1 ||
		res.Experiment.Variant != res.VariationType || res.Experiment.Source != "split" {
		t.Fatalf("bad exposure: %+v", res.Experiment)
	}

	// Repeated evaluation: same exposure, idempotent for the caller's
	// collector to dedup downstream.
	_, res2 := e.StringVariation("exp-flag", ctx(unit, map[string]any{"user_id": unit}), "?")
	if res2.Experiment == nil || res2.Experiment.Variant != res.Experiment.Variant {
		t.Fatal("exposure must be stable per unit")
	}
}

func TestExperimentNotRunning(t *testing.T) {
	for _, state := range []model.ExperimentState{
		model.ExperimentPaused, model.ExperimentStopped, model.ExperimentDraft, model.ExperimentAnalyzed,
	} {
		f := expFlag(t, map[string]float64{"control": 50, "candidate": 50}, func(f *model.Flag) {
			f.Experiment.State = state
		})
		e := mustEvaluator(t, snap(map[string]*model.Flag{"exp-flag": f}, nil))
		v, res := e.StringVariation("exp-flag", ctx("u", map[string]any{"user_id": "u"}), "?")
		if v != "c" || res.Reason != model.ReasonExperimentNotRunning {
			t.Fatalf("%s: got %q %+v, want control/EXPERIMENT_NOT_RUNNING", state, v, res)
		}
		if res.TrackEvents || res.Experiment != nil {
			t.Fatalf("%s: must not emit exposure", state)
		}
	}
}

func TestEligibilityGating(t *testing.T) {
	f := expFlag(t, map[string]float64{"control": 50, "candidate": 50}, func(f *model.Flag) {
		f.Experiment.Eligibility = 30
	})
	e := mustEvaluator(t, snap(map[string]*model.Flag{"exp-flag": f}, nil))

	enrolled, notEnrolled := 0, 0
	const n = 20000
	for i := 0; i < n; i++ {
		unit := fmt.Sprintf("user-%d", i)
		v, res := e.StringVariation("exp-flag", ctx(unit, map[string]any{"user_id": unit}), "?")
		switch res.Reason {
		case model.ReasonNotEnrolled:
			notEnrolled++
			if v != "c" || res.TrackEvents || res.Experiment != nil {
				t.Fatalf("not-enrolled must serve default with no exposure: %q %+v", v, res)
			}
		case model.ReasonSplit:
			enrolled++
			if res.Experiment == nil {
				t.Fatal("enrolled split must carry exposure")
			}
		default:
			t.Fatalf("unexpected reason %s", res.Reason)
		}
	}
	share := float64(enrolled) / n * 100
	if math.Abs(share-30) > 1.5 {
		t.Errorf("enrolled share %.2f%%, want ~30%%", share)
	}
}

func TestLayerMutex(t *testing.T) {
	// Two experiments in one layer with disjoint slots must never enroll
	// the same unit simultaneously.
	mkExp := func(key, id string, slot model.BucketRange) *model.Flag {
		return expFlag(t, map[string]float64{"control": 50, "candidate": 50}, func(f *model.Flag) {
			f.Key = key
			f.Experiment = &model.ExperimentBinding{
				ID: id, Epoch: 1, State: model.ExperimentRunning,
				Layer: "layer-1", LayerSlot: &slot,
			}
		})
	}
	s := snap(map[string]*model.Flag{
		"flag-a": mkExp("flag-a", "exp-a", model.BucketRange{Start: 0, End: 5000}),
		"flag-b": mkExp("flag-b", "exp-b", model.BucketRange{Start: 5000, End: 10000}),
	}, nil)
	e := mustEvaluator(t, s)

	bothEnrolled, aEnrolled, bEnrolled := 0, 0, 0
	for i := 0; i < 5000; i++ {
		unit := fmt.Sprintf("u-%d", i)
		c := ctx(unit, map[string]any{"user_id": unit})
		_, ra := e.StringVariation("flag-a", c, "?")
		_, rb := e.StringVariation("flag-b", c, "?")
		aIn := ra.Experiment != nil
		bIn := rb.Experiment != nil
		if aIn {
			aEnrolled++
		}
		if bIn {
			bEnrolled++
		}
		if aIn && bIn {
			bothEnrolled++
		}
		if !aIn && !bIn {
			t.Fatalf("unit %s enrolled in neither half of a fully-covered layer", unit)
		}
	}
	if bothEnrolled != 0 {
		t.Fatalf("layer mutex violated: %d units in both experiments", bothEnrolled)
	}
	if aEnrolled == 0 || bEnrolled == 0 {
		t.Fatalf("slots not populated: a=%d b=%d", aEnrolled, bEnrolled)
	}
}

func TestEpochChangeIsNewExperiment(t *testing.T) {
	f1 := expFlag(t, map[string]float64{"control": 50, "candidate": 50}, nil)
	e1 := mustEvaluator(t, snap(map[string]*model.Flag{"exp-flag": f1}, nil))
	f2 := expFlag(t, map[string]float64{"control": 50, "candidate": 50}, func(f *model.Flag) {
		f.Experiment.Epoch = 2
	})
	e2 := mustEvaluator(t, snap(map[string]*model.Flag{"exp-flag": f2}, nil))

	moved := 0
	const n = 5000
	for i := 0; i < n; i++ {
		unit := fmt.Sprintf("user-%d", i)
		c := ctx(unit, map[string]any{"user_id": unit})
		v1, _ := e1.StringVariation("exp-flag", c, "?")
		v2, _ := e2.StringVariation("exp-flag", c, "?")
		if v1 != v2 {
			moved++
		}
	}
	// New epoch must redraw assignments (~50% move on a 50/50 split).
	if float64(moved)/n < 0.3 {
		t.Errorf("epoch bump moved only %d/%d users; hash coordinate must change", moved, n)
	}
}

func TestExperimentHashUsesExperimentID(t *testing.T) {
	// Two flags bound to the SAME experiment/epoch must assign identically
	// (that is what makes multi-flag experiments coherent); bound to
	// DIFFERENT experiments they must decorrelate.
	mk := func(key, expID string) *model.Flag {
		return expFlag(t, map[string]float64{"control": 50, "candidate": 50}, func(f *model.Flag) {
			f.Key = key
			f.Experiment = &model.ExperimentBinding{ID: expID, Epoch: 3, State: model.ExperimentRunning}
		})
	}
	e := mustEvaluator(t, snap(map[string]*model.Flag{
		"flag-a": mk("flag-a", "exp-shared"),
		"flag-b": mk("flag-b", "exp-shared"),
		"flag-c": mk("flag-c", "exp-other"),
	}, nil))

	sameCount, diffCount := 0, 0
	const n = 3000
	for i := 0; i < n; i++ {
		unit := fmt.Sprintf("user-%d", i)
		c := ctx(unit, map[string]any{"user_id": unit})
		va, _ := e.StringVariation("flag-a", c, "?")
		vb, _ := e.StringVariation("flag-b", c, "?")
		vc, _ := e.StringVariation("flag-c", c, "?")
		if va == vb {
			sameCount++
		}
		if va == vc {
			diffCount++
		}
	}
	if sameCount != n {
		t.Errorf("same experiment must assign identically: %d/%d equal", sameCount, n)
	}
	if float64(diffCount)/n > 0.6 || float64(diffCount)/n < 0.4 {
		t.Errorf("different experiments must decorrelate: %d/%d equal (~50%% expected)", diffCount, n)
	}
}

func TestTypedVariations(t *testing.T) {
	jsonFlag := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           "model-router", Type: model.FlagJSON,
		Variations: map[string]any{
			"control":   map[string]any{"provider": "qwen", "model": "qwen-max"},
			"candidate": map[string]any{"provider": "anthropic", "model": "approved"},
		},
		DefaultRule: &model.Rule{Variation: "control"},
	}
	numFlag := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           "limit", Type: model.FlagNumber,
		Variations:  map[string]any{"low": 10, "high": 100},
		DefaultRule: &model.Rule{Variation: "low"},
	}
	e := mustEvaluator(t, snap(map[string]*model.Flag{"model-router": jsonFlag, "limit": numFlag}, nil))

	jv, res := e.JSONVariation("model-router", ctx("u", nil), map[string]any{})
	m, ok := jv.(map[string]any)
	if !ok || m["provider"] != "qwen" || res.Reason != model.ReasonDefault {
		t.Fatalf("json variation: %v %+v", jv, res)
	}
	nv, res := e.NumberVariation("limit", ctx("u", nil), 1)
	if nv != 10 || res.Failed {
		t.Fatalf("number variation: %v %+v", nv, res)
	}
	// Wrong typed API on a number flag → caller default + TYPE_MISMATCH.
	sv, res := e.StringVariation("limit", ctx("u", nil), "fallback")
	if sv != "fallback" || res.ErrorCode != model.ErrTypeMismatch || !res.Failed {
		t.Fatalf("type mismatch: %q %+v", sv, res)
	}
	bv, res := e.BoolVariation("limit", ctx("u", nil), false)
	if bv != false || res.ErrorCode != model.ErrTypeMismatch {
		t.Fatalf("bool mismatch: %v %+v", bv, res)
	}
}

func TestInvalidSnapshotsRejected(t *testing.T) {
	bad := map[string]*model.Flag{
		"unregistered-field": {
			SchemaVersion: model.SchemaVersion,
			Key:           "unregistered-field", Type: model.FlagBool,
			Variations:  map[string]any{"on": true, "off": false},
			DefaultRule: &model.Rule{Variation: "off"},
			Targeting: []model.Rule{{
				Constraints: []model.Constraint{{Field: "ghost", Op: model.OpEq, Value: 1}},
				Variation:   "on",
			}},
		},
		"bad-split": {
			SchemaVersion: model.SchemaVersion,
			Key:           "bad-split", Type: model.FlagBool, Fields: []string{"user_id"},
			Bucketing:   &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id"},
			Variations:  map[string]any{"on": true, "off": false},
			DefaultRule: &model.Rule{Percentage: map[string]float64{"on": 60, "off": 60}},
		},
		"bad-algo": {
			SchemaVersion: model.SchemaVersion,
			Key:           "bad-algo", Type: model.FlagBool, Fields: []string{"user_id"},
			Bucketing:   &model.Bucketing{Algorithm: "murmur-3", Unit: "user_id"},
			Variations:  map[string]any{"on": true, "off": false},
			DefaultRule: &model.Rule{Percentage: map[string]float64{"on": 50, "off": 50}},
		},
		"exp-no-fallback": {
			SchemaVersion: model.SchemaVersion,
			Key:           "exp-no-fallback", Type: model.FlagBool, Fields: []string{"user_id"},
			Bucketing:   &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id"},
			Variations:  map[string]any{"on": true, "off": false},
			DefaultRule: &model.Rule{Percentage: map[string]float64{"on": 50, "off": 50}},
			Experiment: &model.ExperimentBinding{
				ID: "exp-9", Epoch: 1, State: model.ExperimentRunning,
			},
		},
	}
	for name, f := range bad {
		if _, err := evaluation.NewEvaluator(snap(map[string]*model.Flag{name: f}, nil)); err == nil {
			t.Errorf("%s: invalid flag accepted", name)
		}
	}
	// Corrupt snapshot metadata.
	if _, err := evaluation.NewEvaluator(&model.Snapshot{SchemaVersion: 99}); err == nil {
		t.Error("bad schemaVersion accepted")
	}
	if _, err := evaluation.NewEvaluator(nil); err == nil {
		t.Error("nil snapshot accepted")
	}
}

func TestTargetingMatchInsideExperimentMarksSource(t *testing.T) {
	f := expFlag(t, nil, func(f *model.Flag) {
		f.Targeting = []model.Rule{
			{Constraints: []model.Constraint{{Field: "user_id", Op: model.OpIn, Values: []any{"staff-1"}}}, Variation: "candidate"},
			{Percentage: map[string]float64{"control": 50, "candidate": 50}},
		}
		f.DefaultRule = &model.Rule{Variation: "control"}
	})
	e := mustEvaluator(t, snap(map[string]*model.Flag{"exp-flag": f}, nil))

	_, res := e.StringVariation("exp-flag", ctx("s", map[string]any{"user_id": "staff-1"}), "?")
	if res.Experiment == nil || res.Experiment.Source != "targeting" {
		t.Fatalf("deterministic arm inside experiment must report source=targeting: %+v", res)
	}
	_, res = e.StringVariation("exp-flag", ctx("u", map[string]any{"user_id": "regular-9"}), "?")
	if res.Experiment == nil || res.Experiment.Source != "split" {
		t.Fatalf("randomized arm must report source=split: %+v", res)
	}
}

// TestClientIPTargetingAndBucketing proves the reserved client_ip attribute
// (injected by golab-proxy from the trusted proxy chain — internal/proxy/
// clientip.go) drives in_cidr targeting and, though experiment-discouraged
// (docs 技术方案 §6), is a fully functional bucketing unit.
func TestClientIPTargetingAndBucketing(t *testing.T) {
	f := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           "ip-gray", Type: model.FlagString,
		Fields: []string{model.ClientIPField},
		Variations: map[string]any{
			"control": "control-value", "candidate": "candidate-value",
		},
		DefaultRule: &model.Rule{Variation: "control"},
		Targeting: []model.Rule{{
			Name: "internal-net",
			Constraints: []model.Constraint{
				{Field: model.ClientIPField, Op: model.OpInCIDR, Value: "10.0.0.0/8"},
			},
			Variation: "candidate",
		}},
	}
	e := mustEvaluator(t, snap(map[string]*model.Flag{"ip-gray": f}, nil))

	for _, c := range []struct {
		name       string
		attrs      map[string]any
		want       string
		wantReason model.Reason
	}{
		{"internal range hits candidate", map[string]any{"client_ip": "10.1.2.3"}, "candidate", model.ReasonTargetingMatch},
		{"internal edge of range", map[string]any{"client_ip": "10.255.255.255"}, "candidate", model.ReasonTargetingMatch},
		{"external range stays default", map[string]any{"client_ip": "203.0.113.9"}, "control", model.ReasonDefault},
		{"missing attribute never matches", nil, "control", model.ReasonDefault},
		{"malformed attribute never matches", map[string]any{"client_ip": "garbage"}, "control", model.ReasonDefault},
		{"v4-mapped form matches via unmap", map[string]any{"client_ip": "::ffff:10.1.2.3"}, "candidate", model.ReasonTargetingMatch},
	} {
		v, res := e.StringVariation("ip-gray", ctx("u1", c.attrs), "?")
		if res.VariationType != c.want || v != c.want+"-value" || res.Reason != c.wantReason {
			t.Errorf("%s: got %q/%s/%s, want variation %q/%s",
				c.name, v, res.VariationType, res.Reason, c.want, c.wantReason)
		}
	}

	// Same IP ⇒ same arm (deterministic); the unit resolves from attributes.
	split := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           "ip-split", Type: model.FlagString,
		Fields:    []string{model.ClientIPField},
		Bucketing: &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: model.ClientIPField, Salt: "s"},
		Variations: map[string]any{
			"control": "control-value", "candidate": "candidate-value",
		},
		DefaultRule: &model.Rule{Variation: "control"},
		Targeting: []model.Rule{
			{Percentage: map[string]float64{"control": 50, "candidate": 50}},
		},
	}
	e2 := mustEvaluator(t, snap(map[string]*model.Flag{"ip-split": split}, nil))
	first, res := e2.StringVariation("ip-split", ctx("", map[string]any{"client_ip": "203.0.113.7"}), "?")
	if res.Reason != model.ReasonSplit || res.Bucket < 0 {
		t.Fatalf("client_ip bucketing must produce a SPLIT: %q %+v", first, res)
	}
	for i := 0; i < 5; i++ {
		again, _ := e2.StringVariation("ip-split", ctx("", map[string]any{"client_ip": "203.0.113.7"}), "?")
		if again != first {
			t.Fatalf("client_ip bucketing not deterministic: %q vs %q", again, first)
		}
	}
	// Missing unit → caller default + MISSING_BUCKET_KEY, never a random
	// split (§6).
	v, res := e2.StringVariation("ip-split", ctx("", nil), "?")
	if v != "?" || !res.Failed || res.ErrorCode != model.ErrMissingBucketKey {
		t.Fatalf("missing client_ip unit must fail closed: %q %+v", v, res)
	}
}
