package experiment

import (
	"fmt"
	"slices"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/internal/stats"
)

// JourneyArm exposes a rate's exact population and exclusions. Immature
// cohorts are excluded from both numerator and denominator.
type JourneyArm struct {
	Variant     string           `json:"variant"`
	Denominator int64            `json:"denominator"`
	Numerator   int64            `json:"numerator"`
	Immature    int64            `json:"immature"`
	Excluded    map[string]int64 `json:"excluded,omitempty"`
}

// JourneySection records the frozen definition, event-time watermark, and
// independently recomputed arm counts without exporting unit identifiers.
type JourneySection struct {
	Metric    string                  `json:"metric"`
	Type      string                  `json:"type"`
	Funnel    *platform.FunnelSpec    `json:"funnel,omitempty"`
	Retention *platform.RetentionSpec `json:"retention,omitempty"`
	Watermark time.Time               `json:"watermark"`
	Arms      []JourneyArm            `json:"arms"`
}

type journeyUnit struct{ variant, unit string }

func buildJourney(exp *platform.Experiment, typ, key string, funnel *platform.FunnelSpec,
	retention *platform.RetentionSpec, events []*platform.Event, window platform.AnalysisWindowStatus) JourneySection {
	section := JourneySection{Metric: key, Type: typ, Funnel: funnel, Retention: retention, Watermark: window.Watermark}
	ordered := slices.Clone(events)
	slices.SortFunc(ordered, func(a, b *platform.Event) int {
		if c := a.ClientTime.Compare(b.ClientTime); c != 0 {
			return c
		}
		if a.EventID < b.EventID {
			return -1
		}
		if a.EventID > b.EventID {
			return 1
		}
		return 0
	})
	exposed := map[journeyUnit]time.Time{}
	for _, ev := range ordered {
		if ev.Type != platform.EventExposure || ev.Source != "split" || ev.ClientTime.Before(window.Start) || !ev.ClientTime.Before(window.End) {
			continue
		}
		k := journeyUnit{ev.Variant, ev.UnitHash}
		if _, ok := exposed[k]; !ok {
			exposed[k] = ev.ClientTime
		}
	}
	for _, variant := range exp.Variants {
		arm := JourneyArm{Variant: variant, Excluded: map[string]int64{}}
		if typ == "funnel" {
			buildFunnelArm(&arm, funnel, ordered, exposed, window)
		} else {
			buildRetentionArm(&arm, retention, ordered, exposed, window)
		}
		section.Arms = append(section.Arms, arm)
	}
	return section
}

func buildFunnelArm(arm *JourneyArm, spec *platform.FunnelSpec, events []*platform.Event,
	exposed map[journeyUnit]time.Time, window platform.AnalysisWindowStatus) {
	groups := map[string][]*platform.Event{}
	for _, ev := range events {
		if ev.Type != platform.EventConversion || ev.Variant != arm.Variant || ev.ClientTime.Before(window.Start) || !ev.ClientTime.Before(window.End) {
			continue
		}
		at, ok := exposed[journeyUnit{ev.Variant, ev.UnitHash}]
		if !ok || ev.ClientTime.Before(at) {
			arm.Excluded["not_randomized_or_before_exposure"]++
			continue
		}
		if !slices.Contains(spec.Steps, ev.MetricKey) {
			continue
		}
		group := ev.UnitHash
		if spec.DedupUnit == "session" {
			if ev.SessionID == "" {
				arm.Excluded["missing_session"]++
				continue
			}
			group += "/" + ev.SessionID
		}
		groups[group] = append(groups[group], ev)
	}
	for _, group := range groups {
		start := time.Time{}
		step := 0
		prev := time.Time{}
		for _, ev := range group {
			if step == 0 {
				if ev.MetricKey == spec.Steps[0] {
					start = ev.ClientTime
					prev = start
					step = 1
				}
				continue
			}
			if ev.MetricKey == spec.Steps[step] && ev.ClientTime.After(prev) && ev.ClientTime.Before(prev.Add(spec.StepTimeout)) {
				prev = ev.ClientTime
				step++
				if step == len(spec.Steps) {
					break
				}
			}
		}
		if start.IsZero() {
			arm.Excluded["missing_first_step"]++
			continue
		}
		arm.Denominator++
		if step == len(spec.Steps) {
			arm.Numerator++
		} else {
			arm.Excluded["missing_or_late_step"]++
		}
	}
}

func buildRetentionArm(arm *JourneyArm, spec *platform.RetentionSpec, events []*platform.Event,
	exposed map[journeyUnit]time.Time, window platform.AnalysisWindowStatus) {
	loc, _ := time.LoadLocation(spec.Timezone) // checked at registration
	byUnit := map[string][]*platform.Event{}
	for _, ev := range events {
		if ev.Type == platform.EventConversion && ev.Variant == arm.Variant {
			byUnit[ev.UnitHash] = append(byUnit[ev.UnitHash], ev)
		}
	}
	for k, exposureAt := range exposed {
		if k.variant != arm.Variant {
			continue
		}
		cohortAt := exposureAt
		if spec.CohortKey != "" {
			cohortAt = time.Time{}
			for _, ev := range byUnit[k.unit] {
				if ev.Type == platform.EventConversion && ev.Variant == k.variant && ev.UnitHash == k.unit &&
					ev.MetricKey == spec.CohortKey && !ev.ClientTime.Before(exposureAt) && ev.ClientTime.Before(window.End) {
					cohortAt = ev.ClientTime
					break
				}
			}
			if cohortAt.IsZero() {
				arm.Excluded["missing_cohort_event"]++
				continue
			}
		}
		local := cohortAt.In(loc)
		start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, spec.Day)
		end := start.AddDate(0, 0, 1)
		if window.Watermark.Before(end) {
			arm.Immature++
			arm.Excluded["immature_cohort"]++
			continue
		}
		arm.Denominator++
		found := false
		for _, ev := range byUnit[k.unit] {
			if ev.Type == platform.EventConversion && ev.Variant == k.variant && ev.UnitHash == k.unit &&
				ev.MetricKey == spec.ReturnKey && !ev.ClientTime.Before(start) && ev.ClientTime.Before(end) && !ev.ClientTime.Before(cohortAt) {
				found = true
				break
			}
		}
		if found {
			arm.Numerator++
		} else {
			arm.Excluded["no_return_in_window"]++
		}
	}
}

func journeyStats(section JourneySection, control, treatment string) (stats.ProportionStats, stats.ProportionStats) {
	var a, b stats.ProportionStats
	for _, arm := range section.Arms {
		value := stats.ProportionStats{N: arm.Denominator, Conversions: arm.Numerator}
		if arm.Variant == control {
			a = value
		}
		if arm.Variant == treatment {
			b = value
		}
	}
	return a, b
}

func journeyGuardrail(section JourneySection, g platform.GuardrailSpec, control, treatment string) stats.GuardrailCheck {
	a, b := journeyStats(section, control, treatment)
	check := stats.GuardrailCheck{Metric: g.MetricKey}
	if a.N < g.MinSample || b.N < g.MinSample {
		check.Detail = fmt.Sprintf("journey denominator below min sample (%d/%d < %d)", a.N, b.N, g.MinSample)
		return check
	}
	av := float64(a.Conversions) / float64(a.N)
	bv := float64(b.Conversions) / float64(b.N)
	if av == 0 {
		check.Detail = "journey baseline zero; relative guardrail undefined"
		return check
	}
	regression := (av - bv) / av
	if g.Direction == "decrease" {
		regression = -regression
	}
	check.Breached = regression > g.MaxRelativeRegression
	check.Detail = fmt.Sprintf("journey relative regression %.2f%% (control %d/%d, treatment %d/%d)", regression*100, a.Conversions, a.N, b.Conversions, b.N)
	return check
}
