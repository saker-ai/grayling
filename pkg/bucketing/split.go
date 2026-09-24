package bucketing

import (
	"fmt"
	"math"
	"sort"
)

// Split converts a variant→percentage map into contiguous integer bucket
// ranges covering exactly [0, 10000) (docs/技术方案.md §6).
//
// Determinism rules (normative, cross-language):
//
//  1. Percentages must each be in [0, 100] and sum to 100 within 1e-9.
//  2. Variants are ordered lexicographically by name (byte order). This is
//     the "explicit stable order" required by the spec — serialization order
//     of maps is irrelevant.
//  3. base_i = floor(percent_i * 100) buckets per variant.
//  4. The remainder R = 10000 - Σ base_i is distributed one bucket at a time
//     to the variants with the largest fractional parts (percent_i*100 -
//     base_i); ties are broken by the lexicographic order from rule 2.
//  5. Range_i = [Σ_{j<i} buckets_j, Σ_{j<=i} buckets_j) in the same order.
//
// A variant with 0% receives zero buckets and can never be assigned; this is
// allowed (dark launches use it deliberately).
type Split struct {
	// names in canonical (lexicographic) order
	names []string
	// start[i] is inclusive, end[i] exclusive; end[last] == Buckets
	start []int
	end   []int
}

// NewSplit validates percentages and precomputes bucket ranges.
func NewSplit(percentages map[string]float64) (*Split, error) {
	if len(percentages) == 0 {
		return nil, fmt.Errorf("bucketing: empty percentage split")
	}
	names := make([]string, 0, len(percentages))
	for name := range percentages {
		if name == "" {
			return nil, fmt.Errorf("bucketing: empty variation name in split")
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var sum float64
	exact := make([]float64, len(names))
	base := make([]int, len(names))
	for i, name := range names {
		p := percentages[name]
		if p < 0 || p > 100 || math.IsNaN(p) {
			return nil, fmt.Errorf("bucketing: variation %q percentage %v out of [0,100]", name, p)
		}
		sum += p
		scaled := p * float64(Buckets) / 100.0 // buckets per percent = 100
		exact[i] = scaled
		base[i] = int(math.Floor(scaled))
	}
	if math.Abs(sum-100) > 1e-9 {
		return nil, fmt.Errorf("bucketing: percentages sum to %v, want 100", sum)
	}

	// Largest-remainder distribution of the rounding residue.
	assigned := 0
	for _, b := range base {
		assigned += b
	}
	remainder := Buckets - assigned
	if remainder < 0 || remainder > len(names) {
		// Cannot happen when Σpercent == 100 within tolerance, but guard
		// against float pathologies instead of silently skewing splits.
		return nil, fmt.Errorf("bucketing: internal error distributing %d remainder buckets", remainder)
	}
	if remainder > 0 {
		order := make([]int, len(names))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			fa := exact[order[a]] - float64(base[order[a]])
			fb := exact[order[b]] - float64(base[order[b]])
			if fa != fb {
				return fa > fb
			}
			return order[a] < order[b] // lexicographic tie-break
		})
		for i := 0; i < remainder; i++ {
			base[order[i]]++
		}
	}

	s := &Split{names: names, start: make([]int, len(names)), end: make([]int, len(names))}
	pos := 0
	for i := range names {
		s.start[i] = pos
		pos += base[i]
		s.end[i] = pos
	}
	if pos != Buckets {
		return nil, fmt.Errorf("bucketing: internal error, ranges end at %d not %d", pos, Buckets)
	}
	return s, nil
}

// Assign maps a bucket in [0, 9999] to its variation name.
func (s *Split) Assign(bucket int) (string, error) {
	if bucket < 0 || bucket >= Buckets {
		return "", fmt.Errorf("bucketing: bucket %d out of range [0,%d)", bucket, Buckets)
	}
	for i := range s.names {
		if bucket >= s.start[i] && bucket < s.end[i] {
			return s.names[i], nil
		}
	}
	return "", fmt.Errorf("bucketing: internal error, bucket %d uncovered", bucket)
}

// Names returns the canonical (lexicographic) variation order.
func (s *Split) Names() []string {
	out := make([]string, len(s.names))
	copy(out, s.names)
	return out
}

// Range returns the inclusive-exclusive bucket range of one variation for
// simulation tooling ("what-if" rebalance impact, docs §6).
func (s *Split) Range(name string) (start, end int, ok bool) {
	for i := range s.names {
		if s.names[i] == name {
			return s.start[i], s.end[i], true
		}
	}
	return 0, 0, false
}
