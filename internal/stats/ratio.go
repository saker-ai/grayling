package stats

import "math"

// This file implements the ratio-metric analysis (docs/技术方案.md §10.1/§10.3:
// ratio 需每单元分子/分母及交叉乘积；后续版本按验证结果加入 ratio) using the
// delta-method variance of Budylin et al., "Consistent Transformation of Ratio
// Metrics for Efficient Online Controlled Experiments" (WSDM 2018) — the
// standard linearization that makes a ratio of two random sums amenable to the
// same fixed-sample normal inference as a mean.
//
// A ratio metric is R = Σᵢ yᵢ / Σᵢ xᵢ = ȳ/x̄ over randomization units, e.g.
// revenue-per-session (y=revenue, x=sessions), clicks-per-impression, or
// cost-per-task. The analysis unit is the RANDOMIZATION UNIT (§10.1), so both
// the numerator yᵢ and the denominator xᵢ are per-unit totals; the cross
// product xᵢyᵢ is required because the numerator and denominator of the same
// unit are generally correlated.

// RatioStats is the per-arm sufficient statistic for a ratio metric: the unit
// count plus the five second-order sums needed by the delta method
// (docs/技术方案.md §10.1: ratio 需每单元分子/分母及交叉乘积).
type RatioStats struct {
	// N is the number of randomization units contributing a (x, y) pair.
	N int64
	// SumX is Σ xᵢ (denominator), SumY is Σ yᵢ (numerator).
	SumX float64
	SumY float64
	// SumXX = Σ xᵢ², SumYY = Σ yᵢ², SumXY = Σ xᵢyᵢ (the cross product).
	SumXX float64
	SumYY float64
	SumXY float64
}

// Merge combines two disjoint RatioStats aggregates by summing every field
// (docs/技术方案.md §10.2). Inputs must cover disjoint unit sets. Float
// accumulation error grows with the number of merged partials; merge in a
// stable order when bit-exact reproducibility is required. int64 overflow of N
// is not checked.
func (r RatioStats) Merge(other RatioStats) RatioStats {
	return RatioStats{
		N:     r.N + other.N,
		SumX:  r.SumX + other.SumX,
		SumY:  r.SumY + other.SumY,
		SumXX: r.SumXX + other.SumXX,
		SumYY: r.SumYY + other.SumYY,
		SumXY: r.SumXY + other.SumXY,
	}
}

// Ratio returns the point estimate R = SumY/SumX, or NaN when SumX == 0.
func (r RatioStats) Ratio() float64 {
	if r.SumX == 0 {
		return math.NaN()
	}
	return r.SumY / r.SumX
}

// AddUnit folds one randomization unit's (x, y) totals into the aggregate.
// It exists for test/report builders that stream per-unit pairs.
func (r *RatioStats) AddUnit(x, y float64) {
	r.N++
	r.SumX += x
	r.SumY += y
	r.SumXX += x * x
	r.SumYY += y * y
	r.SumXY += x * y
}

// validate checks the invariants required before delta-method inference.
func (r RatioStats) validate(arm string) error {
	if r.N <= 0 {
		return &statsError{arm + ": N must be positive"}
	}
	for _, v := range []float64{r.SumX, r.SumY, r.SumXX, r.SumYY, r.SumXY} {
		if !isFinite(v) {
			return &statsError{arm + ": ratio aggregates must be finite (no NaN/Inf)"}
		}
	}
	if r.SumXX < 0 || r.SumYY < 0 {
		return &statsError{arm + ": SumXX and SumYY must be non-negative"}
	}
	if r.SumX <= 0 {
		return &statsError{arm + ": SumX (denominator) must be positive for a ratio metric"}
	}
	return nil
}

// deltaMethodVariance returns Var(R̂) for R̂ = ȳ/x̄ by the delta method:
//
//	Var(R) ≈ (1/(N·x̄²))·[ s²_y − 2R·s_xy + R²·s²_x ],
//
// where x̄ = SumX/N, ȳ = SumY/N, R = ȳ/x̄ and s²_x, s²_y, s_xy are the
// unbiased sample variances/covariance of the per-unit values. Returns NaN if
// N < 2 or x̄ == 0. A tiny negative value from floating-point cancellation is
// clamped to zero.
func (r RatioStats) deltaMethodVariance() float64 {
	if r.N < 2 || r.SumX == 0 {
		return math.NaN()
	}
	n := float64(r.N)
	xbar := r.SumX / n
	ybar := r.SumY / n
	R := ybar / xbar

	sxx := (r.SumXX - r.SumX*r.SumX/n) / (n - 1)
	syy := (r.SumYY - r.SumY*r.SumY/n) / (n - 1)
	sxy := (r.SumXY - r.SumX*r.SumY/n) / (n - 1)
	// Guard cancellation.
	if sxx < 0 {
		sxx = 0
	}
	if syy < 0 {
		syy = 0
	}

	v := (syy - 2*R*sxy + R*R*sxx) / (n * xbar * xbar)
	if v < 0 {
		v = 0
	}
	return v
}

// RatioResult is the outcome of a two-sided test comparing two ratio metrics
// (arm B minus arm A) via the delta method. It mirrors the reporting shape of
// WelchResult/ProportionResult so the decision gate and console render it
// uniformly (docs/技术方案.md §10.3).
type RatioResult struct {
	// RatioA, RatioB are the per-arm ratio point estimates.
	RatioA float64 `json:"ratioA"`
	RatioB float64 `json:"ratioB"`
	// Diff is RatioB − RatioA (absolute difference).
	Diff float64 `json:"diff"`
	// RelativeDiff is Diff/RatioA when the baseline is meaningful, else 0.
	RelativeDiff float64 `json:"relativeDiff"`
	// VarA, VarB are the delta-method variances of each arm's ratio.
	VarA float64 `json:"varA"`
	VarB float64 `json:"varB"`
	// NA, NB are the per-arm unit counts.
	NA int64 `json:"na"`
	NB int64 `json:"nb"`
	// SE is the standard error of Diff = sqrt(VarA + VarB).
	SE float64 `json:"se"`
	// Z is the normal test statistic Diff/SE.
	Z float64 `json:"z"`
	// PValue is the two-sided p-value from the normal approximation.
	PValue float64 `json:"pValue"`
	// CIDiffLow, CIDiffHigh bound the 1−alpha Wald interval for the true
	// difference in ratios.
	CIDiffLow  float64 `json:"ciDiffLow"`
	CIDiffHigh float64 `json:"ciDiffHigh"`
	// Significant is PValue < Alpha.
	Significant bool `json:"significant"`
	// RelativeMeaningful is false when |RatioA| <= relativeEps.
	RelativeMeaningful bool `json:"relativeMeaningful"`
}

// RatioDeltaTest performs a two-sided ratio-metric test from the per-arm
// sufficient statistics, using the delta-method variance and the normal
// approximation for the difference of the two independent arm ratios
// (docs/技术方案.md §10.3).
//
// Degenerate inputs have documented, deterministic behavior:
//   - N < 2 in either arm, non-positive denominator, non-finite aggregates, or
//     an alpha outside (0,1): an error is returned.
//   - Zero delta-method variance in both arms with equal ratios: SE = 0,
//     p = 1, Z = 0, CI = [0, 0], not significant (never NaN).
//   - Zero delta-method variance in both arms with different ratios: the
//     difference is exact, so p = 0, Significant = true, Z = ±Inf and the CI
//     collapses to [Diff, Diff].
//
// FIXED-SAMPLE SEMANTICS (docs/技术方案.md §10.3): evaluate once after the
// pre-registered analysis window closes; repeated peeking at this p-value must
// not be used to declare victory early. For sequential monitoring of a ratio
// metric, feed the per-look RatioStats into the confidence-sequence machinery
// of sequential.go instead of re-reading this p-value.
func RatioDeltaTest(a, b RatioStats, alpha float64) (RatioResult, error) {
	var res RatioResult
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
		return res, &statsError{"ratio delta test requires N >= 2 in both arms"}
	}

	res.NA, res.NB = a.N, b.N
	res.RatioA = a.Ratio()
	res.RatioB = b.Ratio()
	res.Diff = res.RatioB - res.RatioA
	res.RelativeDiff, res.RelativeMeaningful = relativeDiff(res.Diff, res.RatioA)
	if !res.RelativeMeaningful {
		res.RelativeDiff = 0
	}

	res.VarA = a.deltaMethodVariance()
	res.VarB = b.deltaMethodVariance()
	if math.IsNaN(res.VarA) || math.IsNaN(res.VarB) {
		return res, &statsError{"ratio delta variance undefined (N < 2 or zero denominator)"}
	}

	se2 := res.VarA + res.VarB
	res.SE = math.Sqrt(se2)
	if se2 == 0 {
		res.CIDiffLow, res.CIDiffHigh = res.Diff, res.Diff
		if res.Diff == 0 {
			res.Z, res.PValue, res.Significant = 0, 1, false
		} else {
			res.Z = math.Copysign(math.Inf(1), res.Diff)
			res.PValue, res.Significant = 0, true
		}
		return res, nil
	}

	res.Z = res.Diff / res.SE
	res.PValue = clamp01(math.Erfc(math.Abs(res.Z) / math.Sqrt2))
	half := normalQuantile(1-alpha/2) * res.SE
	res.CIDiffLow = res.Diff - half
	res.CIDiffHigh = res.Diff + half
	res.Significant = res.PValue < alpha
	return res, nil
}
