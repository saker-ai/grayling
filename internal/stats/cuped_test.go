package stats

import (
	"math"
	mrand "math/rand/v2"
	"testing"
)

// pairedOf builds PairedStats from explicit (covariate X, outcome Y) pairs.
func pairedOf(pairs ...[2]float64) PairedStats {
	var p PairedStats
	for _, q := range pairs {
		p.AddPair(q[0], q[1])
	}
	return p
}

// TestCUPEDThetaAndPerfectCorrelation pins θ = Cov/Var and ρ to a
// hand-computed example where Y = 2X exactly: pooled θ = 2, ρ = 1, and the
// adjusted per-arm variance collapses to 0 (the covariate perfectly predicts
// the outcome), so VarianceReduction ≈ 1.
//
//	Pooled X=1..6, Y=2X: sxx=3.5, sxy=7, syy=14 → θ=7/3.5=2, ρ=7/√(3.5·14)=1.
func TestCUPEDThetaAndPerfectCorrelation(t *testing.T) {
	a := pairedOf([2]float64{1, 2}, [2]float64{2, 4}, [2]float64{3, 6})
	b := pairedOf([2]float64{4, 8}, [2]float64{5, 10}, [2]float64{6, 12})
	res, err := CUPEDTest(a, b, 0.05)
	if err != nil {
		t.Fatalf("CUPEDTest: %v", err)
	}
	if math.Abs(res.Theta-2) > 1e-9 {
		t.Fatalf("Theta = %v, want 2", res.Theta)
	}
	if math.Abs(res.Correlation-1) > 1e-9 {
		t.Fatalf("Correlation = %v, want 1", res.Correlation)
	}
	if res.VarA > 1e-9 || res.VarB > 1e-9 {
		t.Fatalf("adjusted variances must → 0 for perfect correlation: A=%v B=%v", res.VarA, res.VarB)
	}
	if math.Abs(res.VarianceReduction-1) > 1e-6 {
		t.Fatalf("VarianceReduction = %v, want ≈ 1", res.VarianceReduction)
	}
}

// TestCUPEDReducesToWelchWhenCovariateConstant verifies the documented
// degeneracy: a constant covariate gives Var(X)=0 → θ=0, so CUPED must equal
// the unadjusted Welch t-test on the outcome Y (same diff, same p-value, zero
// variance reduction).
func TestCUPEDReducesToWelchWhenCovariateConstant(t *testing.T) {
	ys := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	ysB := []float64{3, 5, 6, 6, 7, 8, 9, 12}
	var a, b PairedStats
	var ma, mb MeanStats
	for _, y := range ys {
		a.AddPair(7, y) // constant covariate
		ma.N++
		ma.Sum += y
		ma.SumSquares += y * y
	}
	for _, y := range ysB {
		b.AddPair(7, y)
		mb.N++
		mb.Sum += y
		mb.SumSquares += y * y
	}
	cup, err := CUPEDTest(a, b, 0.05)
	if err != nil {
		t.Fatalf("CUPEDTest: %v", err)
	}
	w, err := WelchTTest(ma, mb, 0.05)
	if err != nil {
		t.Fatalf("WelchTTest: %v", err)
	}
	if cup.Theta != 0 {
		t.Fatalf("Theta = %v, want 0 for constant covariate", cup.Theta)
	}
	if cup.VarianceReduction != 0 {
		t.Fatalf("VarianceReduction = %v, want 0", cup.VarianceReduction)
	}
	if math.Abs(cup.Diff-w.Diff) > 1e-9 {
		t.Fatalf("CUPED diff %v != Welch diff %v", cup.Diff, w.Diff)
	}
	if math.Abs(cup.PValue-w.PValue) > 1e-9 {
		t.Fatalf("CUPED p %v != Welch p %v", cup.PValue, w.PValue)
	}
	if math.Abs(cup.T-w.T) > 1e-9 {
		t.Fatalf("CUPED T %v != Welch T %v", cup.T, w.T)
	}
}

// TestCUPEDUncorrelatedNoReduction: an orthogonal covariate/outcome gives
// θ ≈ 0 and no variance reduction (the analytic ρ=0 case).
func TestCUPEDUncorrelatedNoReduction(t *testing.T) {
	// X and Y constructed to be uncorrelated: Σ(X−X̄)(Y−Ȳ) = 0.
	a := pairedOf([2]float64{-1, -1}, [2]float64{1, 1}, [2]float64{-1, 1}, [2]float64{1, -1})
	b := pairedOf([2]float64{-2, 0}, [2]float64{2, 0}, [2]float64{-2, 2}, [2]float64{2, 2})
	res, err := CUPEDTest(a, b, 0.05)
	if err != nil {
		t.Fatalf("CUPEDTest: %v", err)
	}
	if math.Abs(res.Theta) > 1e-9 {
		t.Fatalf("Theta = %v, want ≈ 0 for uncorrelated covariate", res.Theta)
	}
	if math.Abs(res.Correlation) > 1e-9 {
		t.Fatalf("Correlation = %v, want ≈ 0", res.Correlation)
	}
	if res.VarianceReduction > 1e-9 {
		t.Fatalf("VarianceReduction = %v, want ≈ 0", res.VarianceReduction)
	}
}

// drawPairedArm streams n units with covariate X ~ N(0,1) and outcome
// Y = mu + c·X + sd·N(0,1). corr(X,Y) = c/√(c²+sd²); the CUPED variance
// reduction should approach ρ² = c²/(c²+sd²).
func drawPairedArm(rng *mrand.Rand, n int, mu, c, sd float64) PairedStats {
	var p PairedStats
	for i := 0; i < n; i++ {
		x := rng.NormFloat64()
		y := mu + c*x + sd*rng.NormFloat64()
		p.AddPair(x, y)
	}
	return p
}

// TestCUPEDSimulationUnbiasedReductionCoverage is the fixed-seed validation:
// (1) Δ_adj is unbiased for the true effect, (2) the achieved variance
// reduction matches the analytic ρ², and (3) the adjusted CI covers the true
// effect at ~95%.
func TestCUPEDSimulationUnbiasedReductionCoverage(t *testing.T) {
	const (
		iters   = 3000
		nPerArm = 100
		alpha   = 0.05
		effect  = 0.2
		c       = 1.0 // covariate slope
		sd      = 1.0 // residual noise → ρ² = c²/(c²+sd²) = 0.5
	)
	wantReduction := c * c / (c*c + sd*sd) // 0.5

	rng := mrand.New(mrand.NewPCG(505, 505))
	var sumAdj, sumRaw float64
	var adjVals, rawVals []float64
	covered := 0
	for i := 0; i < iters; i++ {
		a := drawPairedArm(rng, nPerArm, 0, c, sd)
		b := drawPairedArm(rng, nPerArm, effect, c, sd)
		res, err := CUPEDTest(a, b, alpha)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		sumAdj += res.Diff
		sumRaw += res.RawDiff
		adjVals = append(adjVals, res.Diff)
		rawVals = append(rawVals, res.RawDiff)
		if res.CIDiffLow <= effect && effect <= res.CIDiffHigh {
			covered++
		}
	}

	// (1) Unbiasedness of the adjusted estimator.
	meanAdj := sumAdj / float64(iters)
	if math.Abs(meanAdj-effect) > 0.05 {
		t.Fatalf("CUPED mean Δ_adj = %v, want ≈ true effect %v (bias > 0.05)", meanAdj, effect)
	}

	// (2) Empirical variance reduction Var(Δ_adj)/Var(Δ_raw) ≈ 1 − ρ².
	varOf := func(v []float64) float64 {
		m := 0.0
		for _, x := range v {
			m += x
		}
		m /= float64(len(v))
		s := 0.0
		for _, x := range v {
			s += (x - m) * (x - m)
		}
		return s / float64(len(v)-1)
	}
	empReduction := 1 - varOf(adjVals)/varOf(rawVals)
	if math.Abs(empReduction-wantReduction) > 0.12 {
		t.Fatalf("empirical variance reduction = %v, want ≈ ρ² = %v (±0.12)", empReduction, wantReduction)
	}
	if varOf(adjVals) >= varOf(rawVals) {
		t.Fatalf("CUPED must reduce variance: adj=%v raw=%v", varOf(adjVals), varOf(rawVals))
	}

	// (3) Coverage of the true effect by the adjusted CI.
	coverage := float64(covered) / float64(iters)
	if coverage < 0.93 || coverage > 0.97 {
		t.Fatalf("CUPED 95%% CI coverage = %v (want [0.93, 0.97])", coverage)
	}
	t.Logf("CUPED: bias=%.4f empReduction=%.3f (want ρ²=%.2f) coverage=%.4f VarAdj=%.5f VarRaw=%.5f (iters=%d n=%d seed=505)",
		meanAdj-effect, empReduction, wantReduction, coverage, varOf(adjVals), varOf(rawVals), iters, nPerArm)
}

func TestCUPEDErrors(t *testing.T) {
	good := pairedOf([2]float64{1, 1}, [2]float64{2, 3}, [2]float64{3, 2})
	tooSmall := pairedOf([2]float64{1, 1})
	nanAgg := PairedStats{N: 3, SumX: 6, SumY: math.NaN(), SumXX: 14, SumYY: 14, SumXY: 14}
	negXX := PairedStats{N: 3, SumX: 6, SumY: 6, SumXX: -14, SumYY: 14, SumXY: 14}
	cases := []struct {
		name    string
		a, b    PairedStats
		alpha   float64
		wantErr bool
	}{
		{"N<2 arm A", tooSmall, good, 0.05, true},
		{"N<2 arm B", good, tooSmall, 0.05, true},
		{"NaN aggregate", nanAgg, good, 0.05, true},
		{"negative SumXX", negXX, good, 0.05, true},
		{"alpha=0", good, good, 0, true},
		{"alpha=1", good, good, 1, true},
		{"valid", good, good, 0.05, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CUPEDTest(tc.a, tc.b, tc.alpha)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
