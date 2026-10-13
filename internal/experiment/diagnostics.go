package experiment

import (
	"fmt"
	"strings"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/internal/stats"
)

// FlowVariant summarizes distinct randomization units without exposing their IDs.
type FlowVariant struct {
	Variant               string `json:"variant"`
	Assignments           int64  `json:"assignments"`
	RandomizedAssignments int64  `json:"randomizedAssignments"`
	Exposures             int64  `json:"exposures"`
	RandomizedExposures   int64  `json:"randomizedExposures"`
	Unexposed             int64  `json:"unexposed"`
	Conversions           int64  `json:"conversions"`
	DuplicateExposures    int64  `json:"duplicateExposures"`
}

// Diagnostics separates allocation health from actual delivery health.
// All exported counts are aggregate; unit hashes and decision IDs stay private.
type Diagnostics struct {
	ExperimentID       string                        `json:"experimentId"`
	Epoch              int64                         `json:"epoch"`
	ExposureMode       string                        `json:"exposureMode"`
	GeneratedAt        time.Time                     `json:"generatedAt"`
	Window             platform.AnalysisWindowStatus `json:"window"`
	PerVariant         []FlowVariant                 `json:"perVariant"`
	AssignmentSRM      *stats.SRMResult              `json:"assignmentSrm,omitempty"`
	ExposureSRM        *stats.SRMResult              `json:"exposureSrm,omitempty"`
	CrossVariantUnits  int64                         `json:"crossVariantUnits"`
	AssignmentMismatch int64                         `json:"assignmentMismatch"`
	DuplicateEvents    int64                         `json:"duplicateEvents"`
	RejectedByReason   map[string]int64              `json:"rejectedByReason,omitempty"`
	LastCollectedAt    *time.Time                    `json:"lastCollectedAt,omitempty"`
	Issues             []string                      `json:"issues,omitempty"`
}

// BuildDiagnostics reads the same replayable analysis population as reports.
func BuildDiagnostics(store *platform.Store, experimentID string, now time.Time) (*Diagnostics, error) {
	exp, err := store.GetExperiment(experimentID)
	if err != nil {
		return nil, err
	}
	if exp.AllocationMode == "bandit" {
		report, err := BuildBanditReport(store, exp, now)
		if err != nil {
			return nil, err
		}
		return report.Diagnostics, nil
	}
	analysis, window, err := store.AnalysisStore(exp, now)
	if err != nil {
		return nil, err
	}
	d := buildDiagnostics(exp, analysis.EventsForExperiment(exp.WorkspaceID, exp.EnvironmentID,
		exp.ID, exp.Epoch, ""), window, now)
	if err := attachCollectionCounts(store, exp, d); err != nil {
		return nil, err
	}
	return d, nil
}

func attachCollectionCounts(store *platform.Store, exp *platform.Experiment, d *Diagnostics) error {
	counts, err := store.CollectionCounts(exp.ID)
	if err != nil {
		return err
	}
	d.DuplicateEvents = counts["duplicate_event"]
	delete(counts, "duplicate_event")
	if len(counts) > 0 {
		d.RejectedByReason = counts
	}
	return nil
}

func buildDiagnostics(exp *platform.Experiment, events []*platform.Event,
	window platform.AnalysisWindowStatus, now time.Time) *Diagnostics {
	d := &Diagnostics{ExperimentID: exp.ID, Epoch: exp.Epoch, ExposureMode: exp.ExposureMode,
		GeneratedAt: now.UTC(), Window: window}
	if d.ExposureMode == "" {
		d.ExposureMode = "onDecision"
	}
	type unitKey struct{ variant, unit string }
	assignmentsByID := make(map[string]*platform.Event)
	assigned := make(map[unitKey]bool)
	randomizedAssigned := make(map[unitKey]bool)
	exposed := make(map[unitKey]bool)
	randomizedExposed := make(map[unitKey]bool)
	converted := make(map[unitKey]bool)
	crossVariant := make(map[string]string)
	crossUnits := make(map[string]bool)
	perVariant := make(map[string]*FlowVariant, len(exp.Variants))
	for _, variant := range exp.Variants {
		perVariant[variant] = &FlowVariant{Variant: variant}
	}
	for _, ev := range events {
		if ev.Type == platform.EventAssignment {
			assignmentsByID[ev.EventID] = ev
		}
	}
	inWindow := func(ev *platform.Event) bool {
		return !ev.ClientTime.Before(window.Start) && ev.ClientTime.Before(window.End) &&
			!ev.ClientTime.After(now)
	}
	for _, ev := range events {
		if ev.ServerTime.After(time.Time{}) && (d.LastCollectedAt == nil || ev.ServerTime.After(*d.LastCollectedAt)) {
			at := ev.ServerTime
			d.LastCollectedAt = &at
		}
		if !inWindow(ev) {
			continue
		}
		key := unitKey{ev.Variant, ev.UnitHash}
		flow := perVariant[ev.Variant]
		if flow == nil {
			continue
		}
		switch ev.Type {
		case platform.EventAssignment:
			if !assigned[key] {
				flow.Assignments++
				assigned[key] = true
			}
			if ev.Source == "split" || ev.Source == "bandit" || ev.Source == "bandit-fallback" {
				if !randomizedAssigned[key] {
					flow.RandomizedAssignments++
					randomizedAssigned[key] = true
				}
			}
		case platform.EventExposure:
			if exposed[key] {
				flow.DuplicateExposures++
			} else {
				flow.Exposures++
				exposed[key] = true
			}
			if ev.Source == "split" || ev.Source == "bandit" || ev.Source == "bandit-fallback" {
				if !randomizedExposed[key] {
					flow.RandomizedExposures++
					randomizedExposed[key] = true
				}
			}
			if first, exists := crossVariant[ev.UnitHash]; exists && first != ev.Variant {
				crossUnits[ev.UnitHash] = true
			} else {
				crossVariant[ev.UnitHash] = ev.Variant
			}
			if d.ExposureMode == "onRender" {
				decisionID, valid := strings.CutPrefix(ev.EventID, "exp-")
				assignment := assignmentsByID["asg-"+decisionID]
				if !valid || assignment == nil || assignment.Variant != ev.Variant ||
					assignment.UnitHash != ev.UnitHash || assignment.FlagKey != ev.FlagKey ||
					assignment.FlagRevision != ev.FlagRevision || assignment.Source != ev.Source ||
					assignment.PolicyVersion != ev.PolicyVersion || assignment.StateVersion != ev.StateVersion ||
					assignment.AllocationProbability != ev.AllocationProbability {
					d.AssignmentMismatch++
				}
			}
		case platform.EventConversion:
			if !converted[key] {
				flow.Conversions++
				converted[key] = true
			}
		}
	}
	d.CrossVariantUnits = int64(len(crossUnits))
	for key := range assigned {
		if !exposed[key] {
			perVariant[key.variant].Unexposed++
		}
	}
	assignmentCounts := make([]int64, 0, len(exp.Variants))
	exposureCounts := make([]int64, 0, len(exp.Variants))
	expected := make([]float64, 0, len(exp.Variants))
	var unmatched int64
	for _, variant := range exp.Variants {
		v := perVariant[variant]
		unmatched += v.Unexposed
		d.PerVariant = append(d.PerVariant, *v)
		assignmentCounts = append(assignmentCounts, v.RandomizedAssignments)
		exposureCounts = append(exposureCounts, v.RandomizedExposures)
		expected = append(expected, exp.Proportions[variant]/100)
	}
	if exp.AllocationMode != "bandit" && d.ExposureMode == "onRender" {
		if enoughSRMSample(assignmentCounts, expected) {
			if result, err := stats.SRMTest(assignmentCounts, expected, stats.DefaultSRMThreshold); err == nil {
				d.AssignmentSRM = &result
				if !result.Healthy {
					d.Issues = append(d.Issues, "assignment SRM alarm")
				}
			}
		}
		if enoughSRMSample(exposureCounts, expected) {
			if result, err := stats.SRMTest(exposureCounts, expected, stats.DefaultSRMThreshold); err == nil {
				d.ExposureSRM = &result
				if !result.Healthy {
					d.Issues = append(d.Issues, "exposure SRM alarm")
				}
			}
		}
	}
	if d.CrossVariantUnits > 0 {
		d.Issues = append(d.Issues, fmt.Sprintf("%d units exposed to multiple variants", d.CrossVariantUnits))
	}
	if d.AssignmentMismatch > 0 {
		d.Issues = append(d.Issues, fmt.Sprintf("%d exposures differ from recorded assignments", d.AssignmentMismatch))
	}
	if d.ExposureMode == "onRender" && unmatched > 0 {
		d.Issues = append(d.Issues, fmt.Sprintf("%d assigned units have no recorded exposure; manual review required", unmatched))
	}
	return d
}

func enoughSRMSample(counts []int64, proportions []float64) bool {
	var total int64
	for _, count := range counts {
		total += count
	}
	if total == 0 {
		return false
	}
	for _, proportion := range proportions {
		if float64(total)*proportion < 5 {
			return false
		}
	}
	return true
}
