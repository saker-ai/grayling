package growth

import (
	"bytes"
	"encoding/json/v2"
)

const LearningVerificationSchemaVersion = "growth.learning-verification.v1"

// LearningVerification is a bounded local consistency result. It authenticates
// neither authorship nor execution and establishes no freshness or causality.
// Status is the supplied observation's status, never an experiment win.
type LearningVerification struct {
	SchemaVersion          string `json:"schemaVersion"`
	WorkspaceID            string `json:"workspaceId"`
	EnvironmentID          string `json:"environmentId"`
	ProposalID             string `json:"proposalId"`
	ProposalVersion        int64  `json:"proposalVersion"`
	ProposalFingerprint    string `json:"proposalFingerprint"`
	ExperimentID           string `json:"experimentId"`
	Epoch                  int64  `json:"epoch"`
	EvidenceVersion        string `json:"evidenceVersion"`
	ObservationStatus      Status `json:"observationStatus"`
	PublicationAuthorized  bool   `json:"publicationAuthorized"`
	BusinessWinEstablished bool   `json:"businessWinEstablished"`
}

// VerifyLearning checks caller-supplied artifacts against an explicit scope.
// It is pure: no Reader, credentials, clock, network, persistence or decisions.
// Every failure returns a nil summary and a fixed diagnostic. HOLD/NEEDS_DATA
// are valid observations; verification does not clear their blockers.
func VerifyLearning(p Proposal, r LearningRecord, workspace, environment string) (*LearningVerification, error) {
	if !validSegment(workspace) || !validSegment(environment) || validateLearningRecord(r) != nil {
		return nil, ErrLearning
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > MaxLearningRecordBytes {
		return nil, ErrLearning
	}
	if p.SchemaVersion != ProposalSchemaVersion || !validSegment(p.ID) || p.Version < 1 || !learningText(p.Hypothesis, 4096) {
		return nil, ErrLearning
	}
	fp, err := ProposalFingerprint(p)
	if err != nil || fp != r.ProposalFingerprint || p.ID != r.ProposalID || p.Version != r.ProposalVersion || p.WorkspaceID != workspace || r.WorkspaceID != workspace || p.EnvironmentID != environment || r.EnvironmentID != environment || p.Hypothesis != r.OriginalHypothesis {
		return nil, ErrLearning
	}
	// Compare all copied baseline fields in order, including windows. Baselines
	// need not be the linked experiment and must never be compared as if they were.
	baseline, err := json.Marshal(p.EvidenceRefs, json.Deterministic(true))
	if err != nil {
		return nil, ErrLearning
	}
	copied, err := json.Marshal(r.ProposalBaselineReferences, json.Deterministic(true))
	if err != nil || !bytes.Equal(baseline, copied) {
		return nil, ErrLearning
	}
	guards := make([]MetricRef, len(p.Guardrails))
	for i, g := range p.Guardrails {
		guards[i] = g.Ref
	}
	if p.PrimaryMetric.Ref != r.LinkedExperiment.PrimaryMetric || !exactMonitorGuards(guards, r.LinkedExperiment.Guardrails) {
		return nil, ErrLearning
	}
	return &LearningVerification{
		SchemaVersion: LearningVerificationSchemaVersion, WorkspaceID: workspace, EnvironmentID: environment,
		ProposalID: p.ID, ProposalVersion: p.Version, ProposalFingerprint: fp,
		ExperimentID: r.LinkedExperiment.ExperimentID, Epoch: r.LinkedExperiment.Epoch,
		EvidenceVersion: r.Observation.EvidenceVersion, ObservationStatus: r.Observation.Status,
	}, nil
}
