// Package stats implements the fixed-sample frequentist statistics engine for
// Grayling experiments: sufficient-statistic aggregation, Welch's t-test for mean
// metrics, a two-proportion z-test for proportion metrics, Sample Ratio
// Mismatch (SRM) detection, and the ship/rollback decision gate.
//
// It implements docs/技术方案.md §10 (实验事件、指标与统计):
//
//   - §10.1 事件契约与归因: the unit of analysis is the randomization unit;
//     aggregates here are per (experiment × epoch × variant × metric)
//     sufficient statistics over enrolled/exposed units. SRM must be computed
//     from exposed-unit counts against the pre-registered allocation — never
//     from conversions.
//   - §10.2 存储与隐私: the aggregates in this file are exactly the
//     precomputed "experiment × epoch × variant × unit × metric" sufficient
//     statistics (N, sum, sum_squares / conversions) that PostgreSQL stores;
//     only aggregates leave the storage layer. Note the documented limit:
//     three aggregate quantities support mean and proportion metrics only —
//     NOT p95/quantiles, ratio metrics, funnels, retention or CUPED (§10.1).
//   - §10.3 推断与决策: v1 is fixed-sample, two-arm, single pre-registered
//     primary metric. Means use Welch's test with an effect CI; proportions
//     use a binomial z-test/interval; absolute and relative differences are
//     reported (relative only when the baseline is not near zero); SRM,
//     guardrails and minimum runtime gate the decision. Repeatedly peeking at
//     the fixed-sample p-value must never be used to declare victory early.
//
// The package is deterministic and stdlib-only: all distribution functions
// (incomplete beta, Student-t CDF/quantile, incomplete gamma) are implemented
// here; no time-based or wall-clock input enters any computation.
package stats

import "math"

// UnitCount is the sufficient statistic for metrics that only count
// randomized units (e.g. enrollment/exposure counts used for SRM and sample
// size checks). Implements docs/技术方案.md §10.2 aggregate storage.
type UnitCount struct {
	// N is the number of distinct randomized units observed.
	N int64
}

// MeanStats is the sufficient statistic for a mean (continuous) metric per
// (variant × metric): unit count, sum of per-unit values and sum of squared
// per-unit values. From these the sample mean, sample variance, Welch test
// and confidence interval are derived (docs/技术方案.md §10.2, §10.3).
//
// Values must be aggregated at the randomization-unit level first (§10.1):
// request-level cost/latency must be pre-aggregated per unit to avoid
// pseudo-replication.
type MeanStats struct {
	// N is the number of randomized units with an observed value.
	N int64
	// Sum is Σ x over units.
	Sum float64
	// SumSquares is Σ x² over units.
	SumSquares float64
}

// ProportionStats is the sufficient statistic for a proportion (binary
// conversion) metric per (variant × metric) (docs/技术方案.md §10.2, §10.3).
type ProportionStats struct {
	// N is the number of randomized units eligible to convert (denominator).
	N int64
	// Conversions is the number of those units that converted (numerator).
	Conversions int64
}

// Merge combines two disjoint UnitCount aggregates (e.g. per-day or
// per-shard partials for the same variant × metric) into one. The operation
// is associative and commutative; callers must only merge disjoint unit sets
// so N stays a count of distinct randomized units (docs/技术方案.md §10.1).
//
// int64 overflow is not checked: unit counts far below 2^63 are assumed.
func (u UnitCount) Merge(other UnitCount) UnitCount {
	return UnitCount{N: u.N + other.N}
}

// Merge combines two disjoint MeanStats aggregates into one by summing N,
// Sum and SumSquares (docs/技术方案.md §10.2). Inputs must cover disjoint
// sets of randomized units.
//
// int64 overflow of N is not checked. Float accumulation error grows with
// the number of merged partials; merge in a stable order when bit-exact
// reproducibility is required.
func (m MeanStats) Merge(other MeanStats) MeanStats {
	return MeanStats{
		N:          m.N + other.N,
		Sum:        m.Sum + other.Sum,
		SumSquares: m.SumSquares + other.SumSquares,
	}
}

// Merge combines two disjoint ProportionStats aggregates into one by summing
// N and Conversions (docs/技术方案.md §10.2). Inputs must cover disjoint
// sets of randomized units.
//
// int64 overflow is not checked.
func (p ProportionStats) Merge(other ProportionStats) ProportionStats {
	return ProportionStats{
		N:           p.N + other.N,
		Conversions: p.Conversions + other.Conversions,
	}
}

// Mean returns the sample mean Sum/N, or NaN when N == 0.
func (m MeanStats) Mean() float64 {
	if m.N == 0 {
		return math.NaN()
	}
	return m.Sum / float64(m.N)
}

// Variance returns the unbiased sample variance (SumSquares − Sum²/N)/(N−1),
// or NaN when N < 2. Tiny negative values caused by floating-point
// cancellation are clamped to zero.
func (m MeanStats) Variance() float64 {
	if m.N < 2 {
		return math.NaN()
	}
	n := float64(m.N)
	ss := m.SumSquares - m.Sum*m.Sum/n
	if ss < 0 {
		ss = 0
	}
	return ss / (n - 1)
}

// Proportion returns the observed conversion rate Conversions/N, or NaN when
// N == 0.
func (p ProportionStats) Proportion() float64 {
	if p.N == 0 {
		return math.NaN()
	}
	return float64(p.Conversions) / float64(p.N)
}

// validate checks the invariants required before inference (docs/技术方案.md
// §10.3: numerically robust aggregates).
func (m MeanStats) validate(arm string) error {
	if m.N <= 0 {
		return &statsError{arm + ": N must be positive"}
	}
	if !isFinite(m.Sum) || !isFinite(m.SumSquares) {
		return &statsError{arm + ": Sum and SumSquares must be finite (no NaN/Inf)"}
	}
	if m.SumSquares < 0 {
		return &statsError{arm + ": SumSquares must be non-negative"}
	}
	return nil
}

// validate checks the invariants required before inference.
func (p ProportionStats) validate(arm string) error {
	if p.N <= 0 {
		return &statsError{arm + ": N must be positive"}
	}
	if p.Conversions < 0 || p.Conversions > p.N {
		return &statsError{arm + ": Conversions must satisfy 0 <= Conversions <= N"}
	}
	return nil
}

// statsError is the package-local error type for invalid statistical inputs.
type statsError struct{ msg string }

func (e *statsError) Error() string { return "stats: " + e.msg }

func isFinite(x float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0)
}

// validateAlpha rejects alphas outside the open interval (0, 1).
func validateAlpha(alpha float64) error {
	if math.IsNaN(alpha) || alpha <= 0 || alpha >= 1 {
		return &statsError{"alpha must be in (0, 1)"}
	}
	return nil
}
