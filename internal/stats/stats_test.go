package stats

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// Sufficient-statistic aggregation (docs/技术方案.md §10.2).
// ---------------------------------------------------------------------------

func TestUnitCountMerge(t *testing.T) {
	got := UnitCount{N: 30}.Merge(UnitCount{N: 12})
	if got.N != 42 {
		t.Fatalf("Merge = %+v, want N=42", got)
	}
}

func TestMeanStatsMerge(t *testing.T) {
	// Partial 1: {1,2,3}; partial 2: {4,5}. Merged must equal the stats of
	// the full sample {1..5}.
	p1 := MeanStats{N: 3, Sum: 6, SumSquares: 14}
	p2 := MeanStats{N: 2, Sum: 9, SumSquares: 41}
	got := p1.Merge(p2)
	want := meanStatsOf(1, 2, 3, 4, 5)
	if got != want {
		t.Fatalf("Merge = %+v, want %+v", got, want)
	}
	// Merge is commutative.
	if back := p2.Merge(p1); back != got {
		t.Fatalf("Merge not commutative: %+v vs %+v", got, back)
	}
	// Associativity on a third partial: {6}.
	p3 := MeanStats{N: 1, Sum: 6, SumSquares: 36}
	lhs := got.Merge(p3)
	rhs := p1.Merge(p2.Merge(p3))
	if lhs != rhs {
		t.Fatalf("Merge not associative: %+v vs %+v", lhs, rhs)
	}
}

func TestProportionStatsMerge(t *testing.T) {
	p1 := ProportionStats{N: 600, Conversions: 60}
	p2 := ProportionStats{N: 400, Conversions: 30}
	got := p1.Merge(p2)
	if got.N != 1000 || got.Conversions != 90 {
		t.Fatalf("Merge = %+v, want N=1000 Conversions=90", got)
	}
}

func TestMeanStatsMeanVariance(t *testing.T) {
	m := meanStatsOf(1, 2, 3, 4, 5)
	if m.Mean() != 3 {
		t.Fatalf("Mean = %v, want 3", m.Mean())
	}
	if v := m.Variance(); math.Abs(v-2.5) > 1e-12 {
		t.Fatalf("Variance = %v, want 2.5", v)
	}
	// Empty / single-unit aggregates: defined sentinels, not panics.
	if !math.IsNaN((MeanStats{}).Mean()) || !math.IsNaN((MeanStats{}).Variance()) {
		t.Fatal("empty MeanStats must report NaN mean/variance")
	}
	if !math.IsNaN((MeanStats{N: 1, Sum: 2, SumSquares: 4}).Variance()) {
		t.Fatal("N=1 variance must be NaN")
	}
	if !math.IsNaN((ProportionStats{}).Proportion()) {
		t.Fatal("empty ProportionStats must report NaN proportion")
	}
	// Floating-point cancellation clamps variance to 0, never negative:
	// constant value 1e8 repeated 4 times.
	constVal := MeanStats{N: 4, Sum: 4e8, SumSquares: 4e16}
	if v := constVal.Variance(); v < 0 || math.IsNaN(v) {
		t.Fatalf("Variance = %v, want >= 0 despite cancellation", v)
	}
}

func TestProportionStatsProportion(t *testing.T) {
	p := ProportionStats{N: 1000, Conversions: 100}
	if p.Proportion() != 0.1 {
		t.Fatalf("Proportion = %v, want 0.1", p.Proportion())
	}
}
