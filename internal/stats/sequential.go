package stats

import "math"

// This file implements ALWAYS-VALID SEQUENTIAL inference for the two-arm mean
// difference: an anytime-valid p-value and a (1−alpha) confidence sequence
// built from the Gaussian MIXTURE sequential probability ratio test (mSPRT) of
// Robbins (1970) / Johari, Koomen, Pekelis, Walsh ("Always Valid Inference:
// Continuous Monitoring of A/B Tests", Operations Research 2022). This realizes
// docs/技术方案.md §10.3's deferred 序贯方法: 多次查看不得膨胀 I 型错误.
//
// ── WHY MIXTURE SEQUENTIAL (mSPRT) AND NOT ALPHA-SPENDING ──────────────────
// The technical plan (§10.3) allows either a mixture sequential test or an
// alpha-spending (group-sequential) design. We deliberately choose the MIXTURE
// mSPRT:
//
//   1. No pre-specified looks. Alpha-spending (O'Brien–Fleming, Pocock) fixes
//      the error rate across a PRE-PLANNED number/timing of interim analyses;
//      deviating from that schedule breaks validity. The mSPRT is valid for a
//      CONTINUUM of looks at arbitrary, data-dependent times — exactly how
//      GoLab's console refreshes a report on demand (§11) and how the rollout
//      engine consults the gate every tick (§5.3). We cannot promise operators
//      will look a fixed number of times.
//   2. Single closed form. The mSPRT p-value and confidence sequence are one
//      analytic expression (a Gaussian-mixture likelihood ratio); alpha-spending
//      needs a boundary-crossing recursion over the multivariate normal of the
//      look statistics. Fewer moving parts, fully deterministic, stdlib-only.
//   3. Time-uniform guarantee. By Ville's inequality the mixture likelihood
//      ratio Λ_n is a non-negative test martingale with Λ_0 = 1, so
//      P(∃n: Λ_n ≥ 1/alpha) ≤ alpha under H0. Hence p_n = min(1, 1/Λ_n) is
//      valid at EVERY n simultaneously — peeking cannot inflate the Type-I
//      error, which is the exact failure mode §10.3 forbids for fixed-sample
//      p-values.
//
// The cost is a modest power/sample-size penalty versus a perfectly-scheduled
// group-sequential test (the confidence sequence is ~sqrt(2·log n) wider than a
// fixed-sample CI); this is the right trade for continuous, operator-driven
// monitoring. The mixing scale tau is a PRE-REGISTERED tuning parameter (the
// effect scale the alternative is spread over); tau ≈ MDE gives good power near
// the design effect. Validity holds for any fixed tau > 0.
//
// Model: the difference estimator d̂ = x̄_B − x̄_A is treated as a single
// Gaussian observation with variance V = s²_A/n_A + s²_B/n_B (the Welch
// variance), re-estimated at each look. This is the standard large-sample
// mSPRT; per-unit values are i.i.d. with finite variance.

// SequentialResult is the always-valid inference at one look of a continuously
// monitored two-arm mean comparison. Unlike the fixed-sample results, its
// PValue and confidence sequence are valid at EVERY look simultaneously
// (docs/技术方案.md §10.3), so Significant may be acted on the moment it turns
// true without inflating the Type-I error.
type SequentialResult struct {
	// NA, NB are the per-arm unit counts at this look.
	NA int64 `json:"na"`
	NB int64 `json:"nb"`
	// Diff is the current mean-difference estimate x̄_B − x̄_A.
	Diff float64 `json:"diff"`
	// Variance is V = s²_A/n_A + s²_B/n_B, the variance of Diff.
	Variance float64 `json:"variance"`
	// Tau is the pre-registered mixture scale (effect scale of the alternative
	// prior); reported for traceability.
	Tau float64 `json:"tau"`
	// LogLambda is the natural log of the mixture likelihood ratio Λ (the
	// evidence against H0: Diff = 0); Lambda = exp(LogLambda).
	LogLambda float64 `json:"logLambda"`
	// PValue is the anytime-valid p-value min(1, 1/Λ).
	PValue float64 `json:"pValue"`
	// CSLow, CSHigh bound the (1−alpha) confidence sequence for the true mean
	// difference: valid at every look (time-uniform), not just one point.
	CSLow  float64 `json:"csLow"`
	CSHigh float64 `json:"csHigh"`
	// Significant is PValue < alpha. Because p is anytime-valid, this flag may
	// be checked at every look without a peeking penalty.
	Significant bool `json:"significant"`
	// Alpha is the pre-registered level the confidence sequence/p-value target.
	Alpha float64 `json:"alpha"`
}

// mSPRTLogLambda returns log Λ for the Gaussian mixture SPRT testing H0: θ = 0
// given a difference estimate d̂ with variance V, mixing over θ ~ N(0, τ²):
//
//	Λ = sqrt(V/(V+τ²)) · exp( d̂²·τ² / (2·V·(V+τ²)) )
//
// Derivation: Λ = ∫ exp(θ·d̂/V − θ²/(2V)) · N(θ; 0, τ²) dθ, a Gaussian integral
// (see sequential_test.go for a numeric-integration cross-check). Requires
// V > 0 and τ > 0.
func mSPRTLogLambda(diff, V, tau float64) float64 {
	t2 := tau * tau
	return 0.5*math.Log(V/(V+t2)) + diff*diff*t2/(2*V*(V+t2))
}

// mSPRTConfidenceHalfWidth returns the half-width of the (1−alpha) confidence
// sequence: the u ≥ 0 solving Λ(u) = 1/alpha, i.e. the boundary of the set of
// θ0 not rejected at level alpha.
//
//	u = sqrt( 2·V·(V+τ²)/τ² · log( sqrt((V+τ²)/V) / alpha ) ).
func mSPRTConfidenceHalfWidth(V, tau, alpha float64) float64 {
	t2 := tau * tau
	logTerm := 0.5*math.Log((V+t2)/V) + math.Log(1/alpha)
	if logTerm < 0 {
		logTerm = 0
	}
	return math.Sqrt(2 * V * (V + t2) / t2 * logTerm)
}

// SequentialMeanTest computes the always-valid p-value and (1−alpha) confidence
// sequence for the two-arm mean difference at the current look, from the per-arm
// sufficient statistics (docs/技术方案.md §10.3: 序贯/always-valid).
//
// tau is the pre-registered mixture scale (see the file header); it must be
// finite and > 0. Errors: alpha outside (0,1); N < 2 in either arm; non-finite
// or negative aggregates; tau <= 0; a non-positive estimated variance V (both
// arms constant) — in that last case the difference is known exactly, so callers
// should treat a nonzero Diff as immediately significant; we return an error to
// force an explicit decision rather than silently emitting ±Inf.
//
// CONTINUOUS-MONITORING SEMANTICS: unlike WelchTTest/TwoProportionZTest, this
// function may be called at EVERY look and its Significant flag acted upon
// immediately — the mixture martingale guarantee bounds the Type-I error at
// alpha regardless of how often or when it is evaluated (proven by simulation in
// sequential_test.go).
func SequentialMeanTest(a, b MeanStats, alpha, tau float64) (SequentialResult, error) {
	var res SequentialResult
	if err := validateAlpha(alpha); err != nil {
		return res, err
	}
	if !isFinite(tau) || tau <= 0 {
		return res, &statsError{"sequential: tau must be finite and > 0"}
	}
	if err := a.validate("arm A"); err != nil {
		return res, err
	}
	if err := b.validate("arm B"); err != nil {
		return res, err
	}
	if a.N < 2 || b.N < 2 {
		return res, &statsError{"sequential test requires N >= 2 in both arms"}
	}

	na, nb := float64(a.N), float64(b.N)
	res.NA, res.NB = a.N, b.N
	res.Alpha = alpha
	res.Tau = tau
	res.Diff = b.Mean() - a.Mean()

	varA := a.Variance() / na
	varB := b.Variance() / nb
	V := varA + varB
	if math.IsNaN(V) || V <= 0 {
		return res, &statsError{"sequential: non-positive estimated variance (both arms constant?)"}
	}
	res.Variance = V

	res.LogLambda = mSPRTLogLambda(res.Diff, V, tau)
	res.PValue = clamp01(math.Exp(-res.LogLambda))
	half := mSPRTConfidenceHalfWidth(V, tau, alpha)
	res.CSLow = res.Diff - half
	res.CSHigh = res.Diff + half
	res.Significant = res.PValue < alpha
	return res, nil
}
