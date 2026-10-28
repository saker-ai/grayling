package growth

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const LearningRecordSchemaVersion = "growth.learning.v1"
const MaxLearningFollowupBytes = 4096
const MaxLearningFollowups = 20

var ErrLearning = errors.New("growth: invalid learning inputs")

// LearningInputs are explicit caller attestations, never authenticated identity
// or proof that a follow-up action occurred. Text is retained as inert data.
type LearningInputs struct {
	ExpectedProposalFingerprint string    `json:"expectedProposalFingerprint"`
	Recorder                    string    `json:"recorder"`
	RecordedAt                  time.Time `json:"recordedAt"`
	Followups                   []string  `json:"followups"`
}

// LearningRecord links an original hypothesis to a read-only monitor snapshot.
// Baseline references are proposal claims, not observations of the linked run.
// LinkedExperiment is also a caller attestation, not proof of proposal execution.
// Server verdicts describe platform decisions, never a business winning claim.
// Records provide neither durable audit storage nor publication authorization.
type LearningRecord struct {
	SchemaVersion              string                `json:"schemaVersion"`
	ProposalID                 string                `json:"proposalId"`
	ProposalVersion            int64                 `json:"proposalVersion"`
	ProposalFingerprint        string                `json:"proposalFingerprint"`
	WorkspaceID                string                `json:"workspaceId"`
	EnvironmentID              string                `json:"environmentId"`
	OriginalHypothesis         string                `json:"originalHypothesis"`
	ProposalBaselineReferences []ProposalEvidenceRef `json:"proposalBaselineReferences"`
	LinkedExperiment           MonitorManifest       `json:"linkedExperiment"`
	Observation                MonitorSummary        `json:"observation"`
	Recorder                   string                `json:"recorder"`
	RecordedAt                 time.Time             `json:"recordedAt"`
	Followups                  []string              `json:"followups"`
	BusinessWinEstablished     bool                  `json:"businessWinEstablished"`
	PublicationAuthorized      bool                  `json:"publicationAuthorized"`
}

// RecordLearning validates all local pins before network access, then reuses
// Monitor without adding health/statistical decisions. Errors return no record.
// No clock, random ID, retries, persistence or HTTP writes are introduced.
func (r *Reader) RecordLearning(ctx context.Context, p Proposal, m MonitorManifest, in LearningInputs) (*LearningRecord, error) {
	if !fingerprint.MatchString(in.ExpectedProposalFingerprint) {
		return nil, ErrLearning
	}
	actual, err := ProposalFingerprint(p)
	if err != nil {
		return nil, err
	}
	if actual != in.ExpectedProposalFingerprint {
		return nil, ErrLearning
	}
	if p.SchemaVersion != ProposalSchemaVersion || !validSegment(p.ID) || p.Version < 1 || !validSegment(p.WorkspaceID) || !validSegment(p.EnvironmentID) || strings.TrimSpace(p.Hypothesis) == "" || len(p.Hypothesis) > 4096 {
		return nil, ErrLearning
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	if p.WorkspaceID != m.WorkspaceID || p.EnvironmentID != m.EnvironmentID || m.WorkspaceID != r.workspace || m.EnvironmentID != r.environment || p.PrimaryMetric.Ref != m.PrimaryMetric {
		return nil, ErrMismatch
	}
	guards := make([]MetricRef, len(p.Guardrails))
	for i, g := range p.Guardrails {
		guards[i] = g.Ref
	}
	if !exactMonitorGuards(guards, m.Guardrails) {
		return nil, ErrMismatch
	}
	recordedAt := in.RecordedAt.UTC().Round(0)
	if !validSegment(in.Recorder) || in.RecordedAt.IsZero() || recordedAt.Year() < 1 || recordedAt.Year() > 9999 || len(in.Followups) < 1 || len(in.Followups) > MaxLearningFollowups {
		return nil, ErrLearning
	}
	for _, s := range in.Followups {
		if strings.TrimSpace(s) == "" || len(s) > MaxLearningFollowupBytes || !utf8.ValidString(s) {
			return nil, ErrLearning
		}
		for _, c := range s {
			if unicode.IsControl(c) && c != '\n' && c != '\t' {
				return nil, ErrLearning
			}
		}
	}
	// Snapshot nested inputs before the read, including baseline window issues.
	data, err := canonicalProposal(p)
	if err != nil {
		return nil, err
	}
	var snapshot Proposal
	if json.Unmarshal(data, &snapshot) != nil {
		return nil, ErrLearning
	}
	m.Guardrails = append([]MetricRef{}, m.Guardrails...)
	followups := append([]string{}, in.Followups...)
	observed, err := r.Monitor(ctx, m)
	if err != nil {
		return nil, err
	}
	if observed.EvidenceVersion == "" {
		// Monitor's constructed source path is a request target, not observed provenance.
		observed.SourcePath = ""
	}
	return &LearningRecord{
		SchemaVersion: LearningRecordSchemaVersion, ProposalID: snapshot.ID, ProposalVersion: snapshot.Version,
		ProposalFingerprint: actual, WorkspaceID: snapshot.WorkspaceID, EnvironmentID: snapshot.EnvironmentID,
		OriginalHypothesis: snapshot.Hypothesis, ProposalBaselineReferences: snapshot.EvidenceRefs,
		LinkedExperiment: m, Observation: *observed, Recorder: in.Recorder, RecordedAt: recordedAt, Followups: followups,
	}, nil
}
