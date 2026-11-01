package stats

import (
	"math"
	"testing"
)

func TestProportionSamplePlanningReferenceAndInvalidInputs(t *testing.T) {
	// Reference design: two-sided .05, power .8, .50 vs .55
	// gives 1565 per arm using the uncorrected normal approximation.
	n, e := PlanTwoProportionSample(.5, .05, .05, .8, "increase")
	if e != nil || n != 1565 {
		t.Fatalf("reference n=%d error=%v", n, e)
	}
	larger, e := PlanTwoProportionSample(.5, .025, .05, .8, "increase")
	if e != nil || larger <= n*3 {
		t.Fatal("MDE monotonicity")
	}
	mirror, e := PlanTwoProportionSample(.5, .05, .05, .8, "decrease")
	if e != nil || mirror != n {
		t.Fatal("direction symmetry")
	}
	for _, input := range [][4]float64{{0, .05, .05, .8}, {1, .05, .05, .8}, {.5, 0, .05, .8}, {.5, .6, .05, .8}, {.5, .05, 0, .8}, {.5, .05, .05, 1}, {math.NaN(), .05, .05, .8}, {.5, math.Inf(1), .05, .8}} {
		if _, e = PlanTwoProportionSample(input[0], input[1], input[2], input[3], "increase"); e == nil {
			t.Fatal("invalid planning accepted")
		}
	}
}
