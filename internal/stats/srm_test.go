package stats

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// Internal gamma-function tests (chi-square survival, stdlib-only).
// ---------------------------------------------------------------------------

func TestGammaQKnownValues(t *testing.T) {
	// Q(1, x) = e^{−x} exactly (Γ(1,x) = e^{−x}).
	if got, want := gammaQ(1, 2), math.Exp(-2); math.Abs(got-want) > 1e-12 {
		t.Fatalf("gammaQ(1, 2) = %v, want %v", got, want)
	}
	// Q(0.5, x) = erfc(√x): chi² with 1 df reduces to the normal tail.
	if got, want := gammaQ(0.5, 0.5), math.Erfc(math.Sqrt(0.5)); math.Abs(got-want) > 1e-12 {
		t.Fatalf("gammaQ(0.5, 0.5) = %v, want %v", got, want)
	}
	// Q(a, 0) = 1.
	if got := gammaQ(3.5, 0); got != 1 {
		t.Fatalf("gammaQ(3.5, 0) = %v, want 1", got)
	}
	// Invalid arguments → NaN, never a panic.
	if !math.IsNaN(gammaQ(0, 1)) || !math.IsNaN(gammaQ(1, -1)) {
		t.Fatal("gammaQ must return NaN for invalid arguments")
	}
}

func TestChi2SurvivalKnownValues(t *testing.T) {
	// chi²=16 with 1 df: exact value erfc(4/√2) = 6.3342484e-05.
	// (Spec reference: ≈6.3348e-05 within abs tol 1e-7.)
	got := chi2Survival(16, 1)
	if math.Abs(got-6.3348e-05) > 1e-7 {
		t.Fatalf("chi2Survival(16, 1) = %v, want ≈ 6.3348e-05 (abs tol 1e-7)", got)
	}
	if want := math.Erfc(4 / math.Sqrt2); math.Abs(got-want) > 1e-12 {
		t.Fatalf("chi2Survival(16, 1) = %v, want exact %v", got, want)
	}
	// chi²=3.841459 with 1 df ≈ 0.05 (the classic 95% critical value).
	if got := chi2Survival(3.841459, 1); math.Abs(got-0.05) > 1e-6 {
		t.Fatalf("chi2Survival(3.841459, 1) = %v, want ≈ 0.05", got)
	}
	// chi²=5.991465 with 2 df ≈ 0.05.
	if got := chi2Survival(5.991465, 2); math.Abs(got-0.05) > 1e-5 {
		t.Fatalf("chi2Survival(5.991465, 2) = %v, want ≈ 0.05", got)
	}
	// x=0 → p=1; invalid df → NaN.
	if chi2Survival(0, 3) != 1 || !math.IsNaN(chi2Survival(1, 0)) {
		t.Fatal("chi2Survival edge cases wrong")
	}
}

// ---------------------------------------------------------------------------
// SRM test (docs/技术方案.md §10.1: SRM 基于曝光单元数与预设分配比较).
// ---------------------------------------------------------------------------

func TestSRMBalancedHealthy(t *testing.T) {
	r, err := SRMTest([]int64{5000, 5000}, []float64{0.5, 0.5}, DefaultSRMThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.ChiSquare != 0 || math.Abs(r.PValue-1) > 1e-12 || !r.Healthy {
		t.Fatalf("perfect 50/50 split must give chi2=0, p≈1, healthy: %+v", r)
	}
	if len(r.Observed) != 2 || len(r.Expected) != 2 || r.Expected[0] != 5000 || r.Expected[1] != 5000 {
		t.Fatalf("Observed/Expected wrong: %+v", r)
	}
}

func TestSRMImbalancedUnhealthy(t *testing.T) {
	// 5200/4800 vs 50/50: chi2 = 200²/5000 + 200²/5000 = 16,
	// p(1 df) = 6.334e-05 < 0.001 → SRM alarm.
	r, err := SRMTest([]int64{5200, 4800}, []float64{0.5, 0.5}, DefaultSRMThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(r.ChiSquare-16) > 1e-9 {
		t.Fatalf("ChiSquare = %v, want 16", r.ChiSquare)
	}
	if math.Abs(r.PValue-6.3348e-05) > 1e-7 {
		t.Fatalf("PValue = %v, want ≈ 6.3348e-05 (abs tol 1e-7)", r.PValue)
	}
	if r.Healthy {
		t.Fatalf("chi2=16 (p≈6.3e-5) must alarm as unhealthy: %+v", r)
	}
}

func TestSRMUnequalAllocationHealthy(t *testing.T) {
	// Pre-registered 60/40 allocation, observed exactly 6000/4000 → healthy.
	r, err := SRMTest([]int64{6000, 4000}, []float64{0.6, 0.4}, DefaultSRMThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.ChiSquare > 1e-9 || math.Abs(r.PValue-1) > 1e-12 || !r.Healthy {
		t.Fatalf("exact 60/40 match must be healthy: %+v", r)
	}
	if r.Expected[0] != 6000 || r.Expected[1] != 4000 {
		t.Fatalf("expected counts must come from the pre-registered allocation: %v", r.Expected)
	}
}

func TestSRMThreeArms(t *testing.T) {
	// Perfect fit across three arms.
	r, err := SRMTest([]int64{1000, 1000, 1000}, []float64{1.0 / 3, 1.0 / 3, 1.0 / 3}, DefaultSRMThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.ChiSquare > 1e-9 || !r.Healthy {
		t.Fatalf("balanced three-arm split must be healthy: %+v", r)
	}
	// Mild deviation that stays healthy: 50/25/25 allocation, observed
	// 5050/2475/2475 → chi2 = 50²/5000 + 25²/2500 + 25²/2500 = 1.0,
	// p(2 df) = e^{−1/2} ≈ 0.6065.
	r, err = SRMTest([]int64{5050, 2475, 2475}, []float64{0.5, 0.25, 0.25}, DefaultSRMThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(r.ChiSquare-1.0) > 1e-9 {
		t.Fatalf("ChiSquare = %v, want 1.0", r.ChiSquare)
	}
	if want := math.Exp(-0.5); math.Abs(r.PValue-want) > 1e-9 {
		t.Fatalf("PValue = %v, want %v (chi²₂ survival is e^{−x/2})", r.PValue, want)
	}
	if !r.Healthy {
		t.Fatalf("mild deviation must stay healthy: %+v", r)
	}
}

func TestSRMThresholdParameter(t *testing.T) {
	// The threshold is a parameter: the same data can be healthy at 1e-6
	// and unhealthy at 1e-2.
	r1, err := SRMTest([]int64{5200, 4800}, []float64{0.5, 0.5}, 1e-6)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r2, err := SRMTest([]int64{5200, 4800}, []float64{0.5, 0.5}, 1e-2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r1.Healthy || r2.Healthy {
		t.Fatalf("threshold parameter not honored: p=%v healthy(1e-6)=%v healthy(1e-2)=%v", r1.PValue, r1.Healthy, r2.Healthy)
	}
}

func TestSRMErrors(t *testing.T) {
	cases := []struct {
		name        string
		observed    []int64
		proportions []float64
		threshold   float64
	}{
		{"mismatched lengths", []int64{100, 100}, []float64{0.5}, DefaultSRMThreshold},
		{"proportions sum to 0.9", []int64{50, 50}, []float64{0.45, 0.45}, DefaultSRMThreshold},
		{"proportions sum to 1.2", []int64{50, 50}, []float64{0.6, 0.6}, DefaultSRMThreshold},
		{"zero proportion", []int64{50, 50}, []float64{1.0, 0.0}, DefaultSRMThreshold},
		{"negative proportion", []int64{50, 50}, []float64{1.5, -0.5}, DefaultSRMThreshold},
		{"NaN proportion", []int64{50, 50}, []float64{math.NaN(), 1.0}, DefaultSRMThreshold},
		{"zero total", []int64{0, 0}, []float64{0.5, 0.5}, DefaultSRMThreshold},
		{"negative observed", []int64{-1, 100}, []float64{0.5, 0.5}, DefaultSRMThreshold},
		{"single variant", []int64{100}, []float64{1.0}, DefaultSRMThreshold},
		{"empty", nil, nil, DefaultSRMThreshold},
		{"threshold 0", []int64{50, 50}, []float64{0.5, 0.5}, 0},
		{"threshold 1", []int64{50, 50}, []float64{0.5, 0.5}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := SRMTest(tc.observed, tc.proportions, tc.threshold); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestSRMExposuresNotConversions documents the input contract of §10.1:
// SRM must be computed on exposed-unit counts against the PRE-REGISTERED
// allocation. Feeding conversion counts through the same math can manufacture
// a false alarm from the treatment effect itself; this test shows the API
// shape callers must use (units in, allocation in).
func TestSRMExposuresNotConversions(t *testing.T) {
	// Correct usage: enrolled/exposed units per variant vs pre-registered
	// 50/50 allocation.
	exposed := []int64{5000, 5000}
	preregistered := []float64{0.5, 0.5}
	r, err := SRMTest(exposed, preregistered, DefaultSRMThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Healthy {
		t.Fatalf("balanced exposures must be healthy: %+v", r)
	}
}
