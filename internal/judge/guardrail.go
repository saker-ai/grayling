package judge

import (
	"fmt"
	"math"

	"github.com/saker-ai/grayling/internal/stats"
)

// Decision-gate glue (docs/技术方案.md §10.4: judge 作为护栏指标接入决策门；校准
// 不达标时质量指标只作诊断，不驱动自动放量或回滚).
//
// QualityGuardrail converts a judge-derived quality metric (mean rubric score of
// the treatment responses vs the control responses) into a stats.GuardrailCheck
// consumable by stats.Decide — so the SAME validity/guardrail/decision ordering
// (docs §10.3) applies to judge metrics as to any other guardrail.
//
// CRITICAL §10.4 RULE: when the judge is NOT calibrated (Calibrate returned
// Calibrated=false), the guardrail is DIAGNOSTIC ONLY — Breached is forced false
// so an untrustworthy judge can never trigger a rollback or block a rollout; the
// observed regression is still reported in Detail for visibility. Only a
// calibrated judge may gate the decision.

// QualityGuardrail builds the guardrail check. controlScore/treatScore are the
// design-weighted mean judge rubric scores per arm; n is the number of judged
// samples behind treatScore. Higher score is better (a judge rubric measures
// quality); a treatment regression beyond maxRelativeRegression breaches — but
// only when calibrated and the sample is sufficient.
func QualityGuardrail(metric string, controlScore, treatScore, maxRelativeRegression float64,
	cal CalibrationReport, n, minSample int64) stats.GuardrailCheck {

	check := stats.GuardrailCheck{Metric: metric}

	if n < minSample {
		check.Detail = fmt.Sprintf("judge sample %d < min %d — insufficient, not gating (§10.4)", n, minSample)
		return check // Breached = false
	}

	rel := relativeChange(controlScore, treatScore)

	if !cal.Calibrated {
		// DIAGNOSTIC ONLY: never breach on an uncalibrated judge (§10.4).
		check.Detail = fmt.Sprintf(
			"judge UNCALIBRATED (%s; κ=%.3f, n=%d): diagnostic only — observed quality change %+.2f%% does NOT gate the decision (§10.4)",
			orDefault(cal.Reason, "not calibrated"), cal.CohenKappa, cal.N, rel*100)
		return check // Breached = false regardless of regression
	}

	if math.Abs(controlScore) < 1e-12 {
		check.Detail = fmt.Sprintf("control judge score ~0 (treatment %.4g); relative guardrail undefined", treatScore)
		return check
	}

	// Calibrated judge → the guardrail gates like any operational guardrail.
	regression := -rel // higher score is better, so a drop is the regression.
	if regression > maxRelativeRegression {
		check.Breached = true
		check.Detail = fmt.Sprintf("judge quality regressed %.2f%% > %.2f%% threshold (control %.4g → treatment %.4g, κ=%.3f)",
			regression*100, maxRelativeRegression*100, controlScore, treatScore, cal.CohenKappa)
	} else {
		check.Detail = fmt.Sprintf("judge quality change %+.2f%% within %.2f%% threshold (κ=%.3f, calibrated)",
			rel*100, maxRelativeRegression*100, cal.CohenKappa)
	}
	return check
}

// relativeChange returns (treat − control)/|control|, or 0 when control ≈ 0.
func relativeChange(control, treat float64) float64 {
	if math.Abs(control) < 1e-12 {
		return 0
	}
	return (treat - control) / math.Abs(control)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
