package stats

import (
	"math"
	"math/rand"
	"testing"
)

func TestHolmAnalyticExample(t *testing.T) {
	// Published worked example: https://en.wikipedia.org/wiki/Holm%E2%80%93Bonferroni_method
	// Also matches statsmodels.stats.multitest.multipletests(method="holm").
	got, err := Holm([]float64{.01, .04, .03, .005}, .05)
	if err != nil {
		t.Fatal(err)
	}
	wantP := []float64{.03, .06, .06, .02}
	wantRank := []int{2, 4, 3, 1}
	wantAlpha := []float64{.05 / 3, .05, .025, .0125}
	wantReject := []bool{true, false, false, true}
	for i, row := range got {
		if math.Abs(row.AdjustedP-wantP[i]) > 1e-12 || row.Rank != wantRank[i] ||
			math.Abs(row.EffectiveAlpha-wantAlpha[i]) > 1e-12 || row.Rejected != wantReject[i] {
			t.Fatalf("row %d: got %+v", i, row)
		}
	}
}

func TestHolmTiesAndInvalidInput(t *testing.T) {
	got, err := Holm([]float64{.01, .01, .8}, .05)
	if err != nil || got[0].Rank != 1 || got[1].Rank != 2 || got[2].AdjustedP != .8 {
		t.Fatalf("ties: %+v, %v", got, err)
	}
	for _, p := range [][]float64{nil, {-1}, {1.1}, {math.NaN()}, {math.Inf(1)}} {
		if _, err := Holm(p, .05); err == nil {
			t.Fatalf("accepted invalid family %v", p)
		}
	}
}

func TestHolmFamilywiseErrorFixedSeed(t *testing.T) {
	// Under the global null, independent U(0,1) p-values are exact. Holm
	// must control the probability of any false rejection at alpha .05.
	rng := rand.New(rand.NewSource(909))
	const trials = 20000
	falseFamilies := 0
	for range trials {
		p := []float64{rng.Float64(), rng.Float64(), rng.Float64(), rng.Float64()}
		result, err := Holm(p, .05)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range result {
			if row.Rejected {
				falseFamilies++
				break
			}
		}
	}
	rate := float64(falseFamilies) / trials
	if rate < .035 || rate > .055 {
		t.Fatalf("familywise Type I rate = %.4f, expected near .05", rate)
	}
	t.Logf("Holm FWER (seed=909, n=%d, m=4): %.4f", trials, rate)
}
