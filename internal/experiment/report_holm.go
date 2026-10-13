package experiment

import (
	"fmt"

	"github.com/godeps/grayling/internal/platform"
	"github.com/godeps/grayling/internal/stats"
)

// HolmComparison exposes the fixed-sample result for a registered hypothesis.
// Diff/CI are unadjusted descriptive estimates; only AdjustedP/Rejected
// determine confirmatory significance. An incomplete family yields no claims.
type HolmComparison struct {
	ID             string              `json:"id"`
	Metric         platform.MetricSpec `json:"metric"`
	Control        string              `json:"control"`
	Treatment      string              `json:"treatment"`
	RawP           float64             `json:"rawP"`
	AdjustedP      float64             `json:"adjustedP"`
	EffectiveAlpha float64             `json:"effectiveAlpha"`
	Rank           int                 `json:"rank"`
	Rejected       bool                `json:"rejected"`
	Diff           float64             `json:"diff"`
	DirectedDiff   float64             `json:"directedDiff"`
	CILow          float64             `json:"ciLow"`
	CIHigh         float64             `json:"ciHigh"`
}

// HolmSection is present only for explicitly registered multi-comparison
// experiments. It cannot contain exploratory, guardrail, sequential or
// Bayesian results. Version matches the persisted registration audit key.
type HolmSection struct {
	FamilyID            string           `json:"familyId"`
	RegistrationVersion string           `json:"registrationVersion"`
	Alpha               float64          `json:"alpha"`
	Comparisons         []HolmComparison `json:"comparisons"`
	Error               string           `json:"error,omitempty"`
}

func buildHolmSection(store *platform.Store, exp *platform.Experiment,
	splitSets map[string]map[string]bool, primary *PrimaryResult) *HolmSection {
	section := &HolmSection{FamilyID: exp.HolmFamily.ID,
		RegistrationVersion: exp.RegistrationVersion, Alpha: exp.Alpha,
		Comparisons: []HolmComparison{}}
	if primary == nil {
		section.Error = "primary test unavailable"
		return section
	}
	rows := make([]HolmComparison, len(exp.HolmFamily.Hypotheses))
	pValues := make([]float64, len(rows))
	for i, hypothesis := range exp.HolmFamily.Hypotheses {
		row := HolmComparison{ID: hypothesis.ID, Metric: hypothesis.Metric,
			Control: hypothesis.Control, Treatment: hypothesis.Treatment}
		if i == 0 {
			row.RawP, row.Diff, row.CILow, row.CIHigh = primary.PValue, primary.Diff, primary.CILow, primary.CIHigh
		} else {
			agg := store.MetricAggregate(exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch, hypothesis.Metric.Key)
			control, treatment, _, _ := buildMetricStats(hypothesis.Metric.Type, agg, splitSets,
				hypothesis.Control, hypothesis.Treatment)
			switch hypothesis.Metric.Type {
			case "proportion":
				result, err := stats.TwoProportionZTest(control.(stats.ProportionStats), treatment.(stats.ProportionStats), exp.Alpha)
				if err != nil {
					section.Error = fmt.Sprintf("%s: %v", hypothesis.ID, err)
					return section
				}
				row.RawP, row.Diff, row.CILow, row.CIHigh = result.PValue, result.Diff, result.CIDiffLow, result.CIDiffHigh
			case "mean":
				result, err := stats.WelchTTest(control.(stats.MeanStats), treatment.(stats.MeanStats), exp.Alpha)
				if err != nil {
					section.Error = fmt.Sprintf("%s: %v", hypothesis.ID, err)
					return section
				}
				row.RawP, row.Diff, row.CILow, row.CIHigh = result.PValue, result.Diff, result.CIDiffLow, result.CIDiffHigh
			}
		}
		row.DirectedDiff = adjustDirection(row.Diff, hypothesis.Metric.Direction)
		rows[i], pValues[i] = row, row.RawP
	}
	adjusted, err := stats.Holm(pValues, exp.Alpha)
	if err != nil {
		section.Error = err.Error()
		return section
	}
	for i, result := range adjusted {
		rows[i].AdjustedP = result.AdjustedP
		rows[i].EffectiveAlpha = result.EffectiveAlpha
		rows[i].Rank = result.Rank
		rows[i].Rejected = result.Rejected
	}
	section.Comparisons = rows
	return section
}
