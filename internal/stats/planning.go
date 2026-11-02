package stats

import (
	"fmt"
	"math"
)

// PlanTwoProportionSample is a two-sided fixed-sample NORMAL APPROXIMATION
// planning calculator for equal-sized independent arms. It reuses the core
// normal quantile implementation; it is not a test, stopping rule or guarantee
// of achieved power. MDE is an absolute increase/decrease in probability.
func PlanTwoProportionSample(baseline, mde, alpha, power float64, direction string) (int64, error) {
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if !finite(baseline) || !finite(mde) || !finite(alpha) || !finite(power) || baseline <= 0 || baseline >= 1 || mde <= 0 || alpha <= 0 || alpha >= 0.5 || power <= 0.5 || power >= 1 {
		return 0, fmt.Errorf("invalid proportion planning inputs")
	}
	alternative := baseline + mde
	if direction == "decrease" {
		alternative = baseline - mde
	} else if direction != "increase" {
		return 0, fmt.Errorf("invalid planning direction")
	}
	if alternative <= 0 || alternative >= 1 {
		return 0, fmt.Errorf("alternative probability outside open unit interval")
	}
	pooled := (baseline + alternative) / 2
	numerator := normalQuantile(1-alpha/2)*math.Sqrt(2*pooled*(1-pooled)) + normalQuantile(power)*math.Sqrt(baseline*(1-baseline)+alternative*(1-alternative))
	n := math.Ceil(numerator * numerator / (mde * mde))
	if !finite(n) || n < 1 || n > 1000000000 {
		return 0, fmt.Errorf("planned sample outside resource bound")
	}
	return int64(n), nil
}
