package stats

import "math"

// This file implements Sample Ratio Mismatch (SRM) detection
// (docs/技术方案.md §10.1: SRM 基于进入实验且真实曝光的单元数，并与预设分配
// 比较；看不到完整曝光时不能用转化数代替).

// DefaultSRMThreshold is the conventional SRM alarm threshold: a healthy
// traffic split should essentially never produce a chi-square p-value this
// small, so p < 0.001 is treated as an SRM alarm (docs/技术方案.md §10.3
// requires SRM to be reported alongside the experiment result).
const DefaultSRMThreshold = 0.001

// srmProportionTolerance is the accepted deviation of the pre-registered
// allocation proportions from summing to exactly 1.
const srmProportionTolerance = 1e-6

// SRMResult is the outcome of a chi-square goodness-of-fit test of observed
// per-variant unit counts against the pre-registered allocation.
type SRMResult struct {
	// ChiSquare is the goodness-of-fit statistic Σ (O−E)²/E.
	ChiSquare float64 `json:"chiSquare"`
	// PValue is the chi-square survival probability with len(Observed)−1
	// degrees of freedom.
	PValue float64 `json:"pValue"`
	// Healthy is PValue >= threshold. An unhealthy result means the observed
	// split is inconsistent with the pre-registered allocation (SRM) and no
	// winner may be declared until the cause is understood.
	Healthy bool `json:"healthy"`
	// Observed and Expected are the per-variant counts used by the test,
	// aligned by index. Expected derives from the pre-registered allocation
	// applied to the observed total.
	Observed []float64 `json:"observed"`
	Expected []float64 `json:"expected"`
}

// SRMTest checks the observed per-variant counts of ENROLLED/EXPOSED
// RANDOMIZATION UNITS against the pre-registered allocation proportions using
// a chi-square goodness-of-fit test, and alarms when the p-value falls below
// threshold (conventionally DefaultSRMThreshold = 0.001).
//
// CRITICAL INPUT CONTRACT (docs/技术方案.md §10.1):
//   - observed MUST be counts of units that entered the experiment and were
//     truly exposed, grouped per variant. It must NEVER be conversion counts
//     (or any other metric event count) used as a stand-in for exposures:
//     conversion-based SRM is biased by the very treatment effect being
//     measured and invalidates the test.
//   - expectedProportions MUST be the PRE-REGISTERED allocation from the
//     experiment definition (e.g. [0.5, 0.5]), not proportions back-derived
//     from the observed data — comparing data to itself can never detect a
//     mismatch.
//
// Errors are returned when: the slice lengths differ; any observed count is
// negative; the total is zero; any expected proportion is <= 0 or non-finite;
// or the proportions do not sum to 1 within 1e-6.
//
// Statistical caveat: the chi-square approximation assumes expected counts
// are large enough (rule of thumb: every E >= 5); with tiny expected cells
// the p-value is only indicative. Healthy = PValue >= threshold.
func SRMTest(observed []int64, expectedProportions []float64, threshold float64) (SRMResult, error) {
	var res SRMResult
	if len(observed) != len(expectedProportions) {
		return res, &statsError{"SRM: observed and expectedProportions must have equal length"}
	}
	if len(observed) < 2 {
		return res, &statsError{"SRM: at least two variants are required"}
	}
	if threshold <= 0 || threshold >= 1 {
		return res, &statsError{"SRM: threshold must be in (0, 1)"}
	}

	var total int64
	var sumProp float64
	for i := range observed {
		if observed[i] < 0 {
			return res, &statsError{"SRM: observed counts must be non-negative"}
		}
		p := expectedProportions[i]
		if !isFinite(p) || p <= 0 {
			return res, &statsError{"SRM: expected proportions must be finite and > 0"}
		}
		total += observed[i]
		sumProp += p
	}
	if total == 0 {
		return res, &statsError{"SRM: total observed count is zero; no exposure data"}
	}
	if math.Abs(sumProp-1) > srmProportionTolerance {
		return res, &statsError{"SRM: expected proportions must sum to 1 (within 1e-6)"}
	}

	totalF := float64(total)
	res.Observed = make([]float64, len(observed))
	res.Expected = make([]float64, len(observed))
	chi2 := 0.0
	for i := range observed {
		res.Observed[i] = float64(observed[i])
		// Expected counts come from the pre-registered allocation applied to
		// the observed total (docs/技术方案.md §10.1).
		res.Expected[i] = totalF * expectedProportions[i]
		d := res.Observed[i] - res.Expected[i]
		chi2 += d * d / res.Expected[i]
	}
	res.ChiSquare = chi2
	res.PValue = clamp01(chi2Survival(chi2, float64(len(observed)-1)))
	res.Healthy = res.PValue >= threshold
	return res, nil
}

// chi2Survival returns P(X >= x) for X ~ chi-square with df degrees of
// freedom, via the upper regularized incomplete gamma Q(df/2, x/2).
func chi2Survival(x, df float64) float64 {
	if df <= 0 {
		return math.NaN()
	}
	if x <= 0 {
		return 1
	}
	return gammaQ(df/2, x/2)
}

// gammaQ returns the upper regularized incomplete gamma function
// Q(a, x) = 1 − P(a, x) = Γ(a, x)/Γ(a) for a > 0, x >= 0, using the
// Numerical-Recipes pair: the series representation for x < a+1 (returning
// 1 − P) and the continued fraction otherwise. Stdlib-only.
func gammaQ(a, x float64) float64 {
	if math.IsNaN(a) || math.IsNaN(x) || a <= 0 || x < 0 {
		return math.NaN()
	}
	if x == 0 {
		return 1
	}
	if x < a+1 {
		return 1 - gammaPSeries(a, x)
	}
	return gammaQCF(a, x)
}

// gammaPSeries returns the lower regularized incomplete gamma P(a, x) by its
// series representation (Numerical Recipes "gser"), valid for x < a+1.
func gammaPSeries(a, x float64) float64 {
	const (
		maxIter = 500
		eps     = 3e-16
	)
	ap := a
	sum := 1 / a
	del := sum
	for n := 0; n < maxIter; n++ {
		ap++
		del *= x / ap
		sum += del
		if math.Abs(del) < math.Abs(sum)*eps {
			break
		}
	}
	la, _ := math.Lgamma(a)
	return sum * math.Exp(-x+a*math.Log(x)-la)
}

// gammaQCF returns the upper regularized incomplete gamma Q(a, x) by the
// modified-Lentz continued fraction (Numerical Recipes "gcf"), used for
// x >= a+1 where the series converges slowly.
func gammaQCF(a, x float64) float64 {
	const (
		maxIter = 500
		eps     = 3e-16
		fpmin   = 1e-300
	)
	b := x + 1 - a
	c := 1 / fpmin
	d := 1 / b
	h := d
	for i := 1; i <= maxIter; i++ {
		an := -float64(i) * (float64(i) - a)
		b += 2
		d = an*d + b
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = b + an/c
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
	la, _ := math.Lgamma(a)
	return math.Exp(-x+a*math.Log(x)-la) * h
}
