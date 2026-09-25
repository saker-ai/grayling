package stats

import "math"

// This file implements CUPED — Controlled-experiment Using Pre-Experiment Data
// (Deng, Xu, Kohavi, Walker, "Improving the Sensitivity of Online Controlled
// Experiments by Utilizing Pre-Experiment Data", WSDM 2013) — the variance
// reduction technique required by docs/技术方案.md §10.1/§10.3 (CUPED 需实验前
// 协变量和实验后结果的同单元配对数据；后续版本按验证结果加入 CUPED).
//
// Idea: for each randomization unit we observe a pre-experiment covariate X
// (e.g. the unit's metric value in the two weeks BEFORE the experiment) and the
// in-experiment outcome Y. Because randomization makes X balanced in
// expectation across arms, subtracting a multiple of the centered covariate
//
//	Y_adj = Y − θ·(X − E[X])
//
// leaves the treatment-effect estimate UNBIASED while reducing its variance by
// the factor (1 − ρ²), where ρ = corr(X, Y). The optimal coefficient is
// θ = Cov(X, Y)/Var(X), estimated from the POOLED arms (a single common θ keeps
// the estimator unbiased and avoids arm-specific overfitting).

// PairedStats is the per-arm sufficient statistic for a covariate-adjusted
// (CUPED) metric: the unit count plus the five second-order sums of the paired
// (covariate X, outcome Y) values (docs/技术方案.md §10.1: 同单元配对数据).
type PairedStats struct {
	// N is the number of randomization units with both a covariate and an
	// outcome observed.
	N int64
	// SumX = Σ Xᵢ (covariate), SumY = Σ Yᵢ (outcome).
	SumX float64
	SumY float64
	// SumXX = Σ Xᵢ², SumYY = Σ Yᵢ², SumXY = Σ XᵢYᵢ.
	SumXX float64
	SumYY float64
	SumXY float64
}

// Merge combines two disjoint PairedStats aggregates by summing every field.
func (p PairedStats) Merge(other PairedStats) PairedStats {
	return PairedStats{
		N:     p.N + other.N,
		SumX:  p.SumX + other.SumX,
		SumY:  p.SumY + other.SumY,
		SumXX: p.SumXX + other.SumXX,
		SumYY: p.SumYY + other.SumYY,
		SumXY: p.SumXY + other.SumXY,
	}
}

// AddPair folds one unit's (covariate, outcome) pair into the aggregate.
func (p *PairedStats) AddPair(x, y float64) {
	p.N++
	p.SumX += x
	p.SumY += y
	p.SumXX += x * x
	p.SumYY += y * y
	p.SumXY += x * y
}

// MeanX, MeanY are the per-arm covariate/outcome means (NaN when N == 0).
func (p PairedStats) MeanX() float64 {
	if p.N == 0 {
		return math.NaN()
	}
	return p.SumX / float64(p.N)
}

func (p PairedStats) MeanY() float64 {
	if p.N == 0 {
		return math.NaN()
	}
	return p.SumY / float64(p.N)
}

// centered returns the unbiased sums of squares/cross-products about the mean:
// (Sxx, Syy, Sxy) each already divided by (N−1). Returns zeros when N < 2.
func (p PairedStats) centered() (sxx, syy, sxy float64) {
	if p.N < 2 {
		return 0, 0, 0
	}
	n := float64(p.N)
	sxx = (p.SumXX - p.SumX*p.SumX/n) / (n - 1)
	syy = (p.SumYY - p.SumY*p.SumY/n) / (n - 1)
	sxy = (p.SumXY - p.SumX*p.SumY/n) / (n - 1)
	if sxx < 0 {
		sxx = 0
	}
	if syy < 0 {
		syy = 0
	}
	return sxx, syy, sxy
}

func (p PairedStats) validate(arm string) error {
	if p.N <= 0 {
		return &statsError{arm + ": N must be positive"}
	}
	for _, v := range []float64{p.SumX, p.SumY, p.SumXX, p.SumYY, p.SumXY} {
		if !isFinite(v) {
			return &statsError{arm + ": CUPED aggregates must be finite (no NaN/Inf)"}
		}
	}
	if p.SumXX < 0 || p.SumYY < 0 {
		return &statsError{arm + ": SumXX and SumYY must be non-negative"}
	}
	return nil
}

// CUPEDResult is the outcome of a covariate-adjusted two-arm comparison (arm B
// minus arm A). It reports BOTH the raw (unadjusted) and CUPED-adjusted effect
// so the variance reduction is auditable, and mirrors the Welch reporting shape
// (docs/技术方案.md §10.3).
type CUPEDResult struct {
	// MeanYA, MeanYB are the raw per-arm outcome means.
	MeanYA float64 `json:"meanYA"`
	MeanYB float64 `json:"meanYB"`
	// RawDiff is the unadjusted mean difference MeanYB − MeanYA.
	RawDiff float64 `json:"rawDiff"`
	// Theta is the pooled covariance-adjustment coefficient Cov(X,Y)/Var(X).
	Theta float64 `json:"theta"`
	// Correlation is the pooled corr(X, Y); VarianceReduction ≈ ρ².
	Correlation float64 `json:"correlation"`
	// Diff is the CUPED-adjusted effect estimate (unbiased for the raw diff).
	Diff float64 `json:"diff"`
	// RelativeDiff is Diff/MeanYA when the baseline is meaningful, else 0.
	RelativeDiff       float64 `json:"relativeDiff"`
	RelativeMeaningful bool    `json:"relativeMeaningful"`
	// VarianceReduction is 1 − Var(Δ_adj)/Var(Δ_raw), the achieved sensitivity
	// gain (0 = none; → 1 as ρ → 1). Clamped to [0, 1) for reporting.
	VarianceReduction float64 `json:"varianceReduction"`
	// VarA, VarB are the CUPED-adjusted variances of each arm's mean.
	VarA float64 `json:"varA"`
	VarB float64 `json:"varB"`
	// NA, NB are the per-arm unit counts.
	NA int64 `json:"na"`
	NB int64 `json:"nb"`
	// T is the Welch t statistic on the adjusted difference; DF the
	// Welch–Satterthwaite degrees of freedom (0 when undefined).
	T  float64 `json:"t"`
	DF float64 `json:"df"`
	// PValue is the two-sided p-value; CIDiff* the 1−alpha interval.
	PValue     float64 `json:"pValue"`
	CIDiffLow  float64 `json:"ciDiffLow"`
	CIDiffHigh float64 `json:"ciDiffHigh"`
	// Significant is PValue < Alpha.
	Significant bool `json:"significant"`
}

// CUPEDTest performs a covariate-adjusted two-arm comparison from the per-arm
// paired sufficient statistics. θ is estimated from the pooled arms; the
// adjusted per-arm mean variances feed a Welch t-test on the adjusted
// difference (docs/技术方案.md §10.3).
//
// Behavior:
//   - N < 2 in either arm, non-finite aggregates, or alpha outside (0,1):
//     an error is returned.
//   - Constant covariate (pooled Var(X) = 0): θ = 0, so the result is EXACTLY
//     the unadjusted Welch test (VarianceReduction = 0). This is the correct
//     degenerate behavior — no adjustment is possible without covariate spread.
//
// The estimator treats θ as fixed (its O(1/n) estimation error is negligible at
// experiment scale), the standard CUPED approximation. FIXED-SAMPLE SEMANTICS
// apply (§10.3): evaluate once after the pre-registered window closes.
func CUPEDTest(a, b PairedStats, alpha float64) (CUPEDResult, error) {
	var res CUPEDResult
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
		return res, &statsError{"CUPED requires N >= 2 in both arms"}
	}

	na, nb := float64(a.N), float64(b.N)
	res.NA, res.NB = a.N, b.N
	res.MeanYA = a.MeanY()
	res.MeanYB = b.MeanY()
	res.RawDiff = res.MeanYB - res.MeanYA

	// Pooled θ = Cov(X,Y)/Var(X) and pooled correlation ρ.
	pool := a.Merge(b)
	pxx, pyy, pxy := pool.centered()
	if pxx > 0 {
		res.Theta = pxy / pxx
		if pyy > 0 {
			res.Correlation = pxy / math.Sqrt(pxx*pyy)
		}
	}
	// else: constant covariate → θ = 0, reduces to unadjusted Welch.

	// Per-arm adjusted mean variances: Var(Ȳ − θX̄) = [s²_y − 2θ·s_xy +
	// θ²·s²_x]/n, using the arm's own second moments and the pooled θ.
	armVar := func(p PairedStats, n float64) (vY, vAdj float64) {
		sxx, _, sxy := p.centered()
		syy := (p.SumYY - p.SumY*p.SumY/n) / (n - 1)
		if syy < 0 {
			syy = 0
		}
		vY = syy / n
		vAdj = vY - 2*res.Theta*sxy/n + res.Theta*res.Theta*sxx/n
		if vAdj < 0 {
			vAdj = 0
		}
		return vY, vAdj
	}
	rawVarA, adjVarA := armVar(a, na)
	rawVarB, adjVarB := armVar(b, nb)
	res.VarA, res.VarB = adjVarA, adjVarB

	// Adjusted effect estimate: Δ_adj = (Ȳ_B − Ȳ_A) − θ(X̄_B − X̄_A).
	res.Diff = res.RawDiff - res.Theta*(b.MeanX()-a.MeanX())
	res.RelativeDiff, res.RelativeMeaningful = relativeDiff(res.Diff, res.MeanYA)
	if !res.RelativeMeaningful {
		res.RelativeDiff = 0
	}

	rawVar := rawVarA + rawVarB
	adjVar := res.VarA + res.VarB
	if rawVar > 0 {
		red := 1 - adjVar/rawVar
		if red < 0 {
			red = 0
		}
		if red > 1 {
			red = 1
		}
		res.VarianceReduction = red
	}

	se2 := adjVar
	se := math.Sqrt(se2)
	if se2 == 0 {
		res.CIDiffLow, res.CIDiffHigh = res.Diff, res.Diff
		if res.Diff == 0 {
			res.T, res.PValue, res.Significant = 0, 1, false
		} else {
			res.T = math.Copysign(math.Inf(1), res.Diff)
			res.PValue, res.Significant = 0, true
		}
		return res, nil
	}

	// Welch–Satterthwaite df on the adjusted arm variances.
	den := res.VarA*res.VarA/(na-1) + res.VarB*res.VarB/(nb-1)
	if den > 0 {
		res.DF = se2 * se2 / den
	}
	res.T = res.Diff / se

	t2 := res.T * res.T
	if res.DF > 0 {
		res.PValue = clamp01(ibeta(res.DF/(res.DF+t2), res.DF/2, 0.5))
	} else {
		res.PValue = clamp01(math.Erfc(math.Abs(res.T) / math.Sqrt2))
	}

	halfWidth := normalQuantile(1-alpha/2) * se
	if res.DF > 0 {
		halfWidth = tQuantile(1-alpha/2, res.DF) * se
	}
	res.CIDiffLow = res.Diff - halfWidth
	res.CIDiffHigh = res.Diff + halfWidth
	res.Significant = res.PValue < alpha
	return res, nil
}
