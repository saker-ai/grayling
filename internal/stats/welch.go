package stats

import "math"

// This file implements Welch's two-sample t-test for mean metrics
// (docs/技术方案.md §10.3: 均值用 Welch 检验和效应置信区间) together with the
// special functions it needs — the regularized incomplete beta function, the
// Student-t CDF and its inverse — all stdlib-only, following the
// Numerical-Recipes continued-fraction method (betacf + ibeta).

// relativeEps is the guard for "baseline is near zero": when |MeanA| <=
// relativeEps the relative difference is not reported (docs/技术方案.md
// §10.3: 基线接近零时只报绝对差).
const relativeEps = 1e-12

// WelchResult is the outcome of a two-sided Welch t-test comparing two
// variant means (arm B minus arm A). Implements docs/技术方案.md §10.3
// reporting requirements: absolute and relative difference, sample sizes,
// effect confidence interval and significance at the pre-registered alpha.
type WelchResult struct {
	// MeanA, MeanB are the sample means of each arm.
	MeanA float64 `json:"meanA"`
	MeanB float64 `json:"meanB"`
	// Diff is MeanB − MeanA (absolute difference).
	Diff float64 `json:"diff"`
	// RelativeDiff is Diff/MeanA when the baseline is meaningful, else 0.
	RelativeDiff float64 `json:"relativeDiff"`
	// VarA, VarB are the unbiased sample variances of each arm.
	VarA float64 `json:"varA"`
	VarB float64 `json:"varB"`
	// NA, NB are the per-arm unit counts.
	NA int64 `json:"na"`
	NB int64 `json:"nb"`
	// T is the Welch t statistic; DF the Welch–Satterthwaite degrees of
	// freedom. In the degenerate zero-variance case T may be ±Inf (see
	// WelchTTest); DF is reported as 0 when undefined.
	T  float64 `json:"t"`
	DF float64 `json:"df"`
	// PValue is the two-sided p-value.
	PValue float64 `json:"pValue"`
	// CIDiffLow, CIDiffHigh bound the 1−alpha Welch confidence interval for
	// the true difference in means.
	CIDiffLow  float64 `json:"ciDiffLow"`
	CIDiffHigh float64 `json:"ciDiffHigh"`
	// Significant is PValue < Alpha.
	Significant bool `json:"significant"`
	// RelativeMeaningful is false when |MeanA| <= relativeEps, in which case
	// RelativeDiff is 0 and only the absolute difference should be reported
	// (docs/技术方案.md §10.3).
	RelativeMeaningful bool `json:"relativeMeaningful"`
}

// WelchTTest performs a two-sided Welch t-test (unequal variances) from the
// sufficient statistics of two variants, with a 1−alpha Welch confidence
// interval for the mean difference. Implements docs/技术方案.md §10.3.
//
// The sample variance is computed as (SumSquares − Sum²/N)/(N−1); the
// Welch–Satterthwaite degrees of freedom as
//
//	df = (va/na + vb/nb)² / ( (va/na)²/(na−1) + (vb/nb)²/(nb−1) ).
//
// Degenerate inputs have documented, deterministic behavior:
//   - N < 2 in either arm, NaN/Inf aggregates, negative SumSquares or an
//     alpha outside (0,1): an error is returned.
//   - Zero variance in both arms with equal means: p = 1 (never NaN),
//     T = 0, DF = 0 (undefined, reported as 0), CI = [0, 0], not significant.
//   - Zero variance in both arms with different means: the difference is
//     exact, so p = 0, Significant = true, T = ±Inf, DF = 0 and the CI
//     collapses to [Diff, Diff]. Note: with truly zero within-arm variance
//     any nonzero difference is "significant" by construction; callers should
//     sanity-check that zero variance is real and not an aggregation bug.
//
// FIXED-SAMPLE SEMANTICS (docs/技术方案.md §10.3): this p-value is valid for
// a single evaluation after the pre-registered analysis window closes.
// Repeatedly recomputing it mid-flight and stopping at the first significant
// reading inflates the Type-I error rate and must not be used to declare
// victory early.
func WelchTTest(a, b MeanStats, alpha float64) (WelchResult, error) {
	var res WelchResult
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
		return res, &statsError{"Welch t-test requires N >= 2 in both arms"}
	}

	na, nb := float64(a.N), float64(b.N)
	res.NA, res.NB = a.N, b.N
	res.MeanA = a.Sum / na
	res.MeanB = b.Sum / nb
	res.Diff = res.MeanB - res.MeanA

	if res.RelativeDiff, res.RelativeMeaningful = relativeDiff(res.Diff, res.MeanA); !res.RelativeMeaningful {
		res.RelativeDiff = 0
	}

	// Unbiased sample variances with cancellation clamping.
	ssA := a.SumSquares - a.Sum*a.Sum/na
	ssB := b.SumSquares - b.Sum*b.Sum/nb
	tol := 1e-9 * math.Max(math.Abs(a.SumSquares), math.Abs(b.SumSquares))
	if ssA < -tol || ssB < -tol {
		return res, &statsError{"SumSquares inconsistent with Sum (negative sum of squared deviations beyond float tolerance)"}
	}
	if ssA < 0 {
		ssA = 0
	}
	if ssB < 0 {
		ssB = 0
	}
	res.VarA = ssA / (na - 1)
	res.VarB = ssB / (nb - 1)

	// Mean-square errors of each arm's mean.
	mseA := res.VarA / na
	mseB := res.VarB / nb
	se2 := mseA + mseB
	se := math.Sqrt(se2)

	if se2 == 0 {
		// Both arms have exactly zero variance: the difference is known
		// exactly. See the documented degenerate behavior above.
		res.DF = 0 // undefined; reported as 0
		res.CIDiffLow, res.CIDiffHigh = res.Diff, res.Diff
		if res.Diff == 0 {
			res.T = 0
			res.PValue = 1
			res.Significant = false
		} else {
			res.T = math.Copysign(math.Inf(1), res.Diff)
			res.PValue = 0
			res.Significant = true
		}
		return res, nil
	}

	// Welch–Satterthwaite degrees of freedom. With se2 > 0 and N >= 2 in
	// both arms, df >= min(na−1, nb−1) >= 1, so the t CDF/quantile below
	// are always well defined.
	den := mseA*mseA/(na-1) + mseB*mseB/(nb-1)
	res.DF = se2 * se2 / den
	res.T = res.Diff / se

	// Two-sided p-value: P(|T_df| >= |t|) = I_{df/(df+t²)}(df/2, 1/2).
	t2 := res.T * res.T
	res.PValue = ibeta(res.DF/(res.DF+t2), res.DF/2, 0.5)
	res.PValue = clamp01(res.PValue)

	// Welch CI at 1−alpha.
	halfWidth := tQuantile(1-alpha/2, res.DF) * se
	res.CIDiffLow = res.Diff - halfWidth
	res.CIDiffHigh = res.Diff + halfWidth

	res.Significant = res.PValue < alpha
	return res, nil
}

// relativeDiff computes the relative difference against the baseline,
// guarding the near-zero baseline case (docs/技术方案.md §10.3: 基线接近零时
// 只报绝对差).
func relativeDiff(diff, baseline float64) (rel float64, meaningful bool) {
	if !isFinite(baseline) || math.Abs(baseline) <= relativeEps {
		return 0, false
	}
	return diff / baseline, true
}

func clamp01(x float64) float64 {
	switch {
	case math.IsNaN(x):
		return 0
	case x < 0:
		return 0
	case x > 1:
		return 1
	}
	return x
}

// betacf evaluates the continued fraction for the incomplete beta function by
// the modified Lentz method (Numerical Recipes, "betacf"). Called only with
// x in (0,1) and a, b > 0 from ibeta.
func betacf(a, b, x float64) float64 {
	const (
		maxIter = 300
		eps     = 3e-16
		fpmin   = 1e-300
	)
	qab := a + b
	qap := a + 1
	qam := a - 1
	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < fpmin {
		d = fpmin
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		m2 := float64(2 * m)
		// Even step.
		aa := float64(m) * (b - float64(m)) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		h *= d * c
		// Odd step.
		aa = -(a + float64(m)) * (qab + float64(m)) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}

// ibeta returns the regularized incomplete beta function I_x(a, b) using the
// Numerical-Recipes combination of betacf and the log-beta front factor, with
// the standard symmetry switch for numerical stability. Requires a, b > 0;
// x is clamped to [0, 1].
func ibeta(x, a, b float64) float64 {
	if a <= 0 || b <= 0 || math.IsNaN(x) || math.IsNaN(a) || math.IsNaN(b) {
		return math.NaN()
	}
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	lbeta, _ := math.Lgamma(a + b)
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	front := math.Exp(lbeta - la - lb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betacf(a, b, x) / a
	}
	return 1 - front*betacf(b, a, 1-x)/b
}

// tCDF returns the CDF P(T_df <= x) of the Student-t distribution. The tail
// is computed directly as 0.5·I_{df/(df+x²)}(df/2, 1/2) so that extreme
// quantiles keep precision (no catastrophic 1−p cancellation). Requires
// df >= 1.
func tCDF(x, df float64) float64 {
	if math.IsNaN(x) || df <= 0 {
		return math.NaN()
	}
	if x == 0 {
		return 0.5
	}
	if math.IsInf(x, 0) {
		if x > 0 {
			return 1
		}
		return 0
	}
	tail := 0.5 * ibeta(df/(df+x*x), df/2, 0.5)
	if x > 0 {
		return 1 - tail
	}
	return tail
}

// tQuantile returns the inverse Student-t CDF (the value x with
// P(T_df <= x) = p) found by bisection on tCDF. It is used only to build
// fixed-precision confidence intervals, where bisection's ~1e-12 resolution
// is ample; it is deliberately simple and robust rather than fast.
// Requires p in (0,1) and df >= 1; results are clamped to the bracket.
func tQuantile(p, df float64) float64 {
	if math.IsNaN(p) || p <= 0 || p >= 1 || df <= 0 {
		return math.NaN()
	}
	lo, hi := -1e15, 1e15
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		if mid <= lo || mid >= hi {
			break // hit float64 resolution
		}
		if tCDF(mid, df) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// normalCDF returns the standard normal CDF Φ(x), via the complementary
// error function for tail accuracy.
func normalCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// normalQuantile returns the inverse standard normal CDF by bisection on
// normalCDF, used for proportion confidence intervals. Requires p in (0,1).
func normalQuantile(p float64) float64 {
	if math.IsNaN(p) || p <= 0 || p >= 1 {
		return math.NaN()
	}
	lo, hi := -1e15, 1e15
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		if mid <= lo || mid >= hi {
			break
		}
		if normalCDF(mid) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}
