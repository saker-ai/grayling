package stats

import (
	"math"
	"sort"
)

// This file implements quantile-metric inference (docs/技术方案.md §10.1/§10.3:
// 成本/延迟护栏可另展示 p95，但分位数推断需要保留单元明细；后续版本按验证结果
// 加入 quantile). Two complementary tools are provided:
//
//   - SampleQuantile: the type-7 (linear-interpolation) sample quantile — the
//     R/NumPy default definition — computed from per-unit detail.
//   - A distribution-free confidence interval for a single quantile via the
//     binomial/order-statistic method (Conover, Practical Nonparametric
//     Statistics): the count of observations at or below the true p-quantile is
//     Binomial(n, p), so a (1−alpha) interval is a pair of order statistics.
//     This is exact and assumption-free (conservative), which suits guardrail
//     percentiles like p95 latency/cost.
//   - A two-sample quantile test via the asymptotic normality of the sample
//     quantile, Var(q̂_p) = p(1−p)/(n·f(q_p)²), with the density f(q_p)
//     estimated by a Gaussian kernel density estimator (Silverman bandwidth).
//     The asymptotic approximation needs a moderate sample; it is documented as
//     such and validated by simulation on the median.
//
// Quantile inference operates on per-unit values restricted to the randomized
// population (docs/技术方案.md §10.1: the analysis unit is the randomization
// unit; sum/sum_squares alone cannot produce a percentile).

// SampleQuantile returns the type-7 sample quantile of an ascending-sorted
// slice at probability p ∈ [0,1]. Definition (Hyndman & Fan type 7):
//
//	h = (n−1)·p;  q = x[⌊h⌋] + (h−⌊h⌋)·(x[⌊h⌋+1] − x[⌊h⌋]).
//
// It requires data to be sorted ascending and non-empty; it returns NaN
// otherwise. p is clamped to [0,1].
func SampleQuantile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 || math.IsNaN(p) {
		return math.NaN()
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[n-1]
	}
	h := float64(n-1) * p
	lo := int(math.Floor(h))
	if lo >= n-1 {
		return sorted[n-1]
	}
	frac := h - float64(lo)
	return sorted[lo] + frac*(sorted[lo+1]-sorted[lo])
}

// binomCDF returns P(X <= k) for X ~ Binomial(n, p), via the regularized
// incomplete beta identity P(X <= k) = I_{1−p}(n−k, k+1). p must be in (0,1).
func binomCDF(k, n int, p float64) float64 {
	if k < 0 {
		return 0
	}
	if k >= n {
		return 1
	}
	return ibeta(1-p, float64(n-k), float64(k+1))
}

// quantileOrderStatisticCI returns the 1-based inclusive order-statistic
// indices [L, U] of a distribution-free (1−alpha) confidence interval for the
// true p-quantile from a sample of size n. The interval is [x_(L), x_(U)].
//
// Derivation (sign-test / binomial): P(true q_p < x_(L)) = P(Binom(n,p) <= L−1)
// and P(true q_p > x_(U)) = P(Binom(n,p) >= U); both tails are held at
// <= alpha/2, choosing L as large and U as small as those constraints allow so
// the interval is the tightest exact one. Requires n >= 1 and p in (0,1).
func quantileOrderStatisticCI(n int, p, alpha float64) (L, U int) {
	L, U = 1, n
	if n < 1 || p <= 0 || p >= 1 {
		return 1, n
	}
	half := alpha / 2
	// L = max{ j : P(Binom <= j−1) <= alpha/2 }.
	largest := 0 // 0 means "keep L=1"
	for j := 1; j <= n; j++ {
		if binomCDF(j-1, n, p) <= half {
			largest = j
		} else {
			break
		}
	}
	if largest >= 1 {
		L = largest
	}
	// U = min{ j : P(Binom <= j−1) >= 1 − alpha/2 }.
	smallest := 0 // 0 means "keep U=n"
	for j := n; j >= 1; j-- {
		if binomCDF(j-1, n, p) >= 1-half {
			smallest = j
		} else {
			break
		}
	}
	if smallest >= 1 {
		U = smallest
	}
	if L > U {
		L, U = U, L
	}
	if L < 1 {
		L = 1
	}
	if U > n {
		U = n
	}
	return L, U
}

// gaussianKDE returns the Gaussian kernel density estimate of `sorted` (any
// order) evaluated at x with bandwidth h: f̂(x) = (1/(n·h))·Σ φ((x−xᵢ)/h).
func gaussianKDE(data []float64, x, h float64) float64 {
	n := len(data)
	if n == 0 || h <= 0 || math.IsNaN(h) {
		return math.NaN()
	}
	sum := 0.0
	for _, xi := range data {
		z := (x - xi) / h
		sum += math.Exp(-0.5 * z * z)
	}
	return sum / (float64(n) * h * math.Sqrt(2*math.Pi))
}

// silvermanBandwidth returns Silverman's rule-of-thumb bandwidth
// h = 0.9·min(σ, IQR/1.349)·n^{−1/5} for a sorted sample. Returns NaN when it
// cannot be formed (degenerate spread).
func silvermanBandwidth(sorted []float64) float64 {
	n := len(sorted)
	if n < 2 {
		return math.NaN()
	}
	mean := 0.0
	for _, x := range sorted {
		mean += x
	}
	mean /= float64(n)
	var ss float64
	for _, x := range sorted {
		ss += (x - mean) * (x - mean)
	}
	sd := math.Sqrt(ss / float64(n-1))
	iqr := SampleQuantile(sorted, 0.75) - SampleQuantile(sorted, 0.25)
	spread := sd
	if iqr > 0 {
		if v := iqr / 1.349; v < spread {
			spread = v
		}
	}
	if spread <= 0 {
		return math.NaN()
	}
	return 0.9 * spread * math.Pow(float64(n), -0.2)
}

// quantileAsymptoticSE returns the asymptotic standard error of the sample
// p-quantile: SE = sqrt(p(1−p)/n)/f̂(q_p), with f̂ from a Gaussian KDE at the
// sample quantile (Silverman bandwidth). Returns NaN when the density cannot be
// estimated (e.g. all-identical values).
func quantileAsymptoticSE(data []float64, p float64) float64 {
	n := len(data)
	if n < 2 || p <= 0 || p >= 1 {
		return math.NaN()
	}
	q := SampleQuantile(data, p)
	h := silvermanBandwidth(data)
	if math.IsNaN(h) {
		return math.NaN()
	}
	f := gaussianKDE(data, q, h)
	if math.IsNaN(f) || f <= 0 {
		return math.NaN()
	}
	return math.Sqrt(p*(1-p)/float64(n)) / f
}

// QuantileResult is the outcome of a two-sample quantile comparison (arm B
// minus arm A). It carries BOTH the distribution-free per-arm confidence
// interval (order statistics) and the asymptotic two-sample test, mirroring the
// reporting shape of the other results (docs/技术方案.md §10.3).
type QuantileResult struct {
	// Level is the quantile probability p (e.g. 0.5 median, 0.95 for p95).
	Level float64 `json:"level"`
	// QuantileA, QuantileB are the per-arm sample quantiles.
	QuantileA float64 `json:"quantileA"`
	QuantileB float64 `json:"quantileB"`
	// Diff is QuantileB − QuantileA.
	Diff float64 `json:"diff"`
	// NA, NB are the per-arm unit counts.
	NA int64 `json:"na"`
	NB int64 `json:"nb"`
	// CIA/CIB are the distribution-free (1−alpha) order-statistic CIs per arm.
	CIALow  float64 `json:"ciALow"`
	CIAHigh float64 `json:"ciAHigh"`
	CIBLow  float64 `json:"ciBLow"`
	CIBHigh float64 `json:"ciBHigh"`
	// SEA, SEB are the asymptotic standard errors per arm; SE of the diff.
	SEA float64 `json:"seA"`
	SEB float64 `json:"seB"`
	SE  float64 `json:"se"`
	// Z, PValue are the asymptotic two-sample test statistic and two-sided p.
	Z      float64 `json:"z"`
	PValue float64 `json:"pValue"`
	// CIDiffLow, CIDiffHigh bound the asymptotic 1−alpha interval for the
	// true difference in quantiles.
	CIDiffLow  float64 `json:"ciDiffLow"`
	CIDiffHigh float64 `json:"ciDiffHigh"`
	// Significant is PValue < Alpha.
	Significant bool `json:"significant"`
}

// QuantileTest compares the p-quantile of two arms from their per-unit values.
// Values are copied and sorted internally (input order/aliasing is irrelevant).
//
// Errors: alpha outside (0,1); level p outside (0,1); either arm with fewer
// than 2 units. The per-arm distribution-free CI is always produced; the
// two-sample asymptotic test additionally requires a positive KDE density in
// both arms — if the density cannot be estimated the p-value is reported as 1
// (not significant) with SE/Z/CI-diff set to NaN rather than failing, so a
// degenerate (constant) metric never crashes a report.
//
// FIXED-SAMPLE SEMANTICS (docs/技术方案.md §10.3): evaluate once after the
// pre-registered window closes. The two-sample asymptotic test is a
// large-sample approximation; treat tail percentiles (e.g. p95) as diagnostic
// when the per-arm sample is small.
func QuantileTest(a, b []float64, level, alpha float64) (QuantileResult, error) {
	var res QuantileResult
	if err := validateAlpha(alpha); err != nil {
		return res, err
	}
	if math.IsNaN(level) || level <= 0 || level >= 1 {
		return res, &statsError{"quantile level must be in (0, 1)"}
	}
	if len(a) < 2 || len(b) < 2 {
		return res, &statsError{"quantile test requires >= 2 units in both arms"}
	}

	sa := append([]float64(nil), a...)
	sb := append([]float64(nil), b...)
	sort.Float64s(sa)
	sort.Float64s(sb)

	res.Level = level
	res.NA, res.NB = int64(len(sa)), int64(len(sb))
	res.QuantileA = SampleQuantile(sa, level)
	res.QuantileB = SampleQuantile(sb, level)
	res.Diff = res.QuantileB - res.QuantileA

	// Distribution-free per-arm CIs (order statistics).
	la, ua := quantileOrderStatisticCI(len(sa), level, alpha)
	lb, ub := quantileOrderStatisticCI(len(sb), level, alpha)
	res.CIALow, res.CIAHigh = sa[la-1], sa[ua-1]
	res.CIBLow, res.CIBHigh = sb[lb-1], sb[ub-1]

	// Asymptotic two-sample test.
	res.SEA = quantileAsymptoticSE(sa, level)
	res.SEB = quantileAsymptoticSE(sb, level)
	if math.IsNaN(res.SEA) || math.IsNaN(res.SEB) || res.SEA <= 0 || res.SEB <= 0 {
		// Degenerate (constant) metric: no density, no test. Report the point
		// estimates and distribution-free CIs but declare non-significance.
		res.PValue = 1
		res.Significant = false
		return res, nil
	}
	se2 := res.SEA*res.SEA + res.SEB*res.SEB
	res.SE = math.Sqrt(se2)
	res.Z = res.Diff / res.SE
	res.PValue = clamp01(math.Erfc(math.Abs(res.Z) / math.Sqrt2))
	half := normalQuantile(1-alpha/2) * res.SE
	res.CIDiffLow = res.Diff - half
	res.CIDiffHigh = res.Diff + half
	res.Significant = res.PValue < alpha
	return res, nil
}
