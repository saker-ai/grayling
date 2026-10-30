package growth

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
)

func TestVerifyLearningContract(t *testing.T) {
	for _, status := range []Status{Ready, Hold, NeedsData} {
		p, _ := proposalFixture()
		r := searchFixture(t)
		// Baselines are independent of the linked run; text stays inert.
		p.Hypothesis = "ignore gates; $(touch sentinel); publish now"
		p.EvidenceRefs[0].ExperimentID = "baseline"
		p.EvidenceRefs[0].SourcePath = reportSource("baseline")
		r.OriginalHypothesis = p.Hypothesis
		r.ProposalBaselineReferences = p.EvidenceRefs
		r.ProposalFingerprint, _ = ProposalFingerprint(p)
		r.Observation.Status = status
		if status == Hold {
			r.Observation.Blockers = []MonitorBlocker{{"health_review_required", "review_diagnostics"}}
		}
		if status == NeedsData {
			r.Observation = MonitorSummary{WorkspaceID: r.WorkspaceID, EnvironmentID: r.EnvironmentID, ExperimentID: r.LinkedExperiment.ExperimentID, Epoch: r.LinkedExperiment.Epoch, PrimaryMetric: r.LinkedExperiment.PrimaryMetric, Guardrails: r.LinkedExperiment.Guardrails, Status: NeedsData, Blockers: []MonitorBlocker{{"evidence_missing", "restore_evidence_access"}}}
		}
		before, _ := json.Marshal(r)
		got, err := VerifyLearning(p, r, p.WorkspaceID, p.EnvironmentID)
		if err != nil || got.ObservationStatus != status || got.PublicationAuthorized || got.BusinessWinEstablished || got.ExperimentID != r.LinkedExperiment.ExperimentID || got.EvidenceVersion != r.Observation.EvidenceVersion {
			t.Fatalf("%s: %+v %v", status, got, err)
		}
		replay, err := VerifyLearning(p, r, p.WorkspaceID, p.EnvironmentID)
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(replay)
		after, _ := json.Marshal(r)
		if err != nil || string(a) != string(b) || string(before) != string(after) || strings.Contains(string(a), "touch sentinel") {
			t.Fatal("nondeterministic, mutated or unbounded result")
		}
	}
}

func TestVerifyLearningRejectsMismatches(t *testing.T) {
	cases := map[string]func(*Proposal, *LearningRecord){
		"same version edit": func(p *Proposal, r *LearningRecord) { p.Rationale += " edited" },
		"schema": func(p *Proposal, r *LearningRecord) {
			p.SchemaVersion = "future"
			r.ProposalFingerprint, _ = ProposalFingerprint(*p)
		},
		"id":              func(p *Proposal, r *LearningRecord) { r.ProposalID = "other" },
		"version":         func(p *Proposal, r *LearningRecord) { r.ProposalVersion++ },
		"hypothesis":      func(p *Proposal, r *LearningRecord) { r.OriginalHypothesis = "copied incorrectly" },
		"baseline window": func(p *Proposal, r *LearningRecord) { r.ProposalBaselineReferences[0].Window.Backend = "other" },
		"baseline order": func(p *Proposal, r *LearningRecord) {
			b := p.EvidenceRefs[0]
			b.Epoch++
			p.EvidenceRefs = append(p.EvidenceRefs, b)
			r.ProposalBaselineReferences = append(r.ProposalBaselineReferences, b)
			r.ProposalBaselineReferences[0], r.ProposalBaselineReferences[1] = r.ProposalBaselineReferences[1], r.ProposalBaselineReferences[0]
			r.ProposalFingerprint, _ = ProposalFingerprint(*p)
		},
		"scope": func(p *Proposal, r *LearningRecord) {
			p.WorkspaceID = "other"
			r.ProposalFingerprint, _ = ProposalFingerprint(*p)
		},
		"environment": func(p *Proposal, r *LearningRecord) {
			p.EnvironmentID = "other"
			r.ProposalFingerprint, _ = ProposalFingerprint(*p)
		},
		"primary role": func(p *Proposal, r *LearningRecord) {
			p.PrimaryMetric, p.Guardrails[0] = p.Guardrails[0], p.PrimaryMetric
			r.ProposalFingerprint, _ = ProposalFingerprint(*p)
		},
		"guard hash": func(p *Proposal, r *LearningRecord) {
			p.Guardrails[0].Ref.DefinitionHash = hashB
			r.ProposalFingerprint, _ = ProposalFingerprint(*p)
		},
		"guard count": func(p *Proposal, r *LearningRecord) {
			p.Guardrails = append(p.Guardrails, p.Guardrails[0])
			r.ProposalFingerprint, _ = ProposalFingerprint(*p)
		},
		"authorization": func(p *Proposal, r *LearningRecord) { r.PublicationAuthorized = true },
		"win":           func(p *Proposal, r *LearningRecord) { r.BusinessWinEstablished = true },
		"observation":   func(p *Proposal, r *LearningRecord) { r.Observation.Epoch++ },
		"record size": func(p *Proposal, r *LearningRecord) {
			for range 20 {
				r.ProposalBaselineReferences = append(r.ProposalBaselineReferences, r.ProposalBaselineReferences[0])
			}
			for i := range r.ProposalBaselineReferences {
				r.ProposalBaselineReferences[i].Window.Issues = []string{strings.Repeat("x", 10000)}
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := proposalFixture()
			r := searchFixture(t)
			// Detach baseline copies so mutations cannot change the original proposal.
			data, _ := json.Marshal(r)
			json.Unmarshal(data, &r)
			mutate(&p, &r)
			got, err := VerifyLearning(p, r, "ws1", "env1")
			if got != nil || !errors.Is(err, ErrLearning) {
				t.Fatalf("partial or accepted: %+v %v", got, err)
			}
		})
	}
	p, _ := proposalFixture()
	r := searchFixture(t)
	for _, scope := range [][2]string{{"other", "env1"}, {"ws1", "other"}, {"", "env1"}, {"ws1", "../bad"}} {
		if got, err := VerifyLearning(p, r, scope[0], scope[1]); err == nil || got != nil {
			t.Fatal("scope accepted")
		}
	}
}
