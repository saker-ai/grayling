package gffimport

import (
	"fmt"
	"math"
	"testing"

	"github.com/godeps/golab/pkg/evaluation"
	"github.com/godeps/golab/pkg/model"
)

// split5050Doc is a valid gff flag with a 50/50 percentage default rule —
// the shape used for the normative contrast test (docs/技术方案.md §2, §5.2:
// legacy gff evaluation and the GoLab fixed-bucket algorithm must NOT be
// claimed to assign the same users).
const split5050Doc = `
variations:
  a: true
  b: false
defaultRule:
  percentage:
    a: 50
    b: 50
`

func TestLegacyDeterministicForSameUser(t *testing.T) {
	legacy, err := NewLegacyEvaluator(map[string][]byte{"split": []byte(split5050Doc)})
	if err != nil {
		t.Fatalf("NewLegacyEvaluator: %v", err)
	}
	first, res := legacy.BoolVariation("split", "user-42", nil, false)
	if res.Failed || res.ErrorCode != "" {
		t.Fatalf("unexpected failure: %+v", res)
	}
	if res.Reason != "SPLIT" {
		t.Errorf("Reason = %q, want SPLIT (percentage default rule)", res.Reason)
	}
	if res.VariationType != "a" && res.VariationType != "b" {
		t.Errorf("VariationType = %q", res.VariationType)
	}
	for i := 0; i < 20; i++ {
		v, r := legacy.BoolVariation("split", "user-42", nil, false)
		if v != first || r.VariationType != res.VariationType {
			t.Fatalf("iteration %d: gff evaluation is not deterministic for the same user", i)
		}
	}
}

func TestLegacyTypeMismatchReturnsDefault(t *testing.T) {
	doc := `
variations:
  a: "red"
  b: "blue"
defaultRule:
  variation: a
`
	legacy, err := NewLegacyEvaluator(map[string][]byte{"colors": []byte(doc)})
	if err != nil {
		t.Fatalf("NewLegacyEvaluator: %v", err)
	}
	v, res := legacy.BoolVariation("colors", "user-1", nil, true)
	if v != true {
		t.Errorf("value = %v, want the caller default true", v)
	}
	if !res.Failed || res.ErrorCode != "TYPE_MISMATCH" {
		t.Errorf("result = %+v, want Failed with TYPE_MISMATCH", res)
	}
	if res.VariationType != "SdkDefault" {
		t.Errorf("VariationType = %q, want SdkDefault", res.VariationType)
	}
}

func TestLegacyUnknownFlag(t *testing.T) {
	legacy, err := NewLegacyEvaluator(map[string][]byte{"known": []byte(split5050Doc)})
	if err != nil {
		t.Fatalf("NewLegacyEvaluator: %v", err)
	}
	v, res := legacy.BoolVariation("missing", "user-1", nil, true)
	if v != true {
		t.Errorf("value = %v, want the caller default", v)
	}
	if !res.Failed || res.ErrorCode != "FLAG_NOT_FOUND" || res.Reason != "ERROR" {
		t.Errorf("result = %+v, want Failed FLAG_NOT_FOUND/ERROR", res)
	}

	if _, err := NewLegacyEvaluator(nil); err == nil {
		t.Error("NewLegacyEvaluator(nil): want error for empty map")
	}
}

func TestLegacyQueryTargetingWithAttrs(t *testing.T) {
	doc := `
variations:
  standard: "std"
  acme: "acme-plan"
defaultRule:
  variation: standard
targeting:
  - name: acme-org
    query: tenant eq "acme"
    variation: acme
`
	legacy, err := NewLegacyEvaluator(map[string][]byte{"plan": []byte(doc)})
	if err != nil {
		t.Fatalf("NewLegacyEvaluator: %v", err)
	}
	v, res := legacy.StringVariation("plan", "user-1", map[string]any{"tenant": "acme"}, "std")
	if v != "acme-plan" || res.Failed || res.Reason != "TARGETING_MATCH" {
		t.Errorf("acme user: got %q %+v", v, res)
	}
	v, res = legacy.StringVariation("plan", "user-2", map[string]any{"tenant": "globex"}, "std")
	if v != "std" || res.Failed || res.Reason != "DEFAULT" {
		t.Errorf("globex user: got %q %+v", v, res)
	}
}

func TestLegacyNumberAndJSONVariations(t *testing.T) {
	doc := `
variations:
  low: 1.5
  high: 9.25
defaultRule:
  variation: low
`
	legacy, err := NewLegacyEvaluator(map[string][]byte{"limit": []byte(doc)})
	if err != nil {
		t.Fatalf("NewLegacyEvaluator: %v", err)
	}
	// gff quirk: YAML integer variations decode as Go int and mismatch
	// float64, so the doc above uses explicit floats.
	v, res := legacy.NumberVariation("limit", "user-1", nil, 0)
	if v != 1.5 || res.Failed {
		t.Errorf("NumberVariation = %v %+v", v, res)
	}

	jsonDoc := `
variations:
  control: {color: "red", size: 1}
  candidate: {color: "blue", size: 2}
defaultRule:
  variation: candidate
`
	legacy, err = NewLegacyEvaluator(map[string][]byte{"cfg": []byte(jsonDoc)})
	if err != nil {
		t.Fatalf("NewLegacyEvaluator: %v", err)
	}
	jv, jres := legacy.JSONVariation("cfg", "user-1", nil, map[string]any{})
	m, ok := jv.(map[string]any)
	if !ok || m["color"] != "blue" || jres.Failed {
		t.Errorf("JSONVariation = %v %+v", jv, jres)
	}
}

// TestContrastLegacyVsNativeAssignment is NORMATIVE (docs/技术方案.md §2,
// §5.2): for the same 50/50 gff document, the legacy gff algorithm and the
// imported GoLab fixed-bucket evaluator must both honor the split ratio
// (±5pp over 2000 users) while disagreeing on a substantial fraction of
// per-user assignments (>20%) — the two hash algorithms are independent and
// GoLab can never claim gff-identical assignments.
func TestContrastLegacyVsNativeAssignment(t *testing.T) {
	const (
		flagKey = "contrast-split"
		users   = 2000
	)

	legacy, err := NewLegacyEvaluator(map[string][]byte{flagKey: []byte(split5050Doc)})
	if err != nil {
		t.Fatalf("NewLegacyEvaluator: %v", err)
	}

	imported, _, err := ImportFlag([]byte(split5050Doc), Options{
		Workspace: "ws", Environment: "prod", FlagKey: flagKey,
	})
	if err != nil {
		t.Fatalf("ImportFlag: %v", err)
	}
	snap := &model.Snapshot{
		SchemaVersion: model.SchemaVersion,
		Workspace:     "ws",
		Environment:   "prod",
		Flags:         map[string]*model.Flag{flagKey: imported},
	}
	native, err := evaluation.NewEvaluator(snap)
	if err != nil {
		t.Fatalf("evaluation.NewEvaluator: %v", err)
	}

	legacyA, nativeA, disagree := 0, 0, 0
	for i := 0; i < users; i++ {
		uid := fmt.Sprintf("user-%04d", i)

		_, lres := legacy.BoolVariation(flagKey, uid, nil, false)
		if lres.Failed {
			t.Fatalf("legacy evaluation failed for %s: %+v", uid, lres)
		}

		nres := native.Evaluate(flagKey, model.EvaluationContext{TargetingKey: uid})
		if nres.Failed {
			t.Fatalf("native evaluation failed for %s: %+v", uid, nres)
		}
		if nres.Reason != model.ReasonSplit {
			t.Fatalf("native reason = %q, want SPLIT", nres.Reason)
		}

		if lres.VariationType == "a" {
			legacyA++
		}
		if nres.VariationType == "a" {
			nativeA++
		}
		if lres.VariationType != nres.VariationType {
			disagree++
		}
	}

	legacyShare := float64(legacyA) / users
	nativeShare := float64(nativeA) / users
	disagreeShare := float64(disagree) / users
	t.Logf("legacy a-share=%.3f native a-share=%.3f disagreement=%.3f",
		legacyShare, nativeShare, disagreeShare)

	// Both algorithms must reproduce the configured 50/50 ratio within ±5pp.
	if math.Abs(legacyShare-0.5) > 0.05 {
		t.Errorf("legacy a-share %.3f deviates more than 5pp from 50%%", legacyShare)
	}
	if math.Abs(nativeShare-0.5) > 0.05 {
		t.Errorf("native a-share %.3f deviates more than 5pp from 50%%", nativeShare)
	}
	// Independent hashes: per-user assignments must disagree substantially.
	// (Expectation ≈ 50% for independent algorithms; assert > 20% to stay
	// robust while still proving the assignments are NOT interchangeable.)
	if disagreeShare <= 0.20 {
		t.Errorf("disagreement %.3f ≤ 0.20: legacy gff and GoLab assignments are suspiciously aligned; "+
			"the two algorithms must be independent (docs §5.2: 不能承诺原用户分配保持不变)", disagreeShare)
	}
}
