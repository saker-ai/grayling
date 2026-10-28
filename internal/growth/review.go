package growth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const ReviewRecordSchemaVersion = "growth.review.v1"
const MaxReviewRationaleBytes = 4096

type ReviewDecision string

const (
	ReviewApprove        ReviewDecision = "approve"
	ReviewReject         ReviewDecision = "reject"
	ReviewRequestChanges ReviewDecision = "request_changes"
)

// ReviewInputs must be supplied explicitly by trusted caller code, separately
// from proposal/model data. Reviewer is a caller-attested identity, not a token
// or an authenticated principal. No values are inferred from proposal prose.
type ReviewInputs struct {
	ExpectedProposalFingerprint string         `json:"expectedProposalFingerprint"`
	Decision                    ReviewDecision `json:"decision"`
	Reviewer                    string         `json:"reviewer"`
	ReviewedAt                  time.Time      `json:"reviewedAt"`
	Rationale                   string         `json:"rationale"`
}

// ReviewRecord is a caller-attested review artifact, not authenticated
// authorization, durable audit storage, or permission to submit or publish.
// Readiness and evidence pins describe this invocation's supplied snapshot.
type ReviewRecord struct {
	SchemaVersion       string                `json:"schemaVersion"`
	ProposalID          string                `json:"proposalId"`
	ProposalVersion     int64                 `json:"proposalVersion"`
	WorkspaceID         string                `json:"workspaceId"`
	EnvironmentID       string                `json:"environmentId"`
	ProposalFingerprint string                `json:"proposalFingerprint"`
	PrimaryMetric       ProposalMetric        `json:"primaryMetric"`
	Guardrails          []ProposalMetric      `json:"guardrails"`
	EvidencePins        []ProposalEvidenceRef `json:"evidencePins"`
	Reviewer            string                `json:"reviewer"`
	ReviewedAt          time.Time             `json:"reviewedAt"`
	Decision            ReviewDecision        `json:"decision"`
	Rationale           string                `json:"rationale"`
	Readiness           ProposalReview        `json:"readiness"`
}

// ReviewError uses fixed field/code values and never echoes caller text.
// Every error returns a nil record, including failed approval readiness.
type ReviewError struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

func (e *ReviewError) Error() string { return "growth: review " + e.Field + ": " + e.Code }

// canonicalProposal includes every serialized Proposal field, preserving array
// order, null/empty distinctions and exact integers (including int64 versions).
// It uses RFC 8785 formatting with integer rounding disabled to avoid collisions
// above 2^53. No fields are dropped and no human text is interpreted.
func canonicalProposal(p Proposal) ([]byte, error) {
	data, err := json.Marshal(p, json.Deterministic(true))
	if err != nil || len(data) > MaxProposalBytes {
		return nil, &ReviewError{Field: "proposal", Code: "too_large_or_invalid"}
	}
	value := jsontext.Value(data)
	if err := value.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, &ReviewError{Field: "proposal", Code: "too_large_or_invalid"}
	}
	return value, nil
}

// ProposalFingerprint returns sha256:<lowercase hex> of the complete canonical
// proposal JSON. It can pin an incomplete draft; it does not establish readiness.
func ProposalFingerprint(p Proposal) (string, error) {
	data, err := canonicalProposal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// RecordReview is pure: no clock, randomness, authentication, storage or writes.
// Evidence must come separately from Reader.Read via trusted caller code, never
// from model JSON. Approval requires the existing ReviewProposal readiness result;
// rejection/changes can record missing or HOLD evidence without clearing blockers.
func RecordReview(p Proposal, evidence *Envelope, in ReviewInputs) (*ReviewRecord, error) {
	fail := func(field, code string) (*ReviewRecord, error) {
		return nil, &ReviewError{Field: field, Code: code}
	}
	if !fingerprint.MatchString(in.ExpectedProposalFingerprint) {
		return fail("expectedProposalFingerprint", "invalid_fingerprint")
	}
	actual, err := ProposalFingerprint(p)
	if err != nil {
		return nil, err
	}
	if actual != in.ExpectedProposalFingerprint {
		return fail("expectedProposalFingerprint", "stale_proposal")
	}
	if p.SchemaVersion != ProposalSchemaVersion {
		return fail("proposal.schemaVersion", "unsupported_version")
	}
	for _, f := range []struct{ field, value string }{
		{"proposal.id", p.ID}, {"proposal.workspaceId", p.WorkspaceID}, {"proposal.environmentId", p.EnvironmentID},
	} {
		if !validSegment(f.value) {
			return fail(f.field, "invalid_identifier")
		}
	}
	if p.Version < 1 {
		return fail("proposal.version", "positive_required")
	}
	switch in.Decision {
	case ReviewApprove, ReviewReject, ReviewRequestChanges:
	default:
		return fail("decision", "invalid_decision")
	}
	if !validSegment(in.Reviewer) {
		return fail("reviewer", "invalid_identifier")
	}
	reviewedAt := in.ReviewedAt.UTC().Round(0)
	if in.ReviewedAt.IsZero() || reviewedAt.Year() < 1 || reviewedAt.Year() > 9999 {
		return fail("reviewedAt", "invalid_timestamp")
	}
	if strings.TrimSpace(in.Rationale) == "" || len(in.Rationale) > MaxReviewRationaleBytes || !utf8.ValidString(in.Rationale) {
		return fail("rationale", "required_or_invalid")
	}
	for _, r := range in.Rationale {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return fail("rationale", "required_or_invalid")
		}
	}
	// Decode a fresh canonical snapshot to detach every nested slice, including
	// window issues. This also makes returned timestamp representations replayable.
	data, err := canonicalProposal(p)
	if err != nil {
		return nil, err
	}
	var snapshot Proposal
	if json.Unmarshal(data, &snapshot) != nil {
		return fail("proposal", "too_large_or_invalid")
	}
	readiness := ReviewProposal(snapshot, evidence)
	if in.Decision == ReviewApprove && readiness.State != ProposalReadyForReview {
		return fail("proposal", "not_ready_for_review")
	}
	return &ReviewRecord{
		SchemaVersion: ReviewRecordSchemaVersion, ProposalID: snapshot.ID, ProposalVersion: snapshot.Version,
		WorkspaceID: snapshot.WorkspaceID, EnvironmentID: snapshot.EnvironmentID, ProposalFingerprint: actual,
		PrimaryMetric: snapshot.PrimaryMetric, Guardrails: snapshot.Guardrails, EvidencePins: snapshot.EvidenceRefs,
		Reviewer: in.Reviewer, ReviewedAt: reviewedAt, Decision: in.Decision, Rationale: in.Rationale,
		Readiness: readiness,
	}, nil
}
