package judge

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/godeps/golab/internal/stats"
)

// ---------------------------------------------------------------------------
// Calibration: Cohen's κ against a hand-computed value, good/bad-judge
// detection, position/length bias diagnostics.
// ---------------------------------------------------------------------------

func TestCohenKappaKnownValue(t *testing.T) {
	// gold =[A,A,B,B,tie]; judge=[A,A,B,A,tie]
	// p_o = 4/5 = 0.80
	// n_gold: A2 B2 tie1 ; n_judge: A3 B1 tie1
	// p_e = (2/5)(3/5)+(2/5)(1/5)+(1/5)(1/5) = .24+.08+.04 = 0.36 → wait recompute
	//   A: (2/5)(3/5)=0.24 ; B: (2/5)(1/5)=0.08 ; tie: (1/5)(1/5)=0.04 → p_e=0.36
	// κ = (0.80 − 0.36)/(1 − 0.36) = 0.44/0.64 = 0.6875
	gold := []string{"A", "A", "B", "B", "tie"}
	judge := []string{"A", "A", "B", "A", "tie"}
	kappa, po, cats := cohenKappa(gold, judge)
	if math.Abs(po-0.8) > 1e-12 {
		t.Fatalf("p_o = %v, want 0.8", po)
	}
	if math.Abs(kappa-0.6875) > 1e-9 {
		t.Fatalf("κ = %v, want 0.6875", kappa)
	}
	if len(cats) != 3 {
		t.Fatalf("categories = %v, want 3 (A,B,tie)", cats)
	}
}

func TestCohenKappaPerfectAndChance(t *testing.T) {
	// Perfect agreement.
	if k, _, _ := cohenKappa([]string{"A", "B", "A"}, []string{"A", "B", "A"}); math.Abs(k-1) > 1e-12 {
		t.Fatalf("perfect agreement κ = %v, want 1", k)
	}
	// Agreement exactly at chance → κ ≈ 0.
	gold := []string{"A", "A", "B", "B"}
	judge := []string{"A", "B", "A", "B"} // p_o=0, but let's use a chance-level case
	if k, _, _ := cohenKappa(gold, judge); k > 0.001 {
		t.Fatalf("disagreeing κ = %v, want <= 0", k)
	}
	// Chance-level: p_o = p_e → κ ≈ 0.
	gold2 := []string{"A", "A", "B", "B"}
	judge2 := []string{"A", "B", "B", "A"} // p_o = 2/4 = 0.5, p_e = 0.5 → κ=0
	k2, po2, _ := cohenKappa(gold2, judge2)
	if math.Abs(po2-0.5) > 1e-12 || math.Abs(k2) > 1e-9 {
		t.Fatalf("chance-level: p_o=%v κ=%v, want 0.5 / 0", po2, k2)
	}
}

func makeItems(n int, agreeFrac float64, positionBias float64) []CalibrationItem {
	items := make([]CalibrationItem, 0, n)
	for i := 0; i < n; i++ {
		gold := "A"
		if i%2 == 1 {
			gold = "B"
		}
		judge := gold
		// Introduce disagreement on a fraction of items.
		if float64(i%100)/100 >= agreeFrac {
			if judge == "A" {
				judge = "B"
			} else {
				judge = "A"
			}
		}
		presented := "A"
		if positionBias > 0 && float64((i/2)%100)/100 < positionBias {
			// flip presented-first so judge tends to pick first
			presented = "B"
			if judge != "B" {
				judge = "B" // bias toward first-presented (B here)
			}
		}
		items = append(items, CalibrationItem{
			ID: string(rune('a' + i%26)), Gold: gold, Judge: judge,
			PresentedFirst: presented, LenA: 100 + i%10, LenB: 100,
		})
	}
	return items
}

func TestCalibrateGoodJudgePasses(t *testing.T) {
	items := makeItems(200, 0.92, 0) // 92% agreement, no position bias
	rep := Calibrate(items, CalibrationConfig{MinN: 30, MinKappa: 0.6, MaxPositionBias: 0.10})
	if !rep.Calibrated {
		t.Fatalf("high-agreement judge should be calibrated: %+v", rep)
	}
	if rep.Accuracy < 0.85 {
		t.Fatalf("accuracy = %v, want high", rep.Accuracy)
	}
	if rep.CohenKappa < 0.6 {
		t.Fatalf("κ = %v, want >= 0.6", rep.CohenKappa)
	}
}

func TestCalibrateBadJudgeFails(t *testing.T) {
	// Low agreement (chance-level) → κ near 0 → uncalibrated.
	items := makeItems(200, 0.50, 0)
	rep := Calibrate(items, CalibrationConfig{MinN: 30, MinKappa: 0.6, MaxPositionBias: 0.10})
	if rep.Calibrated {
		t.Fatalf("chance-level judge must NOT be calibrated: %+v", rep)
	}
	if rep.Reason == "" {
		t.Fatal("uncalibrated report must carry a reason")
	}
	// Too-few-samples gate.
	small := makeItems(5, 1.0, 0)
	repSmall := Calibrate(small, CalibrationConfig{MinN: 30, MinKappa: 0.6})
	if repSmall.Calibrated || repSmall.Reason != "sample too small" {
		t.Fatalf("small sample must be uncalibrated with reason: %+v", repSmall)
	}
}

// ---------------------------------------------------------------------------
// Position randomization neutralizes systematic position bias (§10.4).
// ---------------------------------------------------------------------------

func TestPositionRandomizationNeutralizesBias(t *testing.T) {
	// A judge with a strong first-position bias, on quality-NEUTRAL items.
	mock := NewMockProvider(map[string]float64{}, 0.9, 0.0, 7) // PositionBias=0.9

	// (a) Without randomization (fixed A-first): the bias shows up directly.
	ctx := context.Background()
	firstWins := 0
	const direct = 2000
	for i := 0; i < direct; i++ {
		sc, _ := mock.ScorePairwise(ctx, PairwiseRequest{ResponseFirst: "A", ResponseSecond: "B"})
		if sc.Winner == "first" {
			firstWins++
		}
	}
	directFrac := float64(firstWins) / direct
	if directFrac < 0.85 {
		t.Fatalf("fixed-position biased judge should favor first (~0.95), got %v", directFrac)
	}

	// (b) Through the pipeline (position randomized 50/50): the systematic
	// bias must cancel → P(winner = A) ≈ 0.5 for quality-neutral items.
	mock2 := NewMockProvider(map[string]float64{}, 0.9, 0.0, 7)
	pipe, err := NewPipeline(mock2, 1.0, 99, "rubric-v1")
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	items := make([]JudgeItem, 3000)
	for i := range items {
		items[i] = JudgeItem{ID: string(rune(i)), ResponseA: "A", ResponseB: "B"}
	}
	judged := pipe.Run(ctx, items)
	aWins := 0
	for _, j := range judged {
		if j.Winner == "A" {
			aWins++
		}
	}
	pA := float64(aWins) / float64(len(judged))
	if math.Abs(pA-0.5) > 0.05 {
		t.Fatalf("after position randomization P(A) = %v, want ≈ 0.5 (bias not neutralized)", pA)
	}
	t.Logf("position bias: fixed-order P(first)=%.3f → randomized P(A)=%.3f (neutralized)", directFrac, pA)
}

// ---------------------------------------------------------------------------
// Pipeline: sampling rate, design weights, quality-winner recovery.
// ---------------------------------------------------------------------------

func TestPipelineSamplingAndWeights(t *testing.T) {
	mock := NewMockProvider(map[string]float64{"hi": 0.9, "lo": 0.1}, 0, 0, 3)
	pipe, _ := NewPipeline(mock, 0.3, 42, "rubric-v1")
	items := make([]JudgeItem, 2000)
	for i := range items {
		items[i] = JudgeItem{ID: string(rune(i)), ResponseA: "lo", ResponseB: "hi"}
	}
	judged := pipe.Run(context.Background(), items)
	rate := float64(len(judged)) / float64(len(items))
	if math.Abs(rate-0.3) > 0.04 {
		t.Fatalf("inclusion rate = %v, want ≈ 0.30", rate)
	}
	// Design weight = 1/0.3 for every included item; recorded per item.
	for _, j := range judged {
		if math.Abs(j.Weight-1/0.3) > 1e-9 || math.Abs(j.InclusionProb-0.3) > 1e-12 {
			t.Fatalf("weight/inclusion wrong: %+v", j)
		}
	}
	// B ("hi") is higher quality → de-randomized winner should be B for all.
	winB, _ := WeightedWinRate(judged)
	if winB < 0.98 {
		t.Fatalf("weighted P(B wins) = %v, want ≈ 1 (B is higher quality)", winB)
	}
}

func TestPipelinePerItemInclusionProb(t *testing.T) {
	mock := NewMockProvider(map[string]float64{"x": 0.5}, 0, 0, 5)
	pipe, _ := NewPipeline(mock, 1.0, 8, "rubric")
	items := []JudgeItem{
		{ID: "1", ResponseA: "x", ResponseB: "x", InclProb: 0.25},
		{ID: "2", ResponseA: "x", ResponseB: "x", InclProb: 1.0},
	}
	// Item 2 always included (prob 1.0); item 1 included 25% of the time.
	judged := pipe.Run(context.Background(), items)
	for _, j := range judged {
		if j.ID == "2" && math.Abs(j.Weight-1.0) > 1e-9 {
			t.Fatalf("item 2 weight = %v, want 1.0", j.Weight)
		}
		if j.ID == "1" && math.Abs(j.Weight-4.0) > 1e-9 {
			t.Fatalf("item 1 weight = %v, want 4.0 (1/0.25)", j.Weight)
		}
	}
}

func TestMockDeterminism(t *testing.T) {
	run := func(seed uint64) []string {
		m := NewMockProvider(map[string]float64{"a": 0.4, "b": 0.6}, 0.2, 0.1, seed)
		var out []string
		for i := 0; i < 30; i++ {
			sc, _ := m.ScorePairwise(context.Background(), PairwiseRequest{ResponseFirst: "a", ResponseSecond: "b"})
			out = append(out, sc.Winner)
		}
		return out
	}
	x, y := run(123), run(123)
	for i := range x {
		if x[i] != y[i] {
			t.Fatalf("mock not deterministic at %d: %v vs %v", i, x[i], y[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Guardrail wiring: uncalibrated judge is diagnostic-only; calibrated judge
// gates the decision (docs §10.4). Verified end to end through stats.Decide.
// ---------------------------------------------------------------------------

func healthySRM() stats.SRMResult {
	return stats.SRMResult{ChiSquare: 0.1, PValue: 0.75, Healthy: true,
		Observed: []float64{500, 500}, Expected: []float64{500, 500}}
}

func baseDecisionInput(g stats.GuardrailCheck) stats.DecisionInput {
	return stats.DecisionInput{
		PrimaryPValue: 0.30, PrimarySignificant: false, PrimaryDiff: 0.004,
		PrimaryCI: [2]float64{-0.01, 0.02}, Alpha: 0.05, MDE: 0.02,
		MinSamplePerArm: 100, NA: 2000, NB: 2000, SRM: healthySRM(),
		Guardrails:          []stats.GuardrailCheck{g},
		MinRuntimeSatisfied: true, DataHealthy: true,
	}
}

func TestQualityGuardrailUncalibratedIsDiagnosticOnly(t *testing.T) {
	// Large regression, but the judge is UNCALIBRATED → must NOT breach (§10.4).
	uncal := CalibrationReport{Calibrated: false, Reason: "kappa below threshold", CohenKappa: 0.2, N: 50}
	g := QualityGuardrail("judge_quality", 0.80, 0.50, 0.10, uncal, 500, 100)
	if g.Breached {
		t.Fatalf("uncalibrated judge must be diagnostic-only (never breach): %+v", g)
	}
	d := stats.Decide(baseDecisionInput(g))
	if d.Verdict == stats.VerdictRollback {
		t.Fatalf("uncalibrated judge regression must not force rollback: verdict=%s", d.Verdict)
	}
}

func TestQualityGuardrailCalibratedBreachesAndGates(t *testing.T) {
	cal := CalibrationReport{Calibrated: true, CohenKappa: 0.8, N: 200}
	// Calibrated judge + 37.5% regression > 10% threshold → breach.
	g := QualityGuardrail("judge_quality", 0.80, 0.50, 0.10, cal, 500, 100)
	if !g.Breached {
		t.Fatalf("calibrated judge regression must breach: %+v", g)
	}
	d := stats.Decide(baseDecisionInput(g))
	if d.Verdict != stats.VerdictRollback {
		t.Fatalf("judge guardrail breach must drive rollback through the gate: verdict=%s (%v)", d.Verdict, d.Reasons)
	}
	// Improvement (treatment better) → no breach.
	gOK := QualityGuardrail("judge_quality", 0.80, 0.85, 0.10, cal, 500, 100)
	if gOK.Breached {
		t.Fatalf("judge improvement must not breach: %+v", gOK)
	}
	// Insufficient sample → not gating.
	gSmall := QualityGuardrail("judge_quality", 0.80, 0.10, 0.10, cal, 5, 100)
	if gSmall.Breached {
		t.Fatalf("insufficient judge sample must not breach: %+v", gSmall)
	}
}

// ---------------------------------------------------------------------------
// HTTP provider: config-only integration point, validated against a local fake
// endpoint (never a real LLM). Missing key/endpoint is refused.
// ---------------------------------------------------------------------------

func TestHTTPProviderRefusesWithoutConfig(t *testing.T) {
	if _, err := NewHTTPProvider(HTTPProviderConfig{Endpoint: "", APIKey: "k", Model: "m"}); err == nil {
		t.Fatal("missing endpoint must be refused")
	}
	if _, err := NewHTTPProvider(HTTPProviderConfig{Endpoint: "http://x", APIKey: "", Model: "m"}); err == nil {
		t.Fatal("missing API key must be refused (never fake a real model)")
	}
	if _, err := NewHTTPProvider(HTTPProviderConfig{Endpoint: "http://x", APIKey: "k", Model: ""}); err == nil {
		t.Fatal("missing model must be refused")
	}
}

func TestHTTPProviderPlumbingAgainstFakeEndpoint(t *testing.T) {
	var gotAuth, gotModel string
	var gotFirst, gotSecond string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		gotFirst, _ = body["responseFirst"].(string)
		gotSecond, _ = body["responseSecond"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"winner":"second","rationale":"fake endpoint"}`))
	}))
	defer srv.Close()

	p, err := NewHTTPProvider(HTTPProviderConfig{
		Endpoint: srv.URL, APIKey: "secret-key", Model: "fake-judge-model", Version: "v1",
	})
	if err != nil {
		t.Fatalf("NewHTTPProvider: %v", err)
	}
	sc, err := p.ScorePairwise(context.Background(), PairwiseRequest{
		Rubric: "r", Prompt: "p", ResponseFirst: "AAA", ResponseSecond: "BBB",
	})
	if err != nil {
		t.Fatalf("ScorePairwise: %v", err)
	}
	if sc.Winner != "second" || sc.Model != "fake-judge-model" || sc.Version != "v1" {
		t.Fatalf("parsed score wrong: %+v", sc)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("auth header = %q, want Bearer secret-key", gotAuth)
	}
	if gotModel != "fake-judge-model" || gotFirst != "AAA" || gotSecond != "BBB" {
		t.Fatalf("request body not transmitted correctly: model=%q first=%q second=%q", gotModel, gotFirst, gotSecond)
	}

	// Invalid winner from the endpoint must be rejected, not passed through.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"winner":"maybe"}`))
	}))
	defer bad.Close()
	pb, _ := NewHTTPProvider(HTTPProviderConfig{Endpoint: bad.URL, APIKey: "k", Model: "m"})
	if _, err := pb.ScorePairwise(context.Background(), PairwiseRequest{ResponseFirst: "a", ResponseSecond: "b"}); err == nil {
		t.Fatal("invalid winner must be rejected")
	}
}
