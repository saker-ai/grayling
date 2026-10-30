package main

import (
	"math"
	"strings"

	"github.com/saker-ai/grayling/internal/growth"
)

var aggregateCases = []struct {
	name                                      string
	converted, randomized, exposed, exposures int64
	value                                     float64
	valid                                     bool
}{
	{"negative converted", -1, 50, 100, 100, .2, false},
	{"negative randomized", 0, -1, 100, 100, .2, false},
	{"negative exposed", 0, 0, -1, 100, .2, false},
	{"negative exposures", 0, 0, 0, -1, .2, false},
	{"converted exceeds randomized", 51, 50, 100, 100, .2, false},
	{"randomized exceeds exposed", 20, 101, 100, 101, .2, false},
	{"exposed exceeds exposures", 20, 50, 100, 99, .2, false},
	{"zero randomized with conversion", 1, 0, 1, 1, .2, false},
	{"zero exposed with randomized", 0, 1, 0, 1, .2, false},
	{"zero exposures with exposed", 0, 0, 1, 0, .2, false},
	{"large converted contradiction", math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64, math.MaxInt64, .2, false},
	{"large randomized contradiction", 0, math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64, .2, false},
	{"large exposed contradiction", 0, 0, math.MaxInt64, math.MaxInt64 - 1, .2, false},
	{"equality", 100, 100, 100, 100, .2, true},
	{"repeated exposures", 20, 100, 100, 200, .2, true},
	{"deterministic exposure", 20, 50, 100, 200, .2, true},
	{"only deterministic exposure", 0, 0, 100, 200, .2, true},
	{"zero counts", 0, 0, 0, 0, 0, true},
	{"large equality", math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64, .2, true},
	{"large repeated exposures", 0, 1, math.MaxInt64 - 1, math.MaxInt64, .2, true},
	{"mean units", 20, 50, 100, 200, -42, true},
	{"ratio units", 20, 50, 100, 200, 42, true},
	{"quantile units", 20, 50, 100, 200, 420, true},
}

func aggregateCaseNames() []string {
	names := []string{}
	for _, tc := range aggregateCases {
		names = append(names, "aggregate "+tc.name)
	}
	return names
}

// Mutate the second arm without sharing count pointers with the healthy arm.
func applyAggregateCase(name string, report *growth.Report) bool {
	for _, tc := range aggregateCases {
		if name != "aggregate "+tc.name {
			continue
		}
		report.PerVariant[1] = growth.Variant{Variant: report.PerVariant[1].Variant,
			ConvertedUnits: &tc.converted, RandomizedUnits: &tc.randomized,
			ExposedUnits: &tc.exposed, Exposures: &tc.exposures, PrimaryValue: &tc.value}
		switch tc.name {
		case "mean units":
			report.AnalysisMethod = "welch"
		case "ratio units":
			report.AnalysisMethod = "ratio-delta"
		case "quantile units":
			report.AnalysisMethod = "quantile"
		}
		return !tc.valid
	}
	return false
}

func isAggregateCase(name string) bool { return strings.HasPrefix(name, "aggregate ") }

func invalidAggregateCase(name string) bool {
	for _, tc := range aggregateCases {
		if name == "aggregate "+tc.name {
			return !tc.valid
		}
	}
	return false
}
