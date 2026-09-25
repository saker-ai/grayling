package bandit

import (
	"math"
	mrand "math/rand/v2"
	"testing"
)

// ---------------------------------------------------------------------------
// Numerical validation: Beta sampler against the analytic Beta mean, posterior
// update against hand computation, deterministic fixed-proportion split.
// ---------------------------------------------------------------------------

func TestBetaSamplerMeanMatchesAnalytic(t *testing.T) {
	rng := mrand.New(mrand.NewPCG(99, 99))
	cases := [][2]float64{{1, 1}, {2, 5}, {10, 10}, {0.5, 3}, {30, 2}}
	for _, c := range cases {
		a, b := c[0], c[1]
		const N = 200000
		sum := 0.0
		for i := 0; i < N; i++ {
			sum += betaSample(rng, a, b)
		}
		emp := sum / float64(N)
		want := a / (a + b)
		if math.Abs(emp-want) > 0.01 {
			t.Fatalf("Beta(%v,%v) sampler mean = %v, want analytic %v", a, b, emp, want)
		}
	}
}

func TestThompsonPosteriorUpdateKnown(t *testing.T) {
	th, err := NewThompson([]string{"a", "b"}, nil, 1, 1, 5)
	if err != nil {
		t.Fatalf("NewThompson: %v", err)
	}
	// arm a: 2 successes, 1 failure → Beta(1+2, 1+1) = Beta(3,2), mean 3/5.
	th.Update(0, 1)
	th.Update(0, 1)
	th.Update(0, 0)
	pm := th.PosteriorMean()
	if math.Abs(pm[0]-0.6) > 1e-12 {
		t.Fatalf("arm a posterior mean = %v, want 0.6", pm[0])
	}
	if math.Abs(pm[1]-0.5) > 1e-12 {
		t.Fatalf("arm b (untouched) mean = %v, want 0.5", pm[1])
	}
	// Reward clamping to [0,1].
	th2, _ := NewThompson([]string{"a", "b"}, nil, 1, 1, 5)
	th2.Update(0, 5) // clamps to 1 → Beta(2,1), mean 2/3
	if math.Abs(th2.PosteriorMean()[0]-2.0/3.0) > 1e-12 {
		t.Fatalf("reward clamp failed: mean = %v, want 2/3", th2.PosteriorMean()[0])
	}
}

func TestPickFixedProportions(t *testing.T) {
	fixed := []float64{0.5, 0.3, 0.2}
	const N = 10000
	counts := make([]int64, 3)
	for i := 0; i < N; i++ {
		counts[pickFixed(fixed, float64(i)/float64(N))]++
	}
	for k := range fixed {
		emp := float64(counts[k]) / float64(N)
		if math.Abs(emp-fixed[k]) > 0.01 {
			t.Fatalf("frozen split arm %d = %v, want %v", k, emp, fixed[k])
		}
	}
	// Edge buckets.
	if pickFixed(fixed, 0) != 0 {
		t.Fatal("bucket 0 must map to arm 0")
	}
	if pickFixed(fixed, 0.999999) != 2 {
		t.Fatal("bucket ~1 must map to the last arm")
	}
}

// ---------------------------------------------------------------------------
// Simulation validation: regret sub-linear and far below random, allocation
// converges to the best arm, determinism, safety freeze.
// ---------------------------------------------------------------------------

func TestThompsonRegretAndConvergence(t *testing.T) {
	trueP := []float64{0.30, 0.50, 0.40} // arm 1 (b) is optimal
	arms := []string{"a", "b", "c"}
	best := 0.50
	meanP := (0.30 + 0.50 + 0.40) / 3
	const T = 6000

	rewardRNG := mrand.New(mrand.NewPCG(11, 11))
	th, err := NewThompson(arms, nil, 1, 1, 22)
	if err != nil {
		t.Fatalf("NewThompson: %v", err)
	}

	var cumRegret float64
	counts := make([]int64, 3)
	secondHalfBest := 0
	for step := 0; step < T; step++ {
		arm, _ := th.Allocate(rewardRNG.Float64()) // bucket only used when frozen
		reward := 0.0
		if rewardRNG.Float64() < trueP[arm] {
			reward = 1
		}
		th.Update(arm, reward)
		cumRegret += best - trueP[arm]
		counts[arm]++
		if step >= T/2 && arm == 1 {
			secondHalfBest++
		}
	}

	randomRegret := float64(T) * (best - meanP)
	if cumRegret > 0.4*randomRegret {
		t.Fatalf("Thompson cumulative regret %v too high vs random %v", cumRegret, randomRegret)
	}
	if cumRegret/float64(T) > 0.05 {
		t.Fatalf("average regret %v too high (want < 0.05)", cumRegret/float64(T))
	}
	fracBest := float64(secondHalfBest) / float64(T/2)
	if fracBest < 0.7 {
		t.Fatalf("best-arm allocation fraction in second half = %v, want > 0.7 (no convergence)", fracBest)
	}
	t.Logf("Thompson: cumRegret=%.1f (random=%.1f, ratio=%.3f) avgRegret=%.4f bestArmFrac2ndHalf=%.3f counts=%v seed=22",
		cumRegret, randomRegret, cumRegret/randomRegret, cumRegret/float64(T), fracBest, counts)
}

func TestEpsilonGreedyRegretBeatsRandom(t *testing.T) {
	trueP := []float64{0.30, 0.50, 0.40}
	best := 0.50
	meanP := (0.30 + 0.50 + 0.40) / 3
	const T = 6000
	rewardRNG := mrand.New(mrand.NewPCG(12, 12))
	eg, err := NewEpsilonGreedy([]string{"a", "b", "c"}, nil, 1, 1, 0.1, 23)
	if err != nil {
		t.Fatalf("NewEpsilonGreedy: %v", err)
	}
	var cumRegret float64
	for step := 0; step < T; step++ {
		arm, _ := eg.Allocate(rewardRNG.Float64())
		reward := 0.0
		if rewardRNG.Float64() < trueP[arm] {
			reward = 1
		}
		eg.Update(arm, reward)
		cumRegret += best - trueP[arm]
	}
	randomRegret := float64(T) * (best - meanP)
	// ε-greedy (ε=0.1) keeps exploring ~10% forever, so regret is higher than
	// Thompson but must still beat uniform-random allocation clearly.
	if cumRegret > 0.6*randomRegret {
		t.Fatalf("ε-greedy regret %v not clearly below random %v", cumRegret, randomRegret)
	}
	t.Logf("ε-greedy(0.1): cumRegret=%.1f (random=%.1f, ratio=%.3f) seed=23", cumRegret, randomRegret, cumRegret/randomRegret)
}

func TestThompsonSafetyFreeze(t *testing.T) {
	arms := []string{"a", "b", "c"}
	fixed := []float64{0.5, 0.3, 0.2}
	th, _ := NewThompson(arms, fixed, 1, 1, 33)
	// Train so arm a dominates; adaptive Thompson would then route ~all to a.
	for i := 0; i < 200; i++ {
		th.Update(0, 1)
		th.Update(1, 0)
		th.Update(2, 0)
	}
	// SRM alarm freezes exploration.
	if frozen := th.ApplySafety(false, true, ""); !frozen {
		t.Fatal("SRM-unhealthy must freeze the bandit")
	}
	if !th.Frozen() || th.FreezeReason() == "" {
		t.Fatalf("expected frozen with a reason, got frozen=%v reason=%q", th.Frozen(), th.FreezeReason())
	}
	// While frozen, allocation must follow the FIXED proportions exactly
	// (uniform buckets), ignoring the dominant posterior — no exploration.
	const N = 3000
	counts := make([]int64, 3)
	for i := 0; i < N; i++ {
		arm, rec := th.Allocate(float64(i) / float64(N))
		counts[arm]++
		if !rec.Frozen {
			t.Fatal("record must be flagged frozen")
		}
	}
	for k := range fixed {
		emp := float64(counts[k]) / float64(N)
		if math.Abs(emp-fixed[k]) > 0.02 {
			t.Fatalf("frozen arm %d = %v, want fixed %v (exploration not halted)", k, emp, fixed[k])
		}
	}
	// Unfreeze resumes adaptive exploration (would now route ~all to arm a).
	th.Unfreeze()
	if th.Frozen() {
		t.Fatal("unfreeze must clear the frozen state")
	}
	aCount := 0
	for i := 0; i < 500; i++ {
		arm, rec := th.Allocate(0.5)
		if rec.Frozen {
			t.Fatal("record must not be frozen after unfreeze")
		}
		if arm == 0 {
			aCount++
		}
	}
	if float64(aCount)/500 < 0.9 {
		t.Fatalf("after unfreeze, dominant arm a should get ~all traffic, got %v", float64(aCount)/500)
	}
}

func TestThompsonGuardrailFreeze(t *testing.T) {
	th, _ := NewThompson([]string{"a", "b"}, nil, 1, 1, 34)
	// Healthy: no freeze.
	if th.ApplySafety(true, true, "") {
		t.Fatal("healthy signals must not freeze")
	}
	// Guardrail breach: freeze.
	if !th.ApplySafety(true, false, "") {
		t.Fatal("guardrail breach must freeze")
	}
	if th.FreezeReason() != "guardrail breach" {
		t.Fatalf("freeze reason = %q, want 'guardrail breach'", th.FreezeReason())
	}
}

func TestThompsonDeterminism(t *testing.T) {
	run := func(seed uint64) []int {
		th, _ := NewThompson([]string{"a", "b", "c"}, nil, 1, 1, seed)
		out := make([]int, 0, 60)
		for i := 0; i < 60; i++ {
			arm, _ := th.Allocate(0.5)
			th.Update(arm, float64(i%2))
			out = append(out, arm)
		}
		return out
	}
	x, y := run(7), run(7)
	for i := range x {
		if x[i] != y[i] {
			t.Fatalf("same seed diverged at step %d: %v vs %v", i, x[i], y[i])
		}
	}
}

func TestThompsonLogBounded(t *testing.T) {
	th, _ := NewThompson([]string{"a", "b"}, nil, 1, 1, 35)
	for i := 0; i < defaultMaxLog+1000; i++ {
		arm, _ := th.Allocate(0.5)
		th.Update(arm, float64(i%2))
	}
	if len(th.Log()) > defaultMaxLog {
		t.Fatalf("log grew unbounded: %d > %d", len(th.Log()), defaultMaxLog)
	}
	// Log records step numbers (monotonic) — the last record's step == total.
	log := th.Log()
	if log[len(log)-1].Step != int64(defaultMaxLog+1000) {
		t.Fatalf("last step = %d, want %d", log[len(log)-1].Step, defaultMaxLog+1000)
	}
}

func TestBanditErrors(t *testing.T) {
	if _, err := NewThompson([]string{"a"}, nil, 1, 1, 1); err == nil {
		t.Fatal("<2 arms must error")
	}
	if _, err := NewThompson([]string{"a", "a"}, nil, 1, 1, 1); err == nil {
		t.Fatal("duplicate arms must error")
	}
	if _, err := NewThompson([]string{"a", ""}, nil, 1, 1, 1); err == nil {
		t.Fatal("empty arm name must error")
	}
	if _, err := NewThompson([]string{"a", "b"}, []float64{0.5, 0.6}, 1, 1, 1); err == nil {
		t.Fatal("fixed proportions not summing to 1 must error")
	}
	if _, err := NewThompson([]string{"a", "b"}, []float64{0.5}, 1, 1, 1); err == nil {
		t.Fatal("fixed proportions length mismatch must error")
	}
	if _, err := NewEpsilonGreedy([]string{"a", "b"}, nil, 1, 1, 1.0, 1); err == nil {
		t.Fatal("epsilon=1 must error")
	}
	if _, err := NewThompson([]string{"a", "b"}, nil, 1, 1, 1); err != nil {
		t.Fatalf("valid construction errored: %v", err)
	}
}

// Compile-time assertion that both policies satisfy the Policy interface.
var _ Policy = (*Thompson)(nil)
var _ Policy = (*EpsilonGreedy)(nil)
