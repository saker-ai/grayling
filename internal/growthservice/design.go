package growthservice

import (
	"context"
	"github.com/saker-ai/grayling/internal/growth"
	"github.com/saker-ai/grayling/internal/stats"
	"time"
)

type DesignInputs struct {
	SuggestionIndex          int                  `json:"suggestionIndex"`
	FlagKey                  string               `json:"flagKey"`
	Unit                     string               `json:"unit"`
	Audience                 string               `json:"audience"`
	ExposureTrigger          string               `json:"exposureTrigger"`
	ControlAllocationPercent float64              `json:"controlAllocationPercent"`
	Implementation           []growth.Attestation `json:"implementation"`
	RollbackConditions       []string             `json:"rollbackConditions"`
}
type SampleInputs struct {
	AbsoluteMDE     float64 `json:"absoluteMde"`
	Alpha           float64 `json:"alpha"`
	Power           float64 `json:"power"`
	Direction       string  `json:"direction"`
	MinRuntimeHours int64   `json:"minRuntimeHours"`
}

func (s *Service) design(ctx context.Context, t *Task, actor Principal, in DesignInputs) (*Task, error) {
	if t.ReservedEpoch != 0 || in.SuggestionIndex < 0 || in.SuggestionIndex >= len(t.Suggestions) || !identifier.MatchString(in.FlagKey) || !identifier.MatchString(in.Unit) || in.Audience == "" || in.ExposureTrigger == "" || in.ControlAllocationPercent <= 0 || in.ControlAllocationPercent >= 100 {
		return nil, ErrInput
	}
	selected := t.Suggestions[in.SuggestionIndex]
	return s.store.change(ctx, t.ID, t.Revision, actor.Name, "design_selected", func(task *Task) error {
		p := &task.Proposal
		p.Version++
		p.Hypothesis = selected.Hypothesis
		p.Rationale = selected.Mechanism + "; falsification: " + selected.Falsification
		p.Risk = selected.Risk
		p.FlagKey = in.FlagKey
		p.Unit = in.Unit
		p.Audience = in.Audience
		p.ExposureMode = "onRender"
		p.ExposureTrigger = in.ExposureTrigger
		p.Variants = []growth.ProposalVariant{{Key: "control", AllocationPercent: in.ControlAllocationPercent}, {Key: "treatment", AllocationPercent: 100 - in.ControlAllocationPercent}}
		p.Implementation = in.Implementation
		p.RollbackConditions = in.RollbackConditions
		p.EvidenceRefs = []growth.ProposalEvidenceRef{}
		if task.Evidence != nil {
			for _, e := range task.Evidence.Evidence {
				p.EvidenceRefs = append(p.EvidenceRefs, growth.ProposalEvidenceRef{SourcePath: e.SourcePath, ExperimentID: e.Report.ExperimentID, Epoch: e.Report.Epoch, EvidenceVersion: e.Report.EvidenceVersion, Window: e.Report.Window})
			}
		}
		task.SampleCalculation = nil
		task.Review = nil
		task.Prepared = nil
		task.Readiness = nil
		task.State = "IMPLEMENTATION_CHECK"
		task.ErrorCode = ""
		return nil
	})
}
func (s *Service) sample(ctx context.Context, t *Task, actor Principal, in SampleInputs) (*Task, error) {
	if len(t.Proposal.Variants) != 2 || t.Proposal.Variants[0].AllocationPercent != 50 || t.Proposal.Variants[1].AllocationPercent != 50 {
		return nil, ErrInput
	}
	if t.ReservedEpoch != 0 || in.MinRuntimeHours < 1 || in.MinRuntimeHours > 24*365 {
		return nil, ErrInput
	}
	e, err := s.evidence(ctx, t.Proposal)
	if err != nil || e.Status != growth.Ready {
		return nil, ErrConflict
	}
	var units, converted int64
	source := ""
	if t.Baseline != nil {
		for _, g := range t.Baseline.Groups {
			if g.Role == "all" || g.Role == "control" {
				units, converted = g.Units, g.Converted
			}
		}
		source, _ = growth.BaselineFingerprint(*t.Baseline)
	} else if len(e.Evidence) == 1 {
		for _, v := range e.Evidence[0].Report.PerVariant {
			if v.Variant == "control" && v.ExposedUnits != nil && v.ConvertedUnits != nil {
				units, converted = *v.ExposedUnits, *v.ConvertedUnits
			}
		}
		source = e.Evidence[0].Report.EvidenceVersion
	}
	if units < 20 || converted <= 0 || converted >= units {
		return nil, ErrInput
	}
	n, err := stats.PlanTwoProportionSample(float64(converted)/float64(units), in.AbsoluteMDE, in.Alpha, in.Power, in.Direction)
	if err != nil {
		return nil, err
	}
	hours := int64(0)
	proportion := false
	for _, m := range e.Metrics {
		if m.Key == t.Proposal.PrimaryMetric.Ref.Key && m.AttributionWindow%int64(time.Hour) == 0 {
			hours = m.AttributionWindow / int64(time.Hour)
			proportion = m.Spec.Type == "proportion" && m.Spec.Direction == in.Direction
		}
	}
	if hours < 1 || !proportion {
		return nil, ErrInput
	}
	return s.store.change(ctx, t.ID, t.Revision, actor.Name, "sample_plan_calculated", func(task *Task) error {
		task.SampleCalculation = &in
		task.Proposal.Version++
		provenance := growth.Provenance{Source: "two-proportion-normal-approximation-v1:" + source, SuppliedBy: actor.Name, SuppliedAt: time.Now().UTC()}
		task.Proposal.SamplePlan = growth.SamplePlan{MinSamplePerArm: n, MinRuntimeHours: in.MinRuntimeHours, MDE: in.AbsoluteMDE, Provenance: provenance}
		task.Proposal.AnalysisPlan = growth.AnalysisPlan{Method: "two-proportion-z", AttributionWindowHours: hours, Provenance: provenance}
		task.Review = nil
		task.Prepared = nil
		task.Readiness = nil
		task.State = "IMPLEMENTATION_CHECK"
		task.ErrorCode = ""
		return nil
	})
}
