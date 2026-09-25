package stats

import (
	"math"
	mrand "math/rand/v2"
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// Ratio metric (delta method): numerical validation against analytic solutions
// and Monte-Carlo ground truth, then fixed-seed simulation studies.
// ---------------------------------------------------------------------------

// ratioStatsOf builds RatioStats from explicit per-unit (x, y) pairs.
func ratioStatsOf(pairs ...[2]float64) RatioStats {
	var r RatioStats
	for _, p := range pairs {
		r.AddUnit(p[0], p[1])
	}
	return r
}

// TestRatioDeltaKnownValues pins the delta-method variance to a hand-computed
// example: units (1,1),(2,5),(3,3) give R = 9/6 = 1.5, and
//
//	s²_x = 1, s²_y = 4, s_xy = 1, x̄ = 2, N = 3
//	Var(R) = (s²_y − 2R·s_xy + R²·s²_x)/(N·x̄²) = (4 − 3 + 2.25)/12 = 3.25/12.
func TestRatioDeltaKnownValues(t *testing.T) {
	a := ratioStatsOf([2]float64{1, 1}, [2]float64{2, 5}, [2]float64{3, 3})
	if got := a.Ratio(); math.Abs(got-1.5) > 1e-12 {
		t.Fatalf("Ratio() = %v, want 1.5", got)
	}
	v := a.deltaMethodVariance()
	want := 3.25 / 12
	if math.Abs(v-want) > 1e-12 {
		t.Fatalf("deltaMethodVariance() = %v, want %v (3.25/12)", v, want)
	}
}

// TestRatioReducesToMeanWhenDenominatorConstant checks the delta-method
// degeneracy: when every denominator xᵢ ≡ 1 the ratio R equals the mean ȳ and
// Var(R) collapses to the mean-variance s²_y/N (so the ratio test reduces to
// the Welch/normal mean test). This is the analytic consistency requirement.
func TestRatioReducesToMeanWhenDenominatorConstant(t *testing.T) {
	ys := []float64{2, 4, 4, 4, 5, 5, 7, 9} // classic sd=2 example
	var r RatioStats
	var m MeanStats
	for _, y := range ys {
		r.AddUnit(1, y)
		m.N++
		m.Sum += y
		m.SumSquares += y * y
	}
	// R == mean.
	if math.Abs(r.Ratio()-m.Mean()) > 1e-12 {
		t.Fatalf("ratio %v != mean %v", r.Ratio(), m.Mean())
	}
	// Var(R) == s²_y/N == mean variance / N.
	want := m.Variance() / float64(m.N)
	if got := r.deltaMethodVariance(); math.Abs(got-want) > 1e-12 {
		t.Fatalf("delta variance %v != mean-variance/N %v", got, want)
	}
}

// TestRatioDeltaMethodVarianceMatchesMonteCarlo validates the analytic
// delta-method variance against the empirical sampling variance of R̂: draw many
// independent datasets from a fixed joint distribution, and compare the mean
// analytic Var(R̂) with the empirical variance of R̂ across datasets. The delta
// method is a first-order asymptotic expansion, so they should agree closely at
// moderate n (relative tolerance 15%). Fixed seed → deterministic.
func TestRatioDeltaMethodVarianceMatchesMonteCarlo(t *testing.T) {
	const (
		iters = 8000
		n     = 120
	)
	rng := mrand.New(mrand.NewPCG(101, 101))
	draw := func() RatioStats {
		var r RatioStats
		for i := 0; i < n; i++ {
			x := 1 + 2*rng.Float64() // Uniform(1,3), mean 2
			y := 0.5 + 0.8*x + 0.4*rng.NormFloat64()
			r.AddUnit(x, y)
		}
		return r
	}
	var ratios []float64
	var analyticSum float64
	for i := 0; i < iters; i++ {
		r := draw()
		ratios = append(ratios, r.Ratio())
		analyticSum += r.deltaMethodVariance()
	}
	mean := 0.0
	for _, v := range ratios {
		mean += v
	}
	mean /= float64(len(ratios))
	empVar := 0.0
	for _, v := range ratios {
		empVar += (v - mean) * (v - mean)
	}
	empVar /= float64(len(ratios) - 1)
	analytic := analyticSum / float64(len(ratios))
	rel := math.Abs(analytic-empVar) / empVar
	if rel > 0.15 {
		t.Fatalf("delta-method variance %v vs Monte-Carlo %v (rel err %v > 0.15)", analytic, empVar, rel)
	}
	t.Logf("ratio delta variance: analytic=%.6g MC=%.6g relerr=%.3f (n=%d, iters=%d, seed=101)",
		analytic, empVar, rel, n, iters)
}

// drawRatioArm streams n per-unit (x, y) pairs with x ~ Uniform(1,3) and
// y = base + slope·x + noise, so the true ratio is (base + 2·slope)/2.
func drawRatioArm(rng *mrand.Rand, n int, base, slope, noise float64) RatioStats {
	var r RatioStats
	for i := 0; i < n; i++ {
		x := 1 + 2*rng.Float64()
		y := base + slope*x + noise*rng.NormFloat64()
		r.AddUnit(x, y)
	}
	return r
}

func TestRatioSimulationTypeICoverageAndPower(t *testing.T) {
	const (
		iters   = 3000
		nPerArm = 200
		alpha   = 0.05
		slope   = 0.8
		noise   = 0.4
	)
	// True ratio R = (base + 2·slope)/2. Under H0 both arms share base=0.5.

	// --- H0: identical arms → Type-I error and CI coverage of true diff 0. ---
	rng := mrand.New(mrand.NewPCG(202, 202))
	rejections, covered := 0, 0
	for i := 0; i < iters; i++ {
		a := drawRatioArm(rng, nPerArm, 0.5, slope, noise)
		b := drawRatioArm(rng, nPerArm, 0.5, slope, noise)
		res, err := RatioDeltaTest(a, b, alpha)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if res.Significant {
			rejections++
		}
		if res.CIDiffLow <= 0 && 0 <= res.CIDiffHigh {
			covered++
		}
	}
	typeI := float64(rejections) / iters
	coverage := float64(covered) / iters
	if typeI < 0.03 || typeI > 0.08 {
		t.Fatalf("ratio Type-I error = %v (want [0.03, 0.08])", typeI)
	}
	if coverage < 0.93 || coverage > 0.97 {
		t.Fatalf("ratio 95%% CI coverage = %v (want [0.93, 0.97])", coverage)
	}
	t.Logf("ratio H0: Type-I=%.4f coverage=%.4f (iters=%d n=%d seed=202)", typeI, coverage, iters, nPerArm)

	// --- H1: treatment ratio shifted up by +0.10 → power. ---
	// ΔR = Δbase/2 = 0.10 → Δbase = 0.20.
	rng = mrand.New(mrand.NewPCG(203, 203))
	rejections = 0
	for i := 0; i < iters; i++ {
		a := drawRatioArm(rng, nPerArm, 0.5, slope, noise)
		b := drawRatioArm(rng, nPerArm, 0.7, slope, noise)
		res, err := RatioDeltaTest(a, b, alpha)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if res.Significant && res.Diff > 0 {
			rejections++
		}
	}
	power := float64(rejections) / iters
	if power < 0.7 {
		t.Fatalf("ratio power at ΔR=0.10 = %v (want > 0.7)", power)
	}
	t.Logf("ratio H1: power=%.4f (iters=%d ΔR=0.10 seed=203)", power, iters)
}

func TestRatioErrors(t *testing.T) {
	good := ratioStatsOf([2]float64{1, 1}, [2]float64{2, 3}, [2]float64{3, 2})
	tooSmall := ratioStatsOf([2]float64{1, 1})
	zeroDen := ratioStatsOf([2]float64{0, 1}, [2]float64{0, 2}, [2]float64{0, 3})
	nanAgg := RatioStats{N: 3, SumX: 6, SumY: math.NaN(), SumXX: 14, SumYY: 14, SumXY: 14}
	negXX := RatioStats{N: 3, SumX: 6, SumY: 6, SumXX: -14, SumYY: 14, SumXY: 14}

	cases := []struct {
		name    string
		a, b    RatioStats
		alpha   float64
		wantErr bool
	}{
		{"N<2 arm A", tooSmall, good, 0.05, true},
		{"N<2 arm B", good, tooSmall, 0.05, true},
		{"zero denominator arm A", zeroDen, good, 0.05, true},
		{"NaN aggregate", nanAgg, good, 0.05, true},
		{"negative SumXX", negXX, good, 0.05, true},
		{"alpha=0", good, good, 0, true},
		{"alpha=1", good, good, 1, true},
		{"valid", good, good, 0.05, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RatioDeltaTest(tc.a, tc.b, tc.alpha)
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
// Quantile metric: numerical validation (type-7 values, binomial CI, analytic
// asymptotic SE) then fixed-seed simulation (coverage / Type-I / power).
// ---------------------------------------------------------------------------

func TestSampleQuantileKnownValues(t *testing.T) {
	cases := []struct {
		name string
		data []float64
		p    float64
		want float64
	}{
		// type-7 values cross-checked against numpy.quantile(..., method='linear').
		{"median even", []float64{1, 2, 3, 4}, 0.5, 2.5},
		{"q1 even", []float64{1, 2, 3, 4}, 0.25, 1.75},
		{"q3 even", []float64{1, 2, 3, 4}, 0.75, 3.25},
		{"median odd", []float64{1, 2, 3, 4, 5}, 0.5, 3},
		{"q9 odd", []float64{1, 2, 3, 4, 5}, 0.9, 4.6},
		{"min", []float64{1, 2, 3}, 0, 1},
		{"max", []float64{1, 2, 3}, 1, 3},
		{"single", []float64{7}, 0.5, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SampleQuantile(tc.data, tc.p); math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("SampleQuantile(%v, %v) = %v, want %v", tc.data, tc.p, got, tc.want)
			}
		})
	}
	if !math.IsNaN(SampleQuantile(nil, 0.5)) {
		t.Fatal("empty data must give NaN")
	}
}

func TestBinomCDFKnownValues(t *testing.T) {
	// P(X <= 5; n=10, p=0.5) = 638/1024 = 0.623046875 (exact).
	if got := binomCDF(5, 10, 0.5); math.Abs(got-0.623046875) > 1e-9 {
		t.Fatalf("binomCDF(5,10,0.5) = %v, want 0.623046875", got)
	}
	// P(X <= 0; 10, 0.5) = 1/1024.
	if got := binomCDF(0, 10, 0.5); math.Abs(got-1.0/1024) > 1e-12 {
		t.Fatalf("binomCDF(0,10,0.5) = %v, want 1/1024", got)
	}
	// Edges.
	if got := binomCDF(-1, 10, 0.5); got != 0 {
		t.Fatalf("binomCDF(-1) = %v, want 0", got)
	}
	if got := binomCDF(10, 10, 0.5); got != 1 {
		t.Fatalf("binomCDF(10) = %v, want 1", got)
	}
	// Skewed: P(X <= 2; 10, 0.3) = 0.382782786 (independent reference).
	if got := binomCDF(2, 10, 0.3); math.Abs(got-0.382782786) > 1e-6 {
		t.Fatalf("binomCDF(2,10,0.3) = %v, want ≈ 0.382782786", got)
	}
}

// TestQuantileOrderStatisticCIKnown pins the distribution-free median CI to the
// classical sign-test result: for n=10, p=0.5, alpha=0.05 the exact 95% CI is
// [x_(2), x_(9)] (the two-sided sign test rejects when <=1 or >=9 of 10 fall on
// one side, since P(Binom(10,0.5) <= 1) = 0.0107 < 0.025).
func TestQuantileOrderStatisticCIKnown(t *testing.T) {
	l, u := quantileOrderStatisticCI(10, 0.5, 0.05)
	if l != 2 || u != 9 {
		t.Fatalf("median CI order stats for n=10 = [%d,%d], want [2,9]", l, u)
	}
	// Coverage guarantee: the binomial tails must each be <= alpha/2.
	n, p, alpha := 10, 0.5, 0.05
	if got := binomCDF(l-1, n, p); got > alpha/2+1e-12 {
		t.Fatalf("lower tail %v exceeds alpha/2", got)
	}
	if got := 1 - binomCDF(u-1, n, p); got > alpha/2+1e-12 {
		t.Fatalf("upper tail %v exceeds alpha/2", got)
	}
	// n=25 median 95% CI: L such that P(Binom<=L-1)<=0.025 → [8,18] (known).
	l, u = quantileOrderStatisticCI(25, 0.5, 0.05)
	if l != 8 || u != 18 {
		t.Fatalf("median CI order stats for n=25 = [%d,%d], want [8,18]", l, u)
	}
}

// TestQuantileAsymptoticSEMatchesAnalyticNormal validates the KDE-based
// asymptotic SE against the closed-form for the standard normal median:
// q_0.5 = 0, f(0) = 1/√(2π), so Var(q̂) = 0.25/(n·f²) = π/(2n) and
// SE = √(π/(2n)). On a large fixed-seed normal sample the KDE estimate must
// match within 12%.
func TestQuantileAsymptoticSEMatchesAnalyticNormal(t *testing.T) {
	const n = 20000
	rng := mrand.New(mrand.NewPCG(303, 303))
	data := make([]float64, n)
	for i := range data {
		data[i] = rng.NormFloat64()
	}
	sort.Float64s(data) // quantileAsymptoticSE expects ascending order
	got := quantileAsymptoticSE(data, 0.5)
	want := math.Sqrt(math.Pi / (2 * float64(n)))
	if math.Abs(got-want)/want > 0.12 {
		t.Fatalf("median asymptotic SE = %v, want √(π/2n) = %v (rel err %.3f)", got, want, math.Abs(got-want)/want)
	}
	t.Logf("quantile median SE: KDE=%.6f analytic=%.6f (n=%d seed=303)", got, want, n)
}

func TestQuantileSimulationCoverageTypeIPower(t *testing.T) {
	const (
		iters   = 2000
		nPerArm = 300
		alpha   = 0.05
		level   = 0.5 // median: the asymptotic test is best-calibrated here
	)

	// --- H0: identical N(0,1) arms. ---
	rng := mrand.New(mrand.NewPCG(404, 404))
	rejections, dfCovered, diffCovered := 0, 0, 0
	arm := func(mu float64) []float64 {
		x := make([]float64, nPerArm)
		for i := range x {
			x[i] = mu + rng.NormFloat64()
		}
		return x
	}
	for i := 0; i < iters; i++ {
		res, err := QuantileTest(arm(0), arm(0), level, alpha)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if res.Significant {
			rejections++
		}
		// Distribution-free CI for arm A must cover the true median 0.
		if res.CIALow <= 0 && 0 <= res.CIAHigh {
			dfCovered++
		}
		// Asymptotic CI for the difference must cover 0.
		if res.CIDiffLow <= 0 && 0 <= res.CIDiffHigh {
			diffCovered++
		}
	}
	typeI := float64(rejections) / iters
	dfCov := float64(dfCovered) / iters
	diffCov := float64(diffCovered) / iters
	if typeI < 0.03 || typeI > 0.08 {
		t.Fatalf("quantile Type-I error = %v (want [0.03, 0.08])", typeI)
	}
	if dfCov < 0.95 || dfCov > 0.999 {
		t.Fatalf("distribution-free median CI coverage = %v (want [0.95, 0.999], conservative)", dfCov)
	}
	if diffCov < 0.92 || diffCov > 0.97 {
		t.Fatalf("asymptotic diff CI coverage = %v (want [0.92, 0.97])", diffCov)
	}
	t.Logf("quantile H0: Type-I=%.4f dfCIcov=%.4f diffCIcov=%.4f (iters=%d n=%d seed=404)",
		typeI, dfCov, diffCov, iters, nPerArm)

	// --- H1: treatment median shifted +0.3 → power. ---
	rng = mrand.New(mrand.NewPCG(405, 405))
	rejections = 0
	for i := 0; i < iters; i++ {
		res, err := QuantileTest(arm(0), arm(0.3), level, alpha)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if res.Significant && res.Diff > 0 {
			rejections++
		}
	}
	power := float64(rejections) / iters
	if power < 0.7 {
		t.Fatalf("quantile power at median shift 0.3 = %v (want > 0.7)", power)
	}
	t.Logf("quantile H1: power=%.4f (iters=%d shift=0.3 seed=405)", power, iters)
}

func TestQuantileErrors(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	small := []float64{1}
	if _, err := QuantileTest(a, a, 0.5, 0); err == nil {
		t.Fatal("alpha=0 must error")
	}
	if _, err := QuantileTest(a, a, 0, 0.05); err == nil {
		t.Fatal("level=0 must error")
	}
	if _, err := QuantileTest(a, a, 1, 0.05); err == nil {
		t.Fatal("level=1 must error")
	}
	if _, err := QuantileTest(small, a, 0.5, 0.05); err == nil {
		t.Fatal("<2 units must error")
	}
	// Constant metric (zero density) must NOT error but must be non-significant.
	c := []float64{5, 5, 5, 5, 5}
	res, err := QuantileTest(c, c, 0.5, 0.05)
	if err != nil {
		t.Fatalf("constant metric unexpected error: %v", err)
	}
	if res.Significant || res.PValue != 1 {
		t.Fatalf("constant metric must be non-significant with p=1: %+v", res)
	}
}
