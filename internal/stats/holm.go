package stats

import (
	"fmt"
	"math"
	"slices"
)

// HolmResult describes one member of a pre-registered fixed-sample family.
// Rank is one-based in ascending raw-p order. EffectiveAlpha is the step-down
// threshold at that rank; Rejected also respects all preceding steps.
type HolmResult struct {
	RawP           float64 `json:"rawP"`
	AdjustedP      float64 `json:"adjustedP"`
	EffectiveAlpha float64 `json:"effectiveAlpha"`
	Rank           int     `json:"rank"`
	Rejected       bool    `json:"rejected"`
}

// Holm applies Holm's step-down Bonferroni procedure to one complete family.
// Results retain input order. Ties are ordered by original position, so report
// order is reproducible; neither the adjustment nor decisions depend on ties.
func Holm(pValues []float64, alpha float64) ([]HolmResult, error) {
	if err := validateAlpha(alpha); err != nil {
		return nil, err
	}
	if len(pValues) == 0 {
		return nil, fmt.Errorf("stats: Holm family is empty")
	}
	order := make([]int, len(pValues))
	for i, p := range pValues {
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return nil, fmt.Errorf("stats: Holm p[%d] must be finite and in [0,1]", i)
		}
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		switch {
		case pValues[a] < pValues[b]:
			return -1
		case pValues[a] > pValues[b]:
			return 1
		default:
			return 0
		}
	})
	results := make([]HolmResult, len(pValues))
	maxAdjusted := 0.0
	precedingRejected := true
	for rank, index := range order {
		remaining := len(order) - rank
		maxAdjusted = max(maxAdjusted, float64(remaining)*pValues[index])
		threshold := alpha / float64(remaining)
		precedingRejected = precedingRejected && pValues[index] <= threshold
		results[index] = HolmResult{
			RawP: pValues[index], AdjustedP: min(1, maxAdjusted),
			EffectiveAlpha: threshold, Rank: rank + 1, Rejected: precedingRejected,
		}
	}
	return results, nil
}
