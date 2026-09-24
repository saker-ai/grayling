package stats

import (
	"testing"
)

// healthySRM is a passing SRM check reused across decision cases.
func healthySRM() SRMResult {
	return SRMResult{ChiSquare: 0.1, PValue: 0.75, Healthy: true, Observed: []float64{500, 500}, Expected: []float64{500, 500}}
}

// srmAlarm is a failing SRM check (p < 0.001).
func srmAlarm() SRMResult {
	return SRMResult{ChiSquare: 16, PValue: 6.3348e-05, Healthy: false, Observed: []float64{5200, 4800}, Expected: []float64{5000, 5000}}
}

// baseInput returns a fully valid decision input: healthy data, healthy SRM,
// runtime satisfied, sample size met, significant positive primary metric.
// Each case mutates one aspect of this baseline.
func baseInput() DecisionInput {
	return DecisionInput{
		PrimaryPValue:       0.001,
		PrimarySignificant:  true,
		PrimaryDiff:         0.05,
		PrimaryCI:           [2]float64{0.02, 0.08},
		Alpha:               0.05,
		MDE:                 0.02,
		MinSamplePerArm:     500,
		NA:                  5000,
		NB:                  5000,
		SRM:                 healthySRM(),
		Guardrails:          nil,
		MinRuntimeSatisfied: true,
		DataHealthy:         true,
	}
}

// TestDecideTable covers every branch of the decision gate in order
// (docs/技术方案.md §10.3).
func TestDecideTable(t *testing.T) {
	cases := []struct {
		name           string
		mutate         func(*DecisionInput)
		wantVerdict    Verdict
		wantBreaches   int
		wantMinReasons int
	}{
		{
			name:           "significant positive primary ships",
			mutate:         func(in *DecisionInput) {},
			wantVerdict:    VerdictShip,
			wantMinReasons: 1,
		},
		{
			name: "significant negative primary rolls back",
			mutate: func(in *DecisionInput) {
				in.PrimaryDiff = -0.05
				in.PrimaryCI = [2]float64{-0.08, -0.02}
			},
			wantVerdict:    VerdictRollback,
			wantMinReasons: 1,
		},
		{
			name: "not significant continues",
			mutate: func(in *DecisionInput) {
				in.PrimaryPValue = 0.30
				in.PrimarySignificant = false
				in.PrimaryDiff = 0.005
				in.PrimaryCI = [2]float64{-0.01, 0.02}
			},
			wantVerdict:    VerdictContinue,
			wantMinReasons: 1,
		},
		{
			name: "significant flag but p >= alpha does not ship (both conditions required)",
			mutate: func(in *DecisionInput) {
				in.PrimaryPValue = 0.06
				in.PrimarySignificant = true
			},
			wantVerdict: VerdictContinue,
		},
		{
			name: "SRM unhealthy holds even when primary significant",
			mutate: func(in *DecisionInput) {
				in.SRM = srmAlarm()
			},
			wantVerdict:    VerdictHold,
			wantMinReasons: 1,
		},
		{
			name: "unhealthy data holds even when primary significant",
			mutate: func(in *DecisionInput) {
				in.DataHealthy = false
			},
			wantVerdict:    VerdictHold,
			wantMinReasons: 1,
		},
		{
			name: "minimum runtime not satisfied holds",
			mutate: func(in *DecisionInput) {
				in.MinRuntimeSatisfied = false
			},
			wantVerdict:    VerdictHold,
			wantMinReasons: 1,
		},
		{
			name: "all three validity failures are reported together",
			mutate: func(in *DecisionInput) {
				in.DataHealthy = false
				in.SRM = srmAlarm()
				in.MinRuntimeSatisfied = false
			},
			wantVerdict:    VerdictHold,
			wantMinReasons: 3,
		},
		{
			name: "guardrail breach rolls back despite significant positive primary",
			mutate: func(in *DecisionInput) {
				in.Guardrails = []GuardrailCheck{
					{Metric: "latency_p95", Breached: true, Detail: "p95 +38% vs baseline"},
					{Metric: "error_rate", Breached: false},
				}
			},
			wantVerdict:    VerdictRollback,
			wantBreaches:   1,
			wantMinReasons: 1,
		},
		{
			name: "multiple guardrail breaches are all recorded",
			mutate: func(in *DecisionInput) {
				in.Guardrails = []GuardrailCheck{
					{Metric: "latency_p95", Breached: true, Detail: "p95 +38%"},
					{Metric: "cost_per_task", Breached: true, Detail: "cost +22%"},
				}
			},
			wantVerdict:    VerdictRollback,
			wantBreaches:   2,
			wantMinReasons: 1,
		},
		{
			name: "guardrail breach rolls back even when primary NOT significant",
			mutate: func(in *DecisionInput) {
				in.PrimaryPValue = 0.40
				in.PrimarySignificant = false
				in.Guardrails = []GuardrailCheck{{Metric: "error_rate", Breached: true, Detail: "errors +5%"}}
			},
			wantVerdict:  VerdictRollback,
			wantBreaches: 1,
		},
		{
			name: "insufficient sample in arm A holds",
			mutate: func(in *DecisionInput) {
				in.NA = 100
			},
			wantVerdict:    VerdictHold,
			wantMinReasons: 1,
		},
		{
			name: "insufficient sample in arm B holds",
			mutate: func(in *DecisionInput) {
				in.NB = 499
			},
			wantVerdict:    VerdictHold,
			wantMinReasons: 1,
		},
		{
			name: "degenerate significant with zero diff does not invent a winner",
			mutate: func(in *DecisionInput) {
				in.PrimaryDiff = 0
			},
			wantVerdict: VerdictContinue,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			tc.mutate(&in)
			d := Decide(in)
			if d.Verdict != tc.wantVerdict {
				t.Fatalf("Verdict = %q, want %q (reasons: %v)", d.Verdict, tc.wantVerdict, d.Reasons)
			}
			if tc.wantMinReasons > 0 && len(d.Reasons) < tc.wantMinReasons {
				t.Fatalf("got %d reasons, want >= %d: %v", len(d.Reasons), tc.wantMinReasons, d.Reasons)
			}
			if len(d.GuardrailBreaches) != tc.wantBreaches {
				t.Fatalf("got %d guardrail breaches, want %d: %+v", len(d.GuardrailBreaches), tc.wantBreaches, d.GuardrailBreaches)
			}
		})
	}
}

// TestDecideRulePrecedence pins the documented evaluation order: validity
// gate first (hold), then guardrails (rollback), then sample size (hold),
// then the primary metric.
func TestDecideRulePrecedence(t *testing.T) {
	// SRM alarm + guardrail breach → hold wins (no winner, no rollback
	// action until validity is restored).
	in := baseInput()
	in.SRM = srmAlarm()
	in.Guardrails = []GuardrailCheck{{Metric: "error_rate", Breached: true}}
	if d := Decide(in); d.Verdict != VerdictHold {
		t.Fatalf("validity gate must precede guardrails: got %q", d.Verdict)
	}

	// Guardrail breach + insufficient sample → rollback wins (guardrails are
	// operational protection and do not wait for sample size).
	in = baseInput()
	in.NA = 10
	in.Guardrails = []GuardrailCheck{{Metric: "error_rate", Breached: true, Detail: "errors +5%"}}
	d := Decide(in)
	if d.Verdict != VerdictRollback {
		t.Fatalf("guardrail gate must precede sample-size gate: got %q", d.Verdict)
	}
	if len(d.GuardrailBreaches) != 1 || d.GuardrailBreaches[0].Metric != "error_rate" {
		t.Fatalf("breach details not propagated: %+v", d.GuardrailBreaches)
	}
}

// TestDecideIsPure verifies Decide mutates nothing observable and returns the
// same verdict for the same input (no clock, no randomness — determinism
// requirement of docs/技术方案.md §10.3 fixed-sample gate).
func TestDecideIsPure(t *testing.T) {
	in := baseInput()
	in.Guardrails = []GuardrailCheck{{Metric: "latency_p95", Breached: true, Detail: "p95 +38%"}}
	first := Decide(in)
	second := Decide(in)
	if first.Verdict != second.Verdict || len(first.Reasons) != len(second.Reasons) {
		t.Fatalf("Decide is not deterministic: %+v vs %+v", first, second)
	}
	for i := range first.Reasons {
		if first.Reasons[i] != second.Reasons[i] {
			t.Fatalf("Decide is not deterministic at reason %d", i)
		}
	}
}
