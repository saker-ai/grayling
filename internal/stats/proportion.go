package stats

import "math"

// This file implements the two-proportion analysis for binary conversion
// metrics (docs/技术方案.md §10.3: 比例用适当的二项区间/检验).

// ProportionResult is the outcome of a two-sided two-proportion z-test
// comparing conversion rates of two variants (arm B minus arm A). Implements
// the reporting requirements of docs/技术方案.md §10.3: absolute and relative
// difference, per-arm counts, p-value, confidence interval and significance
// at the pre-registered alpha.
type ProportionResult struct {
	// PA, PB are the observed conversion rates of each arm.
	PA float64 `json:"pa"`
	PB float64 `json:"pb"`
	// Diff is PB − PA (absolute difference in conversion rate).
	Diff float64 `json:"diff"`
	// RelativeDiff is Diff/PA when the baseline is meaningful, else 0.
	RelativeDiff float64 `json:"relativeDiff"`
	// NA, NB are the per-arm unit counts; XA, XB the per-arm conversions.
	NA int64 `json:"na"`
	NB int64 `json:"nb"`
	XA int64 `json:"xa"`
	XB int64 `json:"xb"`
	// Z is the pooled two-proportion z statistic.
	Z float64 `json:"z"`
	// PValue is the two-sided p-value from the pooled test.
	PValue float64 `json:"pValue"`
	// CIDiffLow, CIDiffHigh bound the 1−alpha unpooled Wald interval for the
	// true difference in proportions.
	CIDiffLow  float64 `json:"ciDiffLow"`
	CIDiffHigh float64 `json:"ciDiffHigh"`
	// Significant is PValue < Alpha.
	Significant bool `json:"significant"`
	// RelativeMeaningful is false when |PA| <= 1e-12, in which case
	// RelativeDiff is 0 and only the absolute difference should be reported
	// (docs/技术方案.md §10.3: 基线接近零时只报绝对差).
	RelativeMeaningful bool `json:"relativeMeaningful"`
}

// TwoProportionZTest performs a two-sided two-proportion z-test from the
// sufficient statistics of two variants. Implements docs/技术方案.md §10.3.
//
// Variance choices (deliberate and documented):
//   - The p-value uses the POOLED variance p̂(1−p̂)(1/NA+1/NB) with
//     p̂ = (XA+XB)/(NA+NB), because under H0: PA = PB the pooled estimator is
//     the efficient estimate of the common rate; this is the standard
//     hypothesis test (Kohavi/Tang/Xu, Trustworthy Online Controlled
//     Experiments).
//   - The confidence interval uses the UNPOOLED (Wald) standard error
//     sqrt(PA(1−PA)/NA + PB(1−PB)/NB) with a normal quantile, because a CI
//     estimates the difference at the observed rates, not under H0. A known
//     consequence is that the CI and the pooled test can marginally disagree
//     near the significance boundary; the pooled test is authoritative for
//     `Significant`.
//
// Degenerate inputs have documented, deterministic behavior:
//   - N < 1 in either arm, Conversions outside [0, N], or alpha outside
//     (0,1): an error is returned.
//   - Both arms all-zero or all-one conversions (pooled SE = 0, which forces
//     PA = PB): Z = 0, p = 1, not significant, CI = [0, 0] (never NaN).
//   - One arm at 0 and the other at 1 (unpooled SE = 0 but pooled SE > 0):
//     the Wald CI collapses to [Diff, Diff]; the p-value remains the pooled
//     normal approximation. Large-sample approximations are unreliable at
//     such extremes — treat the result as diagnostic only.
//
// FIXED-SAMPLE SEMANTICS (docs/技术方案.md §10.3): evaluate once after the
// pre-registered analysis window closes; repeated peeking at this p-value
// must not be used to stop early and declare victory.
func TwoProportionZTest(a, b ProportionStats, alpha float64) (ProportionResult, error) {
	var res ProportionResult
	if err := validateAlpha(alpha); err != nil {
		return res, err
	}
	if err := a.validate("arm A"); err != nil {
		return res, err
	}
	if err := b.validate("arm B"); err != nil {
		return res, err
	}

	na, nb := float64(a.N), float64(b.N)
	res.NA, res.NB = a.N, b.N
	res.XA, res.XB = a.Conversions, b.Conversions
	res.PA = float64(a.Conversions) / na
	res.PB = float64(b.Conversions) / nb
	res.Diff = res.PB - res.PA

	res.RelativeDiff, res.RelativeMeaningful = relativeDiff(res.Diff, res.PA)
	if !res.RelativeMeaningful {
		res.RelativeDiff = 0
	}

	// Pooled test statistic under H0: PA = PB.
	pPool := float64(a.Conversions+b.Conversions) / (na + nb)
	se2Pooled := pPool * (1 - pPool) * (1/na + 1/nb)

	switch {
	case se2Pooled == 0:
		// pPool is exactly 0 or 1, which forces PA == PB and Diff == 0.
		res.Z = 0
		res.PValue = 1
		res.CIDiffLow, res.CIDiffHigh = 0, 0
		res.Significant = false
		return res, nil
	default:
		res.Z = res.Diff / math.Sqrt(se2Pooled)
		// Two-sided p-value: P(|Z| >= |z|) = erfc(|z|/sqrt(2)).
		res.PValue = clamp01(math.Erfc(math.Abs(res.Z) / math.Sqrt2))
	}

	// Unpooled Wald CI at 1−alpha.
	se2Unpooled := res.PA*(1-res.PA)/na + res.PB*(1-res.PB)/nb
	halfWidth := normalQuantile(1-alpha/2) * math.Sqrt(se2Unpooled)
	res.CIDiffLow = res.Diff - halfWidth
	res.CIDiffHigh = res.Diff + halfWidth

	res.Significant = res.PValue < alpha
	return res, nil
}
