package stats

import (
	"math"
	mrand "math/rand/v2"
	"testing"
)

// ---------------------------------------------------------------------------
// Internal special-function tests: regularized incomplete beta, Student-t CDF
// and quantile (docs/技术方案.md §10.3 numerics; stdlib-only implementation).
// ---------------------------------------------------------------------------

func TestIBetaKnownValues(t *testing.T) {
	cases := []struct {
		name    string
		x, a, b float64
		want    float64
		tol     float64
	}{
		{"uniform midpoint", 0.5, 1, 1, 0.5, 1e-12},
		{"closed form I_0.5(1,3)=1-(1-0.5)^3", 0.5, 1, 3, 0.875, 1e-12},
		{"closed form I_0.25(1,2)=1-(0.75)^2", 0.25, 1, 2, 0.4375, 1e-12},
		{"symmetric a=b", 0.5, 4, 4, 0.5, 1e-12},
		{"x=0 edge", 0, 2, 3, 0, 0},
		{"x=1 edge", 1, 2, 3, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ibeta(tc.x, tc.a, tc.b)
			if math.Abs(got-tc.want) > tc.tol {
				t.Fatalf("ibeta(%v, %v, %v) = %v, want %v (tol %v)", tc.x, tc.a, tc.b, got, tc.want, tc.tol)
			}
		})
	}
}

func TestIBetaSymmetryIdentity(t *testing.T) {
	// I_x(a,b) = 1 − I_{1−x}(b,a) for a range of arguments.
	for _, a := range []float64{0.5, 1, 2.5, 7} {
		for _, b := range []float64{0.5, 1, 3, 12.25} {
			for _, x := range []float64{0.05, 0.3, 0.5, 0.8, 0.97} {
				lhs := ibeta(x, a, b)
				rhs := 1 - ibeta(1-x, b, a)
				if math.Abs(lhs-rhs) > 1e-9 {
					t.Fatalf("symmetry violated at a=%v b=%v x=%v: %v vs %v", a, b, x, lhs, rhs)
				}
			}
		}
	}
}

func TestStudentTCDFKnownValues(t *testing.T) {
	// Reference value computed independently by high-precision numeric
	// integration of the t density: CDF(2.228, df=10) = 0.97499411.
	// Docs/tables quote ≈ 0.975 (tolerance 1e-3 per acceptance spec).
	got := tCDF(2.228, 10)
	if math.Abs(got-0.975) > 1e-3 {
		t.Fatalf("tCDF(2.228, 10) = %v, want ≈ 0.975", got)
	}
	if math.Abs(got-0.97499411) > 1e-6 {
		t.Fatalf("tCDF(2.228, 10) = %v, want 0.97499411 ± 1e-6", got)
	}
	if c := tCDF(0, 10); c != 0.5 {
		t.Fatalf("tCDF(0, 10) = %v, want exactly 0.5", c)
	}
	// Antisymmetry: CDF(−x) = 1 − CDF(x).
	for _, x := range []float64{0.3, 1.0, 2.228, 5.5} {
		for _, df := range []float64{1, 3, 10, 88.5} {
			if lo, hi := tCDF(-x, df), 1-tCDF(x, df); math.Abs(lo-hi) > 1e-12 {
				t.Fatalf("antisymmetry violated at x=%v df=%v: %v vs %v", x, df, lo, hi)
			}
		}
	}
	// Tail sanity: normal limit at huge df.
	if c := tCDF(1.959963985, 1e7); math.Abs(c-0.975) > 1e-4 {
		t.Fatalf("tCDF(1.96, df=1e7) = %v, want ≈ 0.975", c)
	}
}

func TestTQuantileKnownValues(t *testing.T) {
	// qt(0.975, 10) ≈ 2.228 (independent reference: 2.22813885).
	got := tQuantile(0.975, 10)
	if math.Abs(got-2.228) > 1e-3 {
		t.Fatalf("tQuantile(0.975, 10) = %v, want ≈ 2.228", got)
	}
	if math.Abs(got-2.22813885) > 1e-5 {
		t.Fatalf("tQuantile(0.975, 10) = %v, want 2.22813885 ± 1e-5", got)
	}
	// qt(0.975, 8) ≈ 2.306004 (used by the CI check below).
	if q := tQuantile(0.975, 8); math.Abs(q-2.306004) > 1e-4 {
		t.Fatalf("tQuantile(0.975, 8) = %v, want ≈ 2.306004", q)
	}
	// Normal limit.
	if q := tQuantile(0.975, 1e6); math.Abs(q-1.959964) > 1e-3 {
		t.Fatalf("tQuantile(0.975, 1e6) = %v, want ≈ 1.96", q)
	}
	// Round trip: CDF(quantile(p)) == p.
	for _, df := range []float64{1, 2, 10, 100} {
		for _, p := range []float64{0.9, 0.95, 0.975, 0.999} {
			if c := tCDF(tQuantile(p, df), df); math.Abs(c-p) > 1e-9 {
				t.Fatalf("round trip failed at p=%v df=%v: CDF(q) = %v", p, df, c)
			}
		}
	}
}

func TestNormalQuantile(t *testing.T) {
	if q := normalQuantile(0.975); math.Abs(q-1.959963985) > 1e-6 {
		t.Fatalf("normalQuantile(0.975) = %v, want 1.959964", q)
	}
	if q := normalQuantile(0.5); math.Abs(q) > 1e-9 {
		t.Fatalf("normalQuantile(0.5) = %v, want 0", q)
	}
}

// ---------------------------------------------------------------------------
// Welch t-test: known-value checks.
// ---------------------------------------------------------------------------

// meanStatsOf builds MeanStats from explicit samples (test helper only).
func meanStatsOf(xs ...float64) MeanStats {
	var m MeanStats
	for _, x := range xs {
		m.N++
		m.Sum += x
		m.SumSquares += x * x
	}
	return m
}

func TestWelchKnownSmallSamples(t *testing.T) {
	// A = {1,2,3,4,5}, B = {6,7,8,9,10}: hand-computable exact values.
	// meanA=3, meanB=8, varA=varB=2.5, SE=1, Diff=5, T=5,
	// Welch df = (0.5+0.5)²/(0.25/4+0.25/4) = 8.
	// Two-sided p (independent reference via numeric integration of the t
	// density): 0.00105282579.
	a := meanStatsOf(1, 2, 3, 4, 5)
	b := meanStatsOf(6, 7, 8, 9, 10)
	r, err := WelchTTest(a, b, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.MeanA != 3 || r.MeanB != 8 || r.Diff != 5 {
		t.Fatalf("means/diff wrong: %+v", r)
	}
	if r.VarA != 2.5 || r.VarB != 2.5 {
		t.Fatalf("variances wrong: %v %v", r.VarA, r.VarB)
	}
	if math.Abs(r.T-5) > 1e-12 {
		t.Fatalf("T = %v, want 5", r.T)
	}
	if math.Abs(r.DF-8) > 1e-9 {
		t.Fatalf("DF = %v, want 8", r.DF)
	}
	if math.Abs(r.PValue-0.00105282579) > 1e-8 {
		t.Fatalf("PValue = %v, want 0.00105282579 ± 1e-8", r.PValue)
	}
	if !r.Significant {
		t.Fatal("expected significant at alpha=0.05")
	}
	// CI = 5 ± t_{0.975,8}·1 = 5 ± 2.306004.
	if math.Abs(r.CIDiffLow-(5-2.306004)) > 1e-3 || math.Abs(r.CIDiffHigh-(5+2.306004)) > 1e-3 {
		t.Fatalf("CI = [%v, %v], want ≈ [2.694, 7.306]", r.CIDiffLow, r.CIDiffHigh)
	}
	// Relative diff vs baseline meanA=3.
	if !r.RelativeMeaningful || math.Abs(r.RelativeDiff-5.0/3.0) > 1e-12 {
		t.Fatalf("RelativeDiff = %v (meaningful=%v), want 5/3", r.RelativeDiff, r.RelativeMeaningful)
	}
}

func TestWelchIdenticalSamplesPApproachesOne(t *testing.T) {
	a := meanStatsOf(1, 2, 3, 4, 5)
	r, err := WelchTTest(a, a, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(r.PValue-1) > 1e-9 {
		t.Fatalf("PValue = %v, want ≈ 1 for identical arms", r.PValue)
	}
	if r.Significant || r.T != 0 || r.Diff != 0 {
		t.Fatalf("identical arms must give T=0, Diff=0, not significant: %+v", r)
	}
}

func TestWelchObviousSeparationPHuge(t *testing.T) {
	// 30 deterministic samples in [0,1) vs the same shifted by +100:
	// overwhelming separation, p must be < 1e-6.
	var a, b MeanStats
	for i := 0; i < 30; i++ {
		x := float64(i) / 30
		a.N++
		a.Sum += x
		a.SumSquares += x * x
		y := x + 100
		b.N++
		b.Sum += y
		b.SumSquares += y * y
	}
	r, err := WelchTTest(a, b, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.PValue >= 1e-6 {
		t.Fatalf("PValue = %v, want < 1e-6 for obviously separated arms", r.PValue)
	}
	if !r.Significant || r.Diff <= 0 {
		t.Fatalf("expected significant positive diff: %+v", r)
	}
}

func TestWelchDegenerateZeroVariance(t *testing.T) {
	// Both arms constant and EQUAL → p = 1, never NaN.
	a := MeanStats{N: 5, Sum: 25, SumSquares: 125} // all values 5
	r, err := WelchTTest(a, a, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.PValue != 1 || r.Significant || r.T != 0 || r.DF != 0 {
		t.Fatalf("equal constants: want p=1, not significant, T=0, DF=0; got %+v", r)
	}
	if r.CIDiffLow != 0 || r.CIDiffHigh != 0 {
		t.Fatalf("equal constants: CI must be [0,0], got [%v,%v]", r.CIDiffLow, r.CIDiffHigh)
	}

	// Both arms constant but DIFFERENT → p = 0, significant (documented).
	b := MeanStats{N: 5, Sum: 35, SumSquares: 245} // all values 7
	r, err = WelchTTest(a, b, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.PValue != 0 || !r.Significant || !math.IsInf(r.T, 1) || r.Diff != 2 {
		t.Fatalf("unequal constants: want p=0, significant, T=+Inf, Diff=2; got %+v", r)
	}
	if r.CIDiffLow != 2 || r.CIDiffHigh != 2 {
		t.Fatalf("unequal constants: CI must be [2,2], got [%v,%v]", r.CIDiffLow, r.CIDiffHigh)
	}
}

func TestWelchNearZeroBaselineReportsAbsoluteOnly(t *testing.T) {
	// meanA = 0 exactly → relative diff is meaningless (docs §10.3:
	// 基线接近零时只报绝对差).
	a := meanStatsOf(-1, 0, 1)           // mean 0, var 1
	b := meanStatsOf(-1.5, 0.5, 1, 2, 3) // mean 1
	r, err := WelchTTest(a, b, 0.05)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.RelativeMeaningful {
		t.Fatal("RelativeMeaningful must be false when |MeanA| <= 1e-12")
	}
	if r.RelativeDiff != 0 {
		t.Fatalf("RelativeDiff = %v, want 0 when baseline near zero", r.RelativeDiff)
	}
	if math.Abs(r.Diff-r.MeanB) > 1e-12 {
		t.Fatalf("absolute diff must still be reported: %+v", r)
	}
}

func TestWelchErrors(t *testing.T) {
	good := MeanStats{N: 5, Sum: 10, SumSquares: 30}
	tooSmall := MeanStats{N: 1, Sum: 1, SumSquares: 1}
	nan := MeanStats{N: 5, Sum: math.NaN(), SumSquares: 30}
	inf := MeanStats{N: 5, Sum: 10, SumSquares: math.Inf(1)}
	negSS := MeanStats{N: 5, Sum: 10, SumSquares: -30}

	cases := []struct {
		name    string
		a, b    MeanStats
		alpha   float64
		wantErr bool
	}{
		{"N<2 arm A", tooSmall, good, 0.05, true},
		{"N<2 arm B", good, tooSmall, 0.05, true},
		{"NaN sum", nan, good, 0.05, true},
		{"Inf sum squares", good, inf, 0.05, true},
		{"negative sum squares", negSS, good, 0.05, true},
		{"alpha=0", good, good, 0, true},
		{"alpha=1", good, good, 1, true},
		{"alpha=NaN", good, good, math.NaN(), true},
		{"alpha=-0.1", good, good, -0.1, true},
		{"valid", good, good, 0.05, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := WelchTTest(tc.a, tc.b, tc.alpha)
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
// Welch simulation studies (docs P2 acceptance: 模拟数据检验 I 型错误与区间
// 覆盖). All simulations use fixed seeds — fully deterministic.
// ---------------------------------------------------------------------------

// drawArm consumes n standard-normal-ish draws from rng, shifted by mu, and
// accumulates sufficient statistics.
func drawArm(rng *mrand.Rand, n int, mu float64) MeanStats {
	var m MeanStats
	for i := 0; i < n; i++ {
		x := mu + rng.NormFloat64()
		m.N++
		m.Sum += x
		m.SumSquares += x * x
	}
	return m
}

func TestWelchSimulationTypeICoverageAndPower(t *testing.T) {
	const (
		iters      = 2000
		nPerArm    = 40
		alpha      = 0.05
		trueDiffH0 = 0.0
		trueDiffH1 = 0.8
	)

	// --- H0: N(0,1) vs N(0,1) → Type-I error and CI coverage. ---
	rng := mrand.New(mrand.NewPCG(42, 42))
	rejections, covered := 0, 0
	for i := 0; i < iters; i++ {
		a := drawArm(rng, nPerArm, 0)
		b := drawArm(rng, nPerArm, 0)
		r, err := WelchTTest(a, b, alpha)
		if err != nil {
			t.Fatalf("iter %d: unexpected error: %v", i, err)
		}
		if r.Significant {
			rejections++
		}
		if r.CIDiffLow <= trueDiffH0 && trueDiffH0 <= r.CIDiffHigh {
			covered++
		}
	}
	typeI := float64(rejections) / iters
	coverage := float64(covered) / iters
	if typeI < 0.03 || typeI > 0.08 {
		t.Fatalf("Type-I error rate = %v (want in [0.03, 0.08])", typeI)
	}
	if coverage < 0.93 || coverage > 0.97 {
		t.Fatalf("95%% CI coverage of true diff 0 = %v (want in [0.93, 0.97])", coverage)
	}
	t.Logf("Welch H0: Type-I=%.4f coverage=%.4f (2000 iters, n=40/arm, seed=42)", typeI, coverage)

	// --- H1: N(0,1) vs N(0.8,1) → power. ---
	rng = mrand.New(mrand.NewPCG(43, 43))
	rejections = 0
	for i := 0; i < iters; i++ {
		a := drawArm(rng, nPerArm, 0)
		b := drawArm(rng, nPerArm, trueDiffH1)
		r, err := WelchTTest(a, b, alpha)
		if err != nil {
			t.Fatalf("iter %d: unexpected error: %v", i, err)
		}
		if r.Significant {
			rejections++
		}
	}
	power := float64(rejections) / iters
	if power <= 0.7 {
		t.Fatalf("power at d=0.8, n=40/arm = %v (want > 0.7)", power)
	}
	t.Logf("Welch H1: power=%.4f (2000 iters, d=0.8, seed=43)", power)
}
