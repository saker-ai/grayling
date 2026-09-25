package stats

import (
	"math"
	mrand "math/rand/v2"
	"testing"
)

// ---------------------------------------------------------------------------
// Bayesian: numerical validation (closed-form posterior & expected loss vs
// quadrature, exact Beta conjugacy, brute-force 2-D cross-check) then
// fixed-seed calibration/coverage simulations. Reported SEPARATELY from the
// frequentist decision (docs/技术方案.md §10.3).
// ---------------------------------------------------------------------------

// numericExpectedLossPositive integrates E[max(0,X)] for X~N(m,s²) directly,
// to validate the closed form normalExpectedLossPositive.
func numericExpectedLossPositive(m, s float64) float64 {
	hi := m + 12*s
	if hi < 12*s {
		hi = 12 * s
	}
	lo := 0.0
	const steps = 200000
	h := (hi - lo) / float64(steps)
	sum := 0.0
	for i := 0; i <= steps; i++ {
		x := lo + float64(i)*h
		pdf := normalPDF((x-m)/s) / s
		w := h
		if i == 0 || i == steps {
			w = h / 2
		}
		if x > 0 {
			sum += w * x * pdf
		}
	}
	return sum
}

func TestNormalExpectedLossClosedForm(t *testing.T) {
	cases := []struct{ m, s float64 }{
		{0, 1}, {1, 1}, {-1, 1}, {0.5, 0.3}, {2, 0.5}, {-0.2, 1.5},
	}
	for _, tc := range cases {
		closed := normalExpectedLossPositive(tc.m, tc.s)
		num := numericExpectedLossPositive(tc.m, tc.s)
		if math.Abs(closed-num) > 1e-5 {
			t.Fatalf("E[max(0,X)] mismatch at m=%v s=%v: closed=%v numeric=%v", tc.m, tc.s, closed, num)
		}
	}
	t.Logf("normal expected-loss closed form matches quadrature to <1e-5")
}

// TestBayesianMeanPosteriorKnownValues pins the Normal-Normal posterior to a
// hand-computed case: n=100/arm, x̄_A=5, x̄_B=6, s²=4, prior N(0,10²).
//
//	prec = 1/100 + 100/4 = 25.01 → postVar = 0.0399840, postSD(diff)=0.28279
//	postMean_A = 4.99800, postMean_B = 5.99760 → diff = 0.99960
//	P(B>A) = Φ(0.99960/0.28279) = 0.99980
func TestBayesianMeanPosteriorKnownValues(t *testing.T) {
	a := MeanStats{N: 100, Sum: 500, SumSquares: 2896} // mean 5, var 4
	b := MeanStats{N: 100, Sum: 600, SumSquares: 3996} // mean 6, var 4
	res, err := BayesianMeanTest(a, b, 0.05, NormalPrior{Mean: 0, SD: 10})
	if err != nil {
		t.Fatalf("BayesianMeanTest: %v", err)
	}
	if math.Abs(res.PostMeanDiff-0.99960) > 1e-3 {
		t.Fatalf("PostMeanDiff = %v, want 0.99960", res.PostMeanDiff)
	}
	if math.Abs(res.PostSDDiff-0.28279) > 1e-3 {
		t.Fatalf("PostSDDiff = %v, want 0.28279", res.PostSDDiff)
	}
	if math.Abs(res.ProbBSuperior-0.99980) > 1e-3 {
		t.Fatalf("ProbBSuperior = %v, want 0.99980", res.ProbBSuperior)
	}
	// Expected loss of choosing the clearly-better B must be ~0.
	if res.ExpectedLossB > 1e-3 {
		t.Fatalf("ExpectedLossB = %v, want ≈ 0 (B clearly superior)", res.ExpectedLossB)
	}
	if res.PriorSDUsed != 10 {
		t.Fatalf("PriorSDUsed = %v, want explicit 10", res.PriorSDUsed)
	}
}

func TestBayesianProportionConjugacyKnown(t *testing.T) {
	// Uniform prior Beta(1,1); A: 30/100, B: 45/100 → Beta(31,71), Beta(46,56).
	a := ProportionStats{N: 100, Conversions: 30}
	b := ProportionStats{N: 100, Conversions: 45}
	res, err := BayesianProportionTest(a, b, 0.05, BetaPrior{})
	if err != nil {
		t.Fatalf("BayesianProportionTest: %v", err)
	}
	if res.PostAA != 31 || res.PostAB != 71 || res.PostBA != 46 || res.PostBB != 56 {
		t.Fatalf("posterior Beta params wrong: %+v", res)
	}
	if math.Abs(res.PostMeanA-31.0/102) > 1e-9 || math.Abs(res.PostMeanB-46.0/102) > 1e-9 {
		t.Fatalf("posterior means wrong: A=%v B=%v", res.PostMeanA, res.PostMeanB)
	}
	// B (45%) beats A (30%) at n=100 → P(B>A) ≈ Φ(2.20) ≈ 0.985 (not extreme:
	// only 100 trials per arm), and a small expected loss for choosing B.
	if res.ProbBSuperior < 0.97 || res.ProbBSuperior > 0.995 {
		t.Fatalf("ProbBSuperior = %v, want ≈ 0.985 (in [0.97,0.995])", res.ProbBSuperior)
	}
	if res.ExpectedLossB > 0.002 {
		t.Fatalf("ExpectedLossB = %v, want small (< 0.002)", res.ExpectedLossB)
	}
	if res.CredibleLowDiff <= 0 || res.CredibleHighDiff <= res.CredibleLowDiff {
		t.Fatalf("difference CI must be entirely positive (B clearly better): [%v,%v]",
			res.CredibleLowDiff, res.CredibleHighDiff)
	}
}

func TestBayesianProportionSymmetry(t *testing.T) {
	a := ProportionStats{N: 200, Conversions: 60}
	res, err := BayesianProportionTest(a, a, 0.05, BetaPrior{})
	if err != nil {
		t.Fatalf("BayesianProportionTest: %v", err)
	}
	if math.Abs(res.ProbBSuperior-0.5) > 1e-6 {
		t.Fatalf("identical arms must give P(B>A)=0.5, got %v", res.ProbBSuperior)
	}
	if math.Abs(res.ExpectedLossA-res.ExpectedLossB) > 1e-6 {
		t.Fatalf("identical arms must have equal expected losses: A=%v B=%v", res.ExpectedLossA, res.ExpectedLossB)
	}
	if math.Abs(res.CredibleLowDiff+res.CredibleHighDiff) > 5e-3 {
		t.Fatalf("identical arms must give a difference CI symmetric about 0: [%v,%v]",
			res.CredibleLowDiff, res.CredibleHighDiff)
	}
}

// lgammaSum returns Lgamma(a+b) − Lgamma(a) − Lgamma(b) (the log Beta density
// normalizer).
func lgammaSum(a, b float64) float64 {
	lab, _ := math.Lgamma(a + b)
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	return lab - la - lb
}

// gammaSample draws from Gamma(shape, 1) by Marsaglia–Tsang (shape >= 1, which
// always holds for our posteriors: prior >= 1 plus a non-negative count).
func gammaSample(rng *mrand.Rand, shape float64) float64 {
	if shape < 1 {
		// Boost: Gamma(a) = Gamma(a+1)·U^(1/a).
		return gammaSample(rng, shape+1) * math.Pow(rng.Float64(), 1/shape)
	}
	d := shape - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)
	for {
		x := rng.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rng.Float64()
		if u < 1-0.0331*x*x*x*x {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}

// mcBetaSummaries is an INDEPENDENT Monte-Carlo reference for P(p_B>p_A) and the
// expected losses, sampling both Beta posteriors as Gamma ratios. Used to
// validate the deterministic quadrature in betaPosteriorSummaries.
func mcBetaSummaries(rng *mrand.Rand, samples int, aA, bA, aB, bB float64) (probB, lossB, lossA float64) {
	for i := 0; i < samples; i++ {
		pA := gammaSample(rng, aA)
		pA = pA / (pA + gammaSample(rng, bA))
		pB := gammaSample(rng, aB)
		pB = pB / (pB + gammaSample(rng, bB))
		if pB > pA {
			probB++
			lossA += pB - pA
		} else {
			lossB += pA - pB
		}
	}
	n := float64(samples)
	return probB / n, lossB / n, lossA / n
}

func TestBetaSummariesMatchesMonteCarlo(t *testing.T) {
	cases := [][4]float64{
		{31, 71, 46, 56},
		{10, 10, 12, 8},
		{120, 80, 90, 110},
		{15, 85, 22, 78},
	}
	rng := mrand.New(mrand.NewPCG(1234, 1234))
	for _, c := range cases {
		p1, l1b, l1a := betaPosteriorSummaries(c[0], c[1], c[2], c[3])
		p2, l2b, l2a := mcBetaSummaries(rng, 400000, c[0], c[1], c[2], c[3])
		if math.Abs(p1-p2) > 5e-3 {
			t.Fatalf("P(B>A) mismatch %+v: quadrature=%v MC=%v", c, p1, p2)
		}
		if math.Abs(l1b-l2b) > 5e-3 || math.Abs(l1a-l2a) > 5e-3 {
			t.Fatalf("expected-loss mismatch %+v: quadrature=(%v,%v) MC=(%v,%v)", c, l1b, l1a, l2b, l2a)
		}
	}
	t.Logf("Beta posterior summaries match independent Monte-Carlo (400k samples) to <5e-3")
}

func TestBayesianMeanSimulationCoverageAndCalibration(t *testing.T) {
	const (
		iters   = 3000
		nPerArm = 100
		alpha   = 0.05
	)
	rng := mrand.New(mrand.NewPCG(909, 909))
	covered := 0
	// Calibration: bucket by ProbBSuperior, compare to empirical truth (d>0).
	var probSum, trueSum float64
	for i := 0; i < iters; i++ {
		trueDiff := 0.5 * rng.NormFloat64() // true effect drawn from a diffuse prior
		var a, b MeanStats
		for j := 0; j < nPerArm; j++ {
			x := rng.NormFloat64()
			a.N++
			a.Sum += x
			a.SumSquares += x * x
			y := trueDiff + rng.NormFloat64()
			b.N++
			b.Sum += y
			b.SumSquares += y * y
		}
		res, err := BayesianMeanTest(a, b, alpha, NormalPrior{}) // weakly-informative default
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if res.CredibleLow <= trueDiff && trueDiff <= res.CredibleHigh {
			covered++
		}
		probSum += res.ProbBSuperior
		if trueDiff > 0 {
			trueSum++
		}
	}
	coverage := float64(covered) / float64(iters)
	if coverage < 0.93 || coverage > 0.97 {
		t.Fatalf("Bayesian mean 95%% credible coverage = %v (want [0.93,0.97])", coverage)
	}
	// E[P(B>A)] must ≈ P(true d > 0) (posterior probability is calibrated in the
	// mean under the generative prior).
	meanProb := probSum / float64(iters)
	trueFrac := trueSum / float64(iters)
	if math.Abs(meanProb-trueFrac) > 0.05 {
		t.Fatalf("calibration: mean P(B>A)=%v vs P(true d>0)=%v (drift > 0.05)", meanProb, trueFrac)
	}
	t.Logf("Bayesian mean: credible coverage=%.4f  E[P(B>A)]=%.4f vs P(d>0)=%.4f (iters=%d seed=909)",
		coverage, meanProb, trueFrac, iters)
}

func TestBayesianProportionSimulationCoverage(t *testing.T) {
	const (
		iters   = 2000
		nPerArm = 300
		alpha   = 0.05
	)
	rng := mrand.New(mrand.NewPCG(910, 910))
	covered := 0
	for i := 0; i < iters; i++ {
		pA := 0.1 + 0.1*rng.Float64() // true rates in [0.1, 0.2]
		pB := 0.1 + 0.1*rng.Float64()
		trueDiff := pB - pA
		xA := binomialDraw(rng, nPerArm, pA)
		xB := binomialDraw(rng, nPerArm, pB)
		res, err := BayesianProportionTest(
			ProportionStats{N: nPerArm, Conversions: int64(xA)},
			ProportionStats{N: nPerArm, Conversions: int64(xB)},
			alpha, BetaPrior{})
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if res.CredibleLowDiff <= trueDiff && trueDiff <= res.CredibleHighDiff {
			covered++
		}
	}
	coverage := float64(covered) / float64(iters)
	if coverage < 0.92 || coverage > 0.98 {
		t.Fatalf("Bayesian proportion difference 95%% credible coverage = %v (want [0.92,0.98])", coverage)
	}
	t.Logf("Bayesian proportion: difference credible coverage=%.4f (iters=%d n=%d seed=910)", coverage, iters, nPerArm)
}

// binomialDraw returns a Binomial(n,p) sample by counting Bernoulli trials
// (test helper; deterministic under a fixed-seed rng).
func binomialDraw(rng *mrand.Rand, n int, p float64) int {
	k := 0
	for i := 0; i < n; i++ {
		if rng.Float64() < p {
			k++
		}
	}
	return k
}

func TestBayesianErrors(t *testing.T) {
	good := MeanStats{N: 5, Sum: 10, SumSquares: 30}
	small := MeanStats{N: 1, Sum: 1, SumSquares: 1}
	if _, err := BayesianMeanTest(small, good, 0.05, NormalPrior{}); err == nil {
		t.Fatal("N<2 must error")
	}
	if _, err := BayesianMeanTest(good, good, 0, NormalPrior{}); err == nil {
		t.Fatal("alpha=0 must error")
	}
	pgood := ProportionStats{N: 10, Conversions: 3}
	if _, err := BayesianProportionTest(pgood, pgood, 1.5, BetaPrior{}); err == nil {
		t.Fatal("alpha>1 must error")
	}
	if _, err := BayesianProportionTest(ProportionStats{N: 5, Conversions: 9}, pgood, 0.05, BetaPrior{}); err == nil {
		t.Fatal("conversions > N must error")
	}
}
