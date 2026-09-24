// Package experiment builds the analysis report for a pre-registered
// experiment from the platform store's unit-level aggregates
// (docs/技术方案.md §10).
//
// Scope of v1 (per §10.3): fixed-sample, TWO-ARM, single pre-registered
// primary metric of type mean (Welch) or proportion (two-proportion z).
// The analysis population is the RANDOMIZED subset — units exposed through
// the percentage split; deterministic targeting/default arms are reported
// for visibility but never enter the primary statistics or SRM.
//
// The report is pure: it reads aggregates and never mutates experiment
// state. Transitioning to Analyzed/Archived is an explicit admin action.
package experiment

import (
	"fmt"
	"math"
	"time"

	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/internal/stats"
	"github.com/godeps/golab/pkg/model"
)

// Report is the full analysis output for one experiment epoch.
type Report struct {
	ExperimentID string                `json:"experimentId"`
	Epoch        int64                 `json:"epoch"`
	FlagKey      string                `json:"flagKey"`
	State        model.ExperimentState `json:"state"`
	GeneratedAt  time.Time             `json:"generatedAt"`

	AnalysisMethod string  `json:"analysisMethod"`
	Alpha          float64 `json:"alpha"`
	MDE            float64 `json:"mde"`
	Control        string  `json:"control"`
	Treatment      string  `json:"treatment"`

	// Healthy is false when any data-quality gate failed; an unhealthy
	// report must never drive ship/rollback automation (§5.3: 统计不可用、
	// SRM、推送异常时暂停，不自动宣称成功).
	Healthy      bool     `json:"healthy"`
	HealthIssues []string `json:"healthIssues,omitempty"`

	SRM                 *stats.SRMResult       `json:"srm,omitempty"`
	PerVariant          []VariantReport        `json:"perVariant"`
	Primary             *PrimaryResult         `json:"primary,omitempty"`
	PrimaryError        string                 `json:"primaryError,omitempty"`
	Guardrails          []stats.GuardrailCheck `json:"guardrails,omitempty"`
	Decision            stats.Decision         `json:"decision"`
	MinRuntimeSatisfied bool                   `json:"minRuntimeSatisfied"`
}

// VariantReport exposes per-arm volumes. RandomizedUnits is the SRM and
// primary-analysis denominator; ExposedUnits additionally counts units that
// reached the arm through deterministic rules (reported for visibility).
type VariantReport struct {
	Variant         string  `json:"variant"`
	ExposedUnits    int64   `json:"exposedUnits"`
	RandomizedUnits int64   `json:"randomizedUnits"`
	Exposures       int64   `json:"exposures"`
	ConvertedUnits  int64   `json:"convertedUnits"` // among randomized units
	PrimaryValue    float64 `json:"primaryValue"`   // conversion rate or mean of per-unit means
}

// PrimaryResult carries the pre-registered test outcome in a uniform shape
// plus the native stats struct for detail.
type PrimaryResult struct {
	Type               string                  `json:"type"` // "welch" | "two-proportion-z"
	PValue             float64                 `json:"pValue"`
	Diff               float64                 `json:"diff"`
	RelativeDiff       float64                 `json:"relativeDiff"`
	RelativeMeaningful bool                    `json:"relativeMeaningful"`
	CILow              float64                 `json:"ciLow"`
	CIHigh             float64                 `json:"ciHigh"`
	Significant        bool                    `json:"significant"`
	Welch              *stats.WelchResult      `json:"welch,omitempty"`
	Proportion         *stats.ProportionResult `json:"proportion,omitempty"`
}

// BuildReport assembles the analysis for one experiment. It fails when the
// experiment has != 2 variants (v1 fixed-sample scope) or aggregates are
// missing in ways that make statistics undefined; a "no data yet" report is
// returned with Healthy=false and verdict hold instead of an error when
// possible, so the console can render progress.
func BuildReport(store *platform.Store, experimentID string, now time.Time) (*Report, error) {
	exp, err := store.GetExperiment(experimentID)
	if err != nil {
		return nil, err
	}
	if len(exp.Variants) != 2 {
		return nil, fmt.Errorf("experiment %s has %d variants; v1 reports are fixed-sample two-arm (§10.3)",
			experimentID, len(exp.Variants))
	}
	control, treatment := exp.Variants[0], exp.Variants[1]

	rep := &Report{
		ExperimentID: exp.ID, Epoch: exp.Epoch, FlagKey: exp.FlagKey,
		State: exp.State, GeneratedAt: now.UTC(),
		AnalysisMethod: exp.AnalysisMethod, Alpha: exp.Alpha, MDE: exp.MDE,
		Control: control, Treatment: treatment,
	}

	// ---- volumes ----
	allExposed := store.ExperimentAggregate(exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch, nil)
	splitSets := store.SplitExposedUnitSet(exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch)

	byVariant := map[string]*VariantReport{}
	order := []string{control, treatment}
	for _, v := range order {
		vr := &VariantReport{Variant: v}
		if agg, ok := allExposed[v]; ok {
			vr.ExposedUnits = agg.ExposedUnits
			vr.Exposures = agg.Exposures
		}
		vr.RandomizedUnits = int64(len(splitSets[v]))
		byVariant[v] = vr
	}

	// ---- primary metric on the randomized population ----
	primaryAgg := store.MetricAggregate(exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch,
		exp.PrimaryMetric.Key)

	ctrlStats, treatStats, convCtrl, convTreat := buildMetricStats(
		exp.PrimaryMetric.Type, primaryAgg, splitSets, control, treatment)

	for v, conv := range map[string]int64{control: convCtrl, treatment: convTreat} {
		byVariant[v].ConvertedUnits = conv
	}
	if exp.PrimaryMetric.Type == "proportion" {
		if n := byVariant[control].RandomizedUnits; n > 0 {
			byVariant[control].PrimaryValue = float64(convCtrl) / float64(n)
		}
		if n := byVariant[treatment].RandomizedUnits; n > 0 {
			byVariant[treatment].PrimaryValue = float64(convTreat) / float64(n)
		}
	} else {
		if ms, ok := ctrlStats.(stats.MeanStats); ok && ms.N > 0 {
			byVariant[control].PrimaryValue = ms.Sum / float64(ms.N)
		}
		if ms, ok := treatStats.(stats.MeanStats); ok && ms.N > 0 {
			byVariant[treatment].PrimaryValue = ms.Sum / float64(ms.N)
		}
	}
	for _, v := range order {
		rep.PerVariant = append(rep.PerVariant, *byVariant[v])
	}

	// ---- SRM: randomized units vs pre-registered proportions ----
	// Experiment proportions are percentages (sum 100); SRMTest expects
	// fractions (sum 1).
	observed := []int64{int64(len(splitSets[control])), int64(len(splitSets[treatment]))}
	expected := []float64{exp.Proportions[control] / 100, exp.Proportions[treatment] / 100}
	srm, srmErr := stats.SRMTest(observed, expected, stats.DefaultSRMThreshold)
	if srmErr != nil {
		if observed[0]+observed[1] == 0 {
			rep.Healthy = false
			rep.HealthIssues = append(rep.HealthIssues, "no randomized exposures recorded yet")
		} else {
			rep.HealthIssues = append(rep.HealthIssues, "SRM computation failed: "+srmErr.Error())
		}
	} else {
		rep.SRM = &srm
		if !srm.Healthy {
			rep.HealthIssues = append(rep.HealthIssues, fmt.Sprintf(
				"SRM alarm: observed %v vs expected %.2f%%/%.2f%% (p=%.2g) — assignment pipeline suspect",
				observed, expected[0], expected[1], srm.PValue))
		}
	}

	// ---- minimum runtime ----
	if started, ok := store.ExperimentStartedAt(exp.ID); ok {
		rep.MinRuntimeSatisfied = now.Sub(started) >= exp.MinRuntime
	} else {
		rep.MinRuntimeSatisfied = false
		if exp.State == model.ExperimentRunning {
			rep.HealthIssues = append(rep.HealthIssues, "experiment running without recorded start time")
		}
	}

	// ---- primary test ----
	var pValue, diff, relDiff, ciLow, ciHigh float64
	var significant, relMeaningful bool
	switch exp.PrimaryMetric.Type {
	case "proportion":
		a, _ := ctrlStats.(stats.ProportionStats)
		b, _ := treatStats.(stats.ProportionStats)
		res, terr := stats.TwoProportionZTest(a, b, exp.Alpha)
		if terr != nil {
			rep.PrimaryError = terr.Error()
		} else {
			pr := res
			rep.Primary = &PrimaryResult{
				Type: "two-proportion-z", PValue: pr.PValue, Diff: pr.Diff,
				RelativeDiff: pr.RelativeDiff, RelativeMeaningful: pr.RelativeMeaningful,
				CILow: pr.CIDiffLow, CIHigh: pr.CIDiffHigh, Significant: pr.Significant,
				Proportion: &pr,
			}
			pValue, diff, relDiff = pr.PValue, pr.Diff, pr.RelativeDiff
			ciLow, ciHigh = pr.CIDiffLow, pr.CIDiffHigh
			significant, relMeaningful = pr.Significant, pr.RelativeMeaningful
		}
	case "mean":
		a, _ := ctrlStats.(stats.MeanStats)
		b, _ := treatStats.(stats.MeanStats)
		res, terr := stats.WelchTTest(a, b, exp.Alpha)
		if terr != nil {
			rep.PrimaryError = terr.Error()
		} else {
			wr := res
			rep.Primary = &PrimaryResult{
				Type: "welch", PValue: wr.PValue, Diff: wr.Diff,
				RelativeDiff: wr.RelativeDiff, RelativeMeaningful: wr.RelativeMeaningful,
				CILow: wr.CIDiffLow, CIHigh: wr.CIDiffHigh, Significant: wr.Significant,
				Welch: &wr,
			}
			pValue, diff, relDiff = wr.PValue, wr.Diff, wr.RelativeDiff
			ciLow, ciHigh = wr.CIDiffLow, wr.CIDiffHigh
			significant, relMeaningful = wr.Significant, wr.RelativeMeaningful
		}
	default:
		return nil, fmt.Errorf("experiment %s: unsupported primary metric type %q", experimentID, exp.PrimaryMetric.Type)
	}
	_, _ = relDiff, relMeaningful // carried in rep.Primary for rendering

	// ---- guardrails (operational rules, separate from significance) ----
	for _, g := range exp.Guardrails {
		check := evaluateGuardrail(store, exp, g, control, treatment, splitSets)
		rep.Guardrails = append(rep.Guardrails, check)
	}

	// ---- health & decision ----
	dataHealthy := len(rep.HealthIssues) == 0 && rep.PrimaryError == "" &&
		(observed[0]+observed[1]) > 0
	rep.Healthy = dataHealthy && (rep.SRM == nil || rep.SRM.Healthy)

	in := stats.DecisionInput{
		PrimaryPValue: pValue, PrimarySignificant: significant,
		PrimaryDiff: adjustDirection(diff, exp.PrimaryMetric.Direction),
		PrimaryCI:   [2]float64{ciLow, ciHigh},
		Alpha:       exp.Alpha, MDE: exp.MDE,
		MinSamplePerArm: exp.MinSamplePerArm,
		NA:              observed[0], NB: observed[1],
		Guardrails:          rep.Guardrails,
		MinRuntimeSatisfied: rep.MinRuntimeSatisfied,
		DataHealthy:         dataHealthy,
	}
	if rep.SRM != nil {
		in.SRM = *rep.SRM
	} else {
		in.SRM = stats.SRMResult{Healthy: false} // unknown SRM never passes the gate
	}
	rep.Decision = stats.Decide(in)
	return rep, nil
}

// adjustDirection flips the sign for "lower is better" metrics so the
// decision gate's diff>0 == improvement invariant holds.
func adjustDirection(diff float64, direction string) float64 {
	if direction == "decrease" {
		return -diff
	}
	return diff
}

// buildMetricStats converts per-unit aggregates restricted to the randomized
// population into the sufficient statistics of the requested metric type.
// Mean metrics aggregate PER UNIT first (mean of each unit's events), so a
// chatty user cannot dominate the test (§10.1: 请求级成本/延迟需先按随机化
// 单元聚合).
func buildMetricStats(metricType string, agg map[string]*platform.MetricAgg,
	splitSets map[string]map[string]bool, control, treatment string) (any, any, int64, int64) {

	build := func(variant string) (any, int64) {
		set := splitSets[variant]
		ma := agg[variant]
		if metricType == "proportion" {
			converted := int64(0)
			if ma != nil {
				for unit := range ma.Units {
					if set[unit] {
						converted++
					}
				}
			}
			return stats.ProportionStats{N: int64(len(set)), Conversions: converted}, converted
		}
		ms := stats.MeanStats{}
		if ma != nil {
			for unit, ua := range ma.Units {
				if !set[unit] || ua.Count == 0 {
					continue
				}
				unitMean := ua.Sum / float64(ua.Count)
				ms.N++
				ms.Sum += unitMean
				ms.SumSquares += unitMean * unitMean
			}
		}
		return ms, ms.N
	}

	ctrlStats, convCtrl := build(control)
	treatStats, convTreat := build(treatment)
	return ctrlStats, treatStats, convCtrl, convTreat
}

// evaluateGuardrail checks one guardrail metric. A breach requires the
// minimum sample on both arms; below it the guardrail is reported as not
// breached with an explanatory detail (insufficient data must not fake
// safety OR panic — the decision gate holds separately).
func evaluateGuardrail(store *platform.Store, exp *platform.Experiment, g platform.GuardrailSpec,
	control, treatment string, splitSets map[string]map[string]bool) stats.GuardrailCheck {

	check := stats.GuardrailCheck{Metric: g.MetricKey}
	agg := store.MetricAggregate(exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch, g.MetricKey)

	ctrlV, treatV, ok := guardrailValues(g.Type, agg, splitSets, control, treatment)
	if !ok {
		check.Detail = "insufficient data for guardrail"
		return check
	}
	nCtrl := int64(len(splitSets[control]))
	nTreat := int64(len(splitSets[treatment]))
	if nCtrl < g.MinSample || nTreat < g.MinSample {
		check.Detail = fmt.Sprintf("below guardrail min sample (%d/%d < %d)", nCtrl, nTreat, g.MinSample)
		return check
	}
	if math.Abs(ctrlV) < 1e-12 {
		// Near-zero baseline: relative regression is undefined; only an
		// absolute move could breach and v1 has no absolute threshold —
		// report without breaching (§10.3: 基线接近零时只报绝对差).
		check.Detail = fmt.Sprintf("baseline ~0 (treatment %.4g); relative guardrail undefined", treatV)
		return check
	}
	relChange := (treatV - ctrlV) / math.Abs(ctrlV)
	// Regression = movement against the guardrail metric's good direction.
	// Direction defaults to "increase" (higher is better); cost/latency
	// guardrails must declare "decrease".
	regression := -relChange
	if g.Direction == "decrease" {
		regression = relChange
	}
	if regression > g.MaxRelativeRegression {
		check.Breached = true
		check.Detail = fmt.Sprintf("relative regression %.2f%% exceeds %.2f%% (control %.4g → treatment %.4g)",
			regression*100, g.MaxRelativeRegression*100, ctrlV, treatV)
	} else {
		check.Detail = fmt.Sprintf("relative change %+.2f%% within %.2f%% threshold",
			relChange*100, g.MaxRelativeRegression*100)
	}
	return check
}

// guardrailValues computes per-arm scalar values (rate or mean of per-unit
// means) restricted to the randomized population.
func guardrailValues(metricType string, agg map[string]*platform.MetricAgg,
	splitSets map[string]map[string]bool, control, treatment string) (float64, float64, bool) {

	value := func(variant string) (float64, bool) {
		set := splitSets[variant]
		if len(set) == 0 {
			return 0, false
		}
		ma := agg[variant]
		if metricType == "proportion" {
			conv := int64(0)
			if ma != nil {
				for unit := range ma.Units {
					if set[unit] {
						conv++
					}
				}
			}
			return float64(conv) / float64(len(set)), true
		}
		var n int64
		var sum float64
		if ma != nil {
			for unit, ua := range ma.Units {
				if !set[unit] || ua.Count == 0 {
					continue
				}
				n++
				sum += ua.Sum / float64(ua.Count)
			}
		}
		if n == 0 {
			return 0, false
		}
		return sum / float64(n), true
	}

	c, okC := value(control)
	t, okT := value(treatment)
	return c, t, okC && okT
}
