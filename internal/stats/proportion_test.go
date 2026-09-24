package stats

import (
	"math"
	mrand "math/rand/v2"
	"testing"
)

// ---------------------------------------------------------------------------
// Two-proportion z-test: known-value checks (docs/技术方案.md §10.3).
// ---------------------------------------------------------------------------

func TestProportionKnownCase(t *testing.T) {
	// XA=100/NA=1000 (pA=0.10) vs XB=150/NB=1000 (pB=0.15).
	// Pooled p̂=0.125, SE=sqrt(0.125·0.875·(1/1000+1/1000))=0.01479020,
	// z = 0.05/SE = 3.38061702, two-sided p = erfc(z/√2) = 7.2323272e-4.
	// (Reference values computed independently in exact arithmetic.)
	a := ProportionStats{N: 1000, Conversions: 100}
	b := ProportionStats{N: 1000, Conversions: 150}
	r, err := TwoProportionZTest(a, b, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.PA != 0.1 || r.PB != 0.15 || math.Abs(r.Diff-0.05) > 1e-12 {
		t.Fatalf("rates/diff wrong: %+v", r)
	}
	if math.Abs(r.Z-3.38061702) > 1e-6 {
		t.Fatalf("Z = %v, want 3.38061702 ± 1e-6", r.Z)
	}
	if math.Abs(r.PValue-7.2323272e-4) > 1e-9 {
		t.Fatalf("PValue = %v, want 7.2323272e-4 ± 1e-9", r.PValue)
	}
	if r.PValue >= 0.01 || !r.Significant {
		t.Fatalf("expected p < 0.01 and significant: %+v", r)
	}
	// Unpooled Wald CI: SE_u = sqrt(0.1·0.9/1000 + 0.15·0.85/1000)
	// = 0.01474788; half-width = 1.959964·SE_u = 0.02890531.
	if math.Abs(r.CIDiffLow-(0.05-0.02890531)) > 1e-5 || math.Abs(r.CIDiffHigh-(0.05+0.02890531)) > 1e-5 {
		t.Fatalf("CI = [%v, %v], want ≈ [0.0210947, 0.0789053]", r.CIDiffLow, r.CIDiffHigh)
	}
	if !r.RelativeMeaningful || math.Abs(r.RelativeDiff-0.5) > 1e-12 {
		t.Fatalf("RelativeDiff = %v (meaningful=%v), want 0.5", r.RelativeDiff, r.RelativeMeaningful)
	}
}

func TestProportionIdenticalArms(t *testing.T) {
	a := ProportionStats{N: 500, Conversions: 60}
	r, err := TwoProportionZTest(a, a, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Z != 0 || math.Abs(r.PValue-1) > 1e-12 || r.Significant {
		t.Fatalf("identical arms must give Z=0, p≈1, not significant: %+v", r)
	}
}

func TestProportionDegenerateZeroVariance(t *testing.T) {
	// Both arms all-zero conversions: pooled SE = 0 forces Diff = 0.
	// Must return p = 1, CI = [0,0], never NaN.
	a := ProportionStats{N: 1000, Conversions: 0}
	r, err := TwoProportionZTest(a, a, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.PValue != 1 || r.Z != 0 || r.Significant || r.CIDiffLow != 0 || r.CIDiffHigh != 0 {
		t.Fatalf("all-zero arms: want p=1, Z=0, not significant, CI=[0,0]; got %+v", r)
	}

	// Both arms all-one conversions.
	full := ProportionStats{N: 1000, Conversions: 1000}
	r, err = TwoProportionZTest(full, full, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.PValue != 1 || r.Significant {
		t.Fatalf("all-one arms: want p=1, not significant; got %+v", r)
	}

	// Extreme case: PA=0 vs PB=1 (unpooled SE = 0, pooled SE > 0).
	b := ProportionStats{N: 1000, Conversions: 1000}
	r, err = TwoProportionZTest(a, b, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Significant || r.PValue > 1e-100 || r.CIDiffLow != 1 || r.CIDiffHigh != 1 {
		t.Fatalf("0 vs 1 arms: want significant, p≈0, CI=[1,1]; got %+v", r)
	}
}

func TestProportionNearZeroBaseline(t *testing.T) {
	// PA = 0 → relative difference is not reported (docs §10.3).
	a := ProportionStats{N: 1000, Conversions: 0}
	b := ProportionStats{N: 1000, Conversions: 10}
	r, err := TwoProportionZTest(a, b, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.RelativeMeaningful || r.RelativeDiff != 0 {
		t.Fatalf("zero baseline: want RelativeMeaningful=false, RelativeDiff=0; got %+v", r)
	}
	if math.Abs(r.Diff-0.01) > 1e-12 {
		t.Fatalf("absolute diff must still be reported: %v", r.Diff)
	}
}

func TestProportionErrors(t *testing.T) {
	good := ProportionStats{N: 100, Conversions: 10}
	cases := []struct {
		name    string
		a, b    ProportionStats
		alpha   float64
		wantErr bool
	}{
		{"N=0", ProportionStats{N: 0}, good, 0.05, true},
		{"negative N", ProportionStats{N: -5, Conversions: 0}, good, 0.05, true},
		{"conversions > N", ProportionStats{N: 10, Conversions: 11}, good, 0.05, true},
		{"negative conversions", ProportionStats{N: 10, Conversions: -1}, good, 0.05, true},
		{"alpha=0", good, good, 0, true},
		{"alpha=1.5", good, good, 1.5, true},
		{"valid", good, good, 0.05, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TwoProportionZTest(tc.a, tc.b, tc.alpha)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Simulation study (docs P2 acceptance: 模拟数据检验 I 型错误与区间覆盖).
// Fixed seed — fully deterministic.
// ---------------------------------------------------------------------------

func TestProportionSimulationTypeIAndCoverage(t *testing.T) {
	const (
		iters   = 2000
		nPerArm = 1000
		pTrue   = 0.1
		alpha   = 0.05
	)
	rng := mrand.New(mrand.NewPCG(44, 44))

	drawBinomial := func() ProportionStats {
		var s ProportionStats
		s.N = nPerArm
		for i := 0; i < nPerArm; i++ {
			if rng.Float64() < pTrue {
				s.Conversions++
			}
		}
		return s
	}

	rejections, covered := 0, 0
	for i := 0; i < iters; i++ {
		a := drawBinomial()
		b := drawBinomial()
		r, err := TwoProportionZTest(a, b, alpha)
		if err != nil {
			t.Fatalf("iter %d: unexpected error: %v", i, err)
		}
		if r.Significant {
			rejections++
		}
		if r.CIDiffLow <= 0 && 0 <= r.CIDiffHigh {
			covered++
		}
	}
	typeI := float64(rejections) / iters
	coverage := float64(covered) / iters
	if typeI < 0.03 || typeI > 0.08 {
		t.Fatalf("Type-I error rate = %v (want in [0.03, 0.08])", typeI)
	}
	if coverage < 0.93 {
		t.Fatalf("95%% CI coverage of true diff 0 = %v (want >= 0.93)", coverage)
	}
	t.Logf("Proportion H0 (p=0.1, n=1000/arm): Type-I=%.4f coverage=%.4f (seed=44)", typeI, coverage)
}
