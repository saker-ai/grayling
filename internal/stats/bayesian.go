package stats

import "math"

// This file implements the BAYESIAN view of a two-arm comparison — posterior
// distribution, probability of superiority and expected loss — required by
// docs/技术方案.md §10.3 (贝叶斯方法). Per that section, Bayesian output is
// reported SEPARATELY from the frequentist fixed-sample decision and must never
// be blended into the same decision rule (不能将“默认 Bayesian”和“固定频率派
// 95% CI”混成同一决策规则). The frequentist gate in decision.go remains
// authoritative for ship/rollback; these structs are a parallel, clearly
// labelled decision-support view (e.g. "expected loss of choosing B").
//
// Two conjugate families are provided:
//   - Mean metrics: Normal likelihood with plug-in variance and a Normal prior
//     on each arm's mean → Normal posterior on the difference (closed form).
//   - Proportion metrics: Beta-Binomial conjugacy → exact Beta posteriors, with
//     P(B>A) and expected loss by 1-D quadrature over the Beta densities
//     (stdlib-only, reusing the regularized incomplete beta of welch.go).

// normalPDF is the standard normal density φ(x).
func normalPDF(x float64) float64 {
	return math.Exp(-0.5*x*x) / math.Sqrt(2*math.Pi)
}

// NormalPrior is a Normal(m0, sd²) prior on EACH arm's mean. A zero or
// negative SD selects a weakly-informative default (10× the pooled SD), which is
// diffuse enough that the posterior is data-dominated yet proper.
type NormalPrior struct {
	Mean float64
	SD   float64
}

// BayesianMeanResult is the Bayesian posterior summary for a two-arm mean
// comparison (arm B minus arm A). It is intentionally a distinct type from
// WelchResult so the two paradigms are never conflated (docs §10.3).
type BayesianMeanResult struct {
	// PostMeanDiff, PostSDDiff parameterize the Normal posterior of the true
	// difference μ_B − μ_A.
	PostMeanDiff float64 `json:"postMeanDiff"`
	PostSDDiff   float64 `json:"postSDDiff"`
	// CredibleLow, CredibleHigh bound the central (1−alpha) credible interval.
	CredibleLow  float64 `json:"credibleLow"`
	CredibleHigh float64 `json:"credibleHigh"`
	// ProbBSuperior = P(μ_B > μ_A | data), the probability of superiority.
	ProbBSuperior float64 `json:"probBSuperior"`
	// ExpectedLossB = E[max(0, μ_A − μ_B) | data]: the expected regret from
	// choosing B (0 when B is certainly at least as good). ExpectedLossA is the
	// symmetric loss from choosing A.
	ExpectedLossB float64 `json:"expectedLossB"`
	ExpectedLossA float64 `json:"expectedLossA"`
	// Alpha and per-arm counts are echoed for traceability.
	Alpha float64 `json:"alpha"`
	NA    int64   `json:"na"`
	NB    int64   `json:"nb"`
	// PriorSDUsed is the effective prior SD (the default when NormalPrior.SD<=0).
	PriorSDUsed float64 `json:"priorSDUsed"`
}

// normalExpectedLossPositive returns E[max(0, X)] for X ~ N(m, s²) in closed
// form: m·Φ(m/s) + s·φ(m/s). Validated against quadrature in bayesian_test.go.
func normalExpectedLossPositive(m, s float64) float64 {
	if s <= 0 {
		if m > 0 {
			return m
		}
		return 0
	}
	z := m / s
	return m*normalCDF(z) + s*normalPDF(z)
}

// BayesianMeanTest computes the Normal-Normal posterior of the two-arm mean
// difference and its decision-support summaries. The per-arm likelihood uses the
// plug-in variance σ̂² = s² (unbiased sample variance); a Normal prior on each
// arm mean gives a Normal posterior:
//
//	postVar = 1/(1/s0² + n/s²);  postMean = postVar·(m0/s0² + n·x̄/s²).
//
// The difference posterior is Normal(postMean_B − postMean_A, postVar_A +
// postVar_B) (independent arms). Errors: N < 2, non-finite/negative aggregates,
// alpha outside (0,1), or zero pooled variance with a non-positive prior SD
// (the weakly-informative default would be undefined).
func BayesianMeanTest(a, b MeanStats, alpha float64, prior NormalPrior) (BayesianMeanResult, error) {
	var res BayesianMeanResult
	if err := validateAlpha(alpha); err != nil {
		return res, err
	}
	if err := a.validate("arm A"); err != nil {
		return res, err
	}
	if err := b.validate("arm B"); err != nil {
		return res, err
	}
	if a.N < 2 || b.N < 2 {
		return res, &statsError{"Bayesian mean test requires N >= 2 in both arms"}
	}

	// Weakly-informative default prior SD: 10× pooled SD.
	priorSD := prior.SD
	if priorSD <= 0 {
		pooled := a.Merge(b)
		pooledSD := math.Sqrt(pooled.Variance())
		if !(pooledSD > 0) {
			return res, &statsError{"Bayesian mean test: zero variance and no explicit prior SD"}
		}
		priorSD = 10 * pooledSD
	}
	res.PriorSDUsed = priorSD
	res.Alpha = alpha
	res.NA, res.NB = a.N, b.N

	post := func(m MeanStats) (mean, variance float64) {
		n := float64(m.N)
		s2 := m.Variance()
		if s2 <= 0 {
			// Degenerate arm: no likelihood information → posterior = prior.
			return prior.Mean, priorSD * priorSD
		}
		prec := 1/(priorSD*priorSD) + n/s2
		variance = 1 / prec
		mean = variance * (prior.Mean/(priorSD*priorSD) + n*m.Mean()/s2)
		return mean, variance
	}
	mA, vA := post(a)
	mB, vB := post(b)

	res.PostMeanDiff = mB - mA
	res.PostSDDiff = math.Sqrt(vA + vB)

	zq := normalQuantile(1 - alpha/2)
	res.CredibleLow = res.PostMeanDiff - zq*res.PostSDDiff
	res.CredibleHigh = res.PostMeanDiff + zq*res.PostSDDiff

	// P(μ_B > μ_A) = P(diff > 0) with diff ~ N(PostMeanDiff, PostSDDiff²).
	res.ProbBSuperior = normalCDF(res.PostMeanDiff / res.PostSDDiff)
	// Loss of choosing B = E[max(0, μ_A − μ_B)]; (μ_A − μ_B) ~ N(−diff, sd²).
	res.ExpectedLossB = normalExpectedLossPositive(-res.PostMeanDiff, res.PostSDDiff)
	// Loss of choosing A = E[max(0, μ_B − μ_A)]; (μ_B − μ_A) ~ N(diff, sd²).
	res.ExpectedLossA = normalExpectedLossPositive(res.PostMeanDiff, res.PostSDDiff)
	return res, nil
}

// BetaPrior is a Beta(alpha0, beta0) prior on a conversion proportion. Zero
// values select the uniform Beta(1,1) prior.
type BetaPrior struct {
	Alpha float64
	Beta  float64
}

func (p BetaPrior) resolve() (a0, b0 float64) {
	a0, b0 = p.Alpha, p.Beta
	if a0 <= 0 {
		a0 = 1
	}
	if b0 <= 0 {
		b0 = 1
	}
	return a0, b0
}

// BayesianProportionResult is the Beta-Binomial posterior summary for a two-arm
// proportion comparison, reported separately from ProportionResult (docs §10.3).
type BayesianProportionResult struct {
	// Posterior Beta parameters per arm: p_A ~ Beta(PostAA, PostAB), etc.
	PostAA float64 `json:"postAA"`
	PostAB float64 `json:"postAB"`
	PostBA float64 `json:"postBA"`
	PostBB float64 `json:"postBB"`
	// Posterior means of each arm's conversion rate.
	PostMeanA float64 `json:"postMeanA"`
	PostMeanB float64 `json:"postMeanB"`
	// ProbBSuperior = P(p_B > p_A | data) by quadrature over the posteriors.
	ProbBSuperior float64 `json:"probBSuperior"`
	// ExpectedLossB = E[max(0, p_A − p_B) | data]; ExpectedLossA symmetric.
	ExpectedLossB float64 `json:"expectedLossB"`
	ExpectedLossA float64 `json:"expectedLossA"`
	// Credible interval for the difference p_B − p_A (normal approximation to
	// the posterior difference, equal-tailed).
	CredibleLowDiff  float64 `json:"credibleLowDiff"`
	CredibleHighDiff float64 `json:"credibleHighDiff"`
	Alpha            float64 `json:"alpha"`
	NA               int64   `json:"na"`
	NB               int64   `json:"nb"`
	XA               int64   `json:"xa"`
	XB               int64   `json:"xb"`
}

// lgBetaNorm returns Lgamma(a+b) − Lgamma(a) − Lgamma(b), the log of the Beta
// density normalizer 1/B(a,b).
func lgBetaNorm(a, b float64) float64 {
	lab, _ := math.Lgamma(a + b)
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	return lab - la - lb
}

// betaPDF returns the Beta(a,b) density at x in (0,1); 0 outside.
func betaPDF(x, a, b float64) float64 {
	if x <= 0 || x >= 1 {
		return 0
	}
	return math.Exp((a-1)*math.Log(x) + (b-1)*math.Log1p(-x) + lgBetaNorm(a, b))
}

// betaPosteriorSummaries computes P(p_B > p_A) and the expected losses by 1-D
// quadrature over a fine grid on (0,1), using the exact Beta CDF (ibeta) for the
// inner terms. Deterministic and stdlib-only.
//
//	P(p_B>p_A)        = ∫ f_A(u)·(1 − F_B(u)) du
//	E[max(0,p_A−p_B)] = ∫ f_A(u)·[ u·F_B(u) − (a_B/(a_B+b_B))·I_u(a_B+1,b_B) ] du
//
// and the symmetric expression for E[max(0,p_B−p_A)]. The Beta density
// normalizers are hoisted out of the loop for speed.
func betaPosteriorSummaries(aA, bA, aB, bB float64) (probBSup, lossB, lossA float64) {
	const grid = 1500
	du := 1.0 / float64(grid)
	lgA, lgB := lgBetaNorm(aA, bA), lgBetaNorm(aB, bB)
	pdf := func(x, a, b, lg float64) float64 {
		if x <= 0 || x >= 1 {
			return 0
		}
		return math.Exp((a-1)*math.Log(x) + (b-1)*math.Log1p(-x) + lg)
	}
	meanFactorB := aB / (aB + bB)
	meanFactorA := aA / (aA + bA)
	// Integrate over u = p_A for P(B>A) and lossB.
	for i := 1; i < grid; i++ {
		u := float64(i) * du
		fA := pdf(u, aA, bA, lgA)
		if fA == 0 {
			continue
		}
		FB := ibeta(u, aB, bB)
		w := fA * du
		probBSup += w * (1 - FB)
		partialMeanB := meanFactorB * ibeta(u, aB+1, bB)
		lossB += w * (u*FB - partialMeanB)
	}
	// Integrate over v = p_B for lossA (symmetric).
	for i := 1; i < grid; i++ {
		v := float64(i) * du
		fB := pdf(v, aB, bB, lgB)
		if fB == 0 {
			continue
		}
		FA := ibeta(v, aA, bA)
		w := fB * du
		partialMeanA := meanFactorA * ibeta(v, aA+1, bA)
		lossA += w * (v*FA - partialMeanA)
	}
	return probBSup, lossB, lossA
}

// BayesianProportionTest computes the Beta-Binomial posterior for a two-arm
// conversion comparison. With prior Beta(a0,b0) and x conversions in n trials,
// the posterior is exactly Beta(a0+x, b0+n−x). P(B>A) and expected losses come
// from quadrature (see betaPosteriorSummaries). The difference credible interval
// is found by inverting the posterior-difference CDF on a grid.
//
// Errors: N < 1, conversions outside [0,N], alpha outside (0,1).
func BayesianProportionTest(a, b ProportionStats, alpha float64, prior BetaPrior) (BayesianProportionResult, error) {
	var res BayesianProportionResult
	if err := validateAlpha(alpha); err != nil {
		return res, err
	}
	if err := a.validate("arm A"); err != nil {
		return res, err
	}
	if err := b.validate("arm B"); err != nil {
		return res, err
	}
	a0, b0 := prior.resolve()
	res.PostAA = a0 + float64(a.Conversions)
	res.PostAB = b0 + float64(a.N-a.Conversions)
	res.PostBA = a0 + float64(b.Conversions)
	res.PostBB = b0 + float64(b.N-b.Conversions)
	res.PostMeanA = res.PostAA / (res.PostAA + res.PostAB)
	res.PostMeanB = res.PostBA / (res.PostBA + res.PostBB)
	res.Alpha = alpha
	res.NA, res.NB = a.N, b.N
	res.XA, res.XB = a.Conversions, b.Conversions

	res.ProbBSuperior, res.ExpectedLossB, res.ExpectedLossA =
		betaPosteriorSummaries(res.PostAA, res.PostAB, res.PostBA, res.PostBB)
	res.CredibleLowDiff, res.CredibleHighDiff = betaDifferenceCredible(res.PostAA, res.PostAB, res.PostBA, res.PostBB, alpha)
	return res, nil
}

// betaDifferenceCredible returns the central (1−alpha) equal-tailed credible
// interval for p_B − p_A under independent Beta posteriors, using the standard
// normal approximation to each Beta posterior: p ~ Beta(a,b) ≈ N(a/(a+b),
// ab/((a+b)²(a+b+1))), so the difference ≈ Normal(meanB−meanA, varA+varB). This
// is fast and accurate at experiment scale (n in the hundreds+); the exact
// P(B>A) and expected losses — the primary Bayesian decision quantities — are
// computed by quadrature in betaPosteriorSummaries instead. Deterministic.
func betaDifferenceCredible(aA, bA, aB, bB, alpha float64) (lo, hi float64) {
	sA := aA + bA
	sB := aB + bB
	mA := aA / sA
	mB := aB / sB
	vA := aA * bA / (sA * sA * (sA + 1))
	vB := aB * bB / (sB * sB * (sB + 1))
	d := mB - mA
	sd := math.Sqrt(vA + vB)
	z := normalQuantile(1 - alpha/2)
	return d - z*sd, d + z*sd
}
