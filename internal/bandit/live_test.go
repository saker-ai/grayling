package bandit

import (
	"math"
	"testing"

	"github.com/godeps/grayling/pkg/model"
)

func TestLiveDistributionAndAssignment(t *testing.T) {
	b := &model.BanditBinding{PolicyVersion: "p1", Strategy: "thompson", StateVersion: 4,
		Variants: []string{"a", "b"}, PriorAlpha: 1, PriorBeta: 1,
		Trials: []int64{100, 100}, Successes: []int64{20, 80}}
	p, err := Distribution(b)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(p[0]+p[1]-1) > 1e-12 || p[1] < 0.99 {
		t.Fatalf("bad Thompson probability %v", p)
	}
	for i := range 10000 {
		unit := string(rune(1000 + i))
		a, propensity := Assign(b, unit, p)
		other, q := Assign(b, unit, p)
		if a != other || propensity != q || propensity != p[a] {
			t.Fatalf("unstable assignment for %q", unit)
		}
	}
	b.Strategy, b.Epsilon = "epsilon-greedy", 0.2
	p, err = Distribution(b)
	if err != nil || math.Abs(p[0]-0.1) > 1e-12 || math.Abs(p[1]-0.9) > 1e-12 {
		t.Fatalf("epsilon propensity %v: %v", p, err)
	}
}
