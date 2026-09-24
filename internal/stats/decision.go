package stats

import "fmt"

// This file implements the fixed-sample decision gate
// (docs/技术方案.md §10.3 推断与决策).
//
// FIXED-SAMPLE SEMANTICS — READ BEFORE CALLING:
//
//	Decide implements a SINGLE-EVALUATION, fixed-sample decision rule. The
//	p-value and confidence interval it consumes are only valid at the
//	pre-registered analysis point (window closed, alpha/power/MDE/minimum
//	runtime fixed in advance). Repeatedly invoking Decide on accumulating
//	data and acting on the first "ship"/"rollback" is p-value peeking: it
//	inflates the Type-I error far beyond alpha and must NOT be used to
//	declare victory early (docs/技术方案.md §10.3: 多次查看固定样本 p 值不得
//	据此随时宣称胜利). Decide is a pure function; deciding WHEN a call is
//	legitimate (analysis window closed, epoch frozen, SRM inputs from
//	exposed units) is the caller's responsibility. Sequential or always-valid
//	methods are deferred to later versions per §10.3.

// Verdict is the outcome of the decision gate.
type Verdict string

const (
	// VerdictShip means: data healthy, no guardrail breach, sample size
	// reached, and the pre-registered primary metric is significantly
	// BETTER in the treatment arm.
	VerdictShip Verdict = "ship"
	// VerdictRollback means: a guardrail was breached, or the primary
	// metric is significantly WORSE in the treatment arm.
	VerdictRollback Verdict = "rollback"
	// VerdictContinue means: everything is healthy but the primary metric
	// is not (yet) significant — keep running until the planned sample size
	// / analysis window; do not conclude.
	VerdictContinue Verdict = "continue"
	// VerdictHold means: the experiment cannot be evaluated — unhealthy
	// data pipeline, SRM alarm, minimum runtime not met, or insufficient
	// sample per arm. No winner may be declared while a hold stands.
	VerdictHold Verdict = "hold"
)

// GuardrailBreach records one breached guardrail metric (docs/技术方案.md
// §10.3: 护栏阈值是运营保护规则，和统计显著性分开).
type GuardrailBreach struct {
	// Metric identifies the guardrail metric (e.g. "latency_p95", "error_rate").
	Metric string `json:"metric"`
	// Detail is a human-readable explanation of the breach.
	Detail string `json:"detail"`
}

// GuardrailCheck is the caller's evaluation of one operational guardrail
// against its pre-registered threshold. Guardrails are operational
// protection rules evaluated INDEPENDENTLY of the primary metric's
// statistical significance (docs/技术方案.md §10.3).
type GuardrailCheck struct {
	// Metric identifies the guardrail metric.
	Metric string
	// Breached is true when the guardrail threshold was violated.
	Breached bool `json:"breached"`
	// Detail explains the breach (used when Breached is true).
	Detail string
}

// Decision is the outcome of Decide.
type Decision struct {
	// Verdict is one of VerdictShip, VerdictRollback, VerdictContinue,
	// VerdictHold.
	Verdict Verdict `json:"verdict"`
	// Reasons are ordered, human-readable explanations for the verdict.
	Reasons []string `json:"reasons"`
	// GuardrailBreaches lists every breached guardrail (empty unless the
	// guardrail rule fired).
	GuardrailBreaches []GuardrailBreach `json:"guardrailBreaches,omitempty"`
}

// DecisionInput carries everything the decision gate needs for one
// fixed-sample evaluation of a two-arm experiment with a single
// pre-registered primary metric (docs/技术方案.md §10.3).
type DecisionInput struct {
	// PrimaryPValue is the two-sided p-value of the pre-registered primary
	// metric (from WelchTTest or TwoProportionZTest).
	PrimaryPValue float64
	// PrimarySignificant is the Significant flag of the primary result.
	PrimarySignificant bool
	// PrimaryDiff is the primary metric's absolute difference (treatment B
	// minus control A).
	PrimaryDiff float64
	// PrimaryCI is the 1−alpha confidence interval [low, high] of
	// PrimaryDiff, carried for reporting/traceability.
	PrimaryCI [2]float64
	// Alpha is the pre-registered significance level.
	Alpha float64
	// MDE is the pre-registered minimum detectable effect. v1's verdict
	// rules do not gate on MDE (it is recorded for reporting and for the
	// pre-flight sample-size calculation); a practical-equivalence floor on
	// the CI is deferred to a later version.
	MDE float64
	// MinSamplePerArm is the pre-registered minimum exposed-unit count per
	// arm before any verdict may be declared.
	MinSamplePerArm int64
	// NA, NB are the exposed-unit counts per arm (randomization units, not
	// requests — docs/技术方案.md §10.1).
	NA, NB int64
	// SRM is the sample-ratio-mismatch check computed from exposed units
	// against the pre-registered allocation (docs/技术方案.md §10.1).
	SRM SRMResult
	// Guardrails are the operational guardrail checks, evaluated
	// independently of statistical significance (docs/技术方案.md §10.3).
	Guardrails []GuardrailCheck
	// MinRuntimeSatisfied is true once the pre-registered minimum experiment
	// runtime has elapsed (guards novelty/weekly-cycle effects).
	MinRuntimeSatisfied bool
	// DataHealthy is true when the reporting pipeline is trustworthy:
	// acceptable event loss/latency, no dedup failure (docs/技术方案.md
	// §10.2: 数据源故障、事件延迟和丢失率是报告健康条件).
	DataHealthy bool
}

// Decide applies the fixed-sample decision gate in EXACTLY this order
// (docs/技术方案.md §10.3):
//
//  1. Validity gate: if the data pipeline is unhealthy, SRM is unhealthy, or
//     the minimum runtime has not elapsed → VerdictHold. No winner may be
//     declared while any validity condition fails.
//  2. Guardrail gate: any breached guardrail → VerdictRollback. Guardrails
//     are operational protection rules, evaluated independently of the
//     primary metric's statistical significance.
//  3. Sample-size gate: NA or NB below MinSamplePerArm → VerdictHold
//     (insufficient sample).
//  4. Primary-metric gate: PrimarySignificant && PrimaryPValue < Alpha →
//     VerdictShip when PrimaryDiff > 0, VerdictRollback when PrimaryDiff < 0
//     (primary metric significantly worse).
//  5. Otherwise → VerdictContinue (keep running to the planned sample size;
//     absence of significance is not evidence of no effect).
//
// Decide is pure and deterministic: it reads no clock and no external state.
// See the file header for the fixed-sample/peeking contract.
func Decide(in DecisionInput) Decision {
	var d Decision

	// Rule 1: validity gate.
	if !in.DataHealthy {
		d.Reasons = append(d.Reasons, "data pipeline unhealthy (event loss/latency/source failure beyond reporting limits, docs §10.2)")
	}
	if !in.SRM.Healthy {
		d.Reasons = append(d.Reasons, fmt.Sprintf("SRM alarm: chi-square=%.4f p=%.3g below threshold; observed split inconsistent with the pre-registered allocation (docs §10.1)", in.SRM.ChiSquare, in.SRM.PValue))
	}
	if !in.MinRuntimeSatisfied {
		d.Reasons = append(d.Reasons, "pre-registered minimum runtime not yet satisfied")
	}
	if len(d.Reasons) > 0 {
		d.Verdict = VerdictHold
		return d
	}

	// Rule 2: guardrail gate — operational protection, independent of
	// statistical significance (docs §10.3).
	for _, g := range in.Guardrails {
		if g.Breached {
			d.GuardrailBreaches = append(d.GuardrailBreaches, GuardrailBreach{Metric: g.Metric, Detail: g.Detail})
		}
	}
	if len(d.GuardrailBreaches) > 0 {
		d.Verdict = VerdictRollback
		d.Reasons = append(d.Reasons, fmt.Sprintf("%d guardrail(s) breached — operational protection fires regardless of primary-metric significance (docs §10.3)", len(d.GuardrailBreaches)))
		return d
	}

	// Rule 3: sample-size gate.
	if in.NA < in.MinSamplePerArm || in.NB < in.MinSamplePerArm {
		d.Verdict = VerdictHold
		d.Reasons = append(d.Reasons, fmt.Sprintf("insufficient sample: NA=%d NB=%d below pre-registered minimum %d per arm", in.NA, in.NB, in.MinSamplePerArm))
		return d
	}

	// Rule 4: primary-metric gate (single pre-registered metric, two-sided).
	if in.PrimarySignificant && in.PrimaryPValue < in.Alpha {
		switch {
		case in.PrimaryDiff > 0:
			d.Verdict = VerdictShip
			d.Reasons = append(d.Reasons, fmt.Sprintf("primary metric significantly better in treatment: diff=%.6g CI=[%.6g, %.6g] p=%.4g < alpha=%.4g", in.PrimaryDiff, in.PrimaryCI[0], in.PrimaryCI[1], in.PrimaryPValue, in.Alpha))
		case in.PrimaryDiff < 0:
			d.Verdict = VerdictRollback
			d.Reasons = append(d.Reasons, fmt.Sprintf("primary metric significantly worse in treatment: diff=%.6g CI=[%.6g, %.6g] p=%.4g < alpha=%.4g", in.PrimaryDiff, in.PrimaryCI[0], in.PrimaryCI[1], in.PrimaryPValue, in.Alpha))
		default:
			// Degenerate: Significant with Diff == 0 cannot occur for the
			// tests in this package (a zero difference yields p = 1). Treat
			// as inconclusive rather than inventing a winner.
			d.Verdict = VerdictContinue
			d.Reasons = append(d.Reasons, "primary metric reported significant with zero effect; inconclusive, no winner declared")
		}
		return d
	}

	// Rule 5: not significant → continue to the planned sample size.
	// (v1 has no planned-total field in DecisionInput; the caller owns the
	// sample-size plan and re-invokes Decide when the window closes.)
	d.Verdict = VerdictContinue
	d.Reasons = append(d.Reasons, fmt.Sprintf("primary metric not significant (p=%.4g, alpha=%.4g); continue to planned sample size — do not conclude equivalence", in.PrimaryPValue, in.Alpha))
	return d
}
