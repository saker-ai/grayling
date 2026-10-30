package growth

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
)

const ReviewVerificationSchemaVersion = "growth.review-verification.v1"

// MaxReviewRecordBytes limits exported review JSON, including whitespace, to 128 KiB.
const MaxReviewRecordBytes = 128 << 10

// Blockers are historical descriptions, not executable instructions or a new
// readiness rule set. Bound both count and individual field/code strings.
const MaxReviewBlockers = 256
const MaxReviewBlockerTextBytes = 256

var ErrReviewVerification = errors.New("growth: invalid or inconsistent review artifacts")

// ReviewVerification reports consistency only. RecordedReadiness is copied from
// the historical record; no evidence envelope is reconstructed or revalidated.
type ReviewVerification struct {
	SchemaVersion         string         `json:"schemaVersion"`
	WorkspaceID           string         `json:"workspaceId"`
	EnvironmentID         string         `json:"environmentId"`
	ProposalID            string         `json:"proposalId"`
	ProposalVersion       int64          `json:"proposalVersion"`
	ProposalFingerprint   string         `json:"proposalFingerprint"`
	Decision              ReviewDecision `json:"decision"`
	RecordedReadiness     ProposalState  `json:"recordedReadiness"`
	PublicationAuthorized bool           `json:"publicationAuthorized"`
}

// DecodeReviewRecord rejects unknown/duplicate members at every depth, invalid
// UTF-8, trailing JSON and null roots. The actual read is bounded to 128 KiB.
func DecodeReviewRecord(r io.Reader) (*ReviewRecord, error) {
	if r == nil {
		return nil, ErrReviewVerification
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxReviewRecordBytes+1))
	if err != nil || len(data) > MaxReviewRecordBytes {
		return nil, ErrReviewVerification
	}
	var record *ReviewRecord
	if json.Unmarshal(data, &record, json.RejectUnknownMembers(true)) != nil || record == nil || validateReviewRecord(*record) != nil {
		return nil, ErrReviewVerification
	}
	return record, nil
}

func validateReviewRecord(r ReviewRecord) error {
	data, err := json.Marshal(r)
	if err != nil || len(data) > MaxReviewRecordBytes || r.SchemaVersion != ReviewRecordSchemaVersion || !validSegment(r.ProposalID) || r.ProposalVersion < 1 || !validSegment(r.WorkspaceID) || !validSegment(r.EnvironmentID) || !fingerprint.MatchString(r.ProposalFingerprint) {
		return ErrReviewVerification
	}
	if validateReviewInputs(ReviewInputs{Decision: r.Decision, Reviewer: r.Reviewer, ReviewedAt: r.ReviewedAt, Rationale: r.Rationale}) != nil {
		return ErrReviewVerification
	}
	switch r.Readiness.State {
	case ProposalReadyForReview:
		if len(r.Readiness.Blockers) != 0 {
			return ErrReviewVerification
		}
	case ProposalDraft, ProposalNeedsData, ProposalNeedsImplementation:
		if len(r.Readiness.Blockers) == 0 || len(r.Readiness.Blockers) > MaxReviewBlockers {
			return ErrReviewVerification
		}
	default:
		return ErrReviewVerification
	}
	if r.Decision == ReviewApprove && r.Readiness.State != ProposalReadyForReview {
		return ErrReviewVerification
	}
	for _, b := range r.Readiness.Blockers {
		if !learningText(b.Field, MaxReviewBlockerTextBytes) || !learningText(b.Code, MaxReviewBlockerTextBytes) {
			return ErrReviewVerification
		}
	}
	return nil
}

// VerifyReview is pure and requires explicit scope. It compares the complete
// recomputed proposal fingerprint and all copied metric/evidence fields in order,
// including windows. Incomplete proposals and negative decisions remain valid
// historical artifacts. This does not authenticate reviewers, prove historical
// readiness or grant any submission/publication permission.
func VerifyReview(p Proposal, r ReviewRecord, workspace, environment string) (*ReviewVerification, error) {
	if !validSegment(workspace) || !validSegment(environment) || validateReviewRecord(r) != nil || p.SchemaVersion != ProposalSchemaVersion || !validSegment(p.ID) || p.Version < 1 {
		return nil, ErrReviewVerification
	}
	fp, err := ProposalFingerprint(p)
	if err != nil || fp != r.ProposalFingerprint || p.ID != r.ProposalID || p.Version != r.ProposalVersion || p.WorkspaceID != workspace || r.WorkspaceID != workspace || p.EnvironmentID != environment || r.EnvironmentID != environment {
		return nil, ErrReviewVerification
	}
	// Compare serialized values to preserve ordering and null/empty distinctions,
	// without relying on the record's fingerprint string for copied-field integrity.
	original, err := json.Marshal([]any{p.PrimaryMetric, p.Guardrails, p.EvidenceRefs}, json.Deterministic(true))
	if err != nil {
		return nil, ErrReviewVerification
	}
	copied, err := json.Marshal([]any{r.PrimaryMetric, r.Guardrails, r.EvidencePins}, json.Deterministic(true))
	if err != nil || !bytes.Equal(original, copied) {
		return nil, ErrReviewVerification
	}
	return &ReviewVerification{SchemaVersion: ReviewVerificationSchemaVersion, WorkspaceID: workspace, EnvironmentID: environment, ProposalID: p.ID, ProposalVersion: p.Version, ProposalFingerprint: fp, Decision: r.Decision, RecordedReadiness: r.Readiness.State}, nil
}
