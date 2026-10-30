package growth

import "time"

// PreparationReviewProvenance is bounded caller-attested linkage. It omits
// rationale, copied evidence and historical blockers, and grants no permission.
// The review covers the proposal only, not separately supplied settings.
type PreparationReviewProvenance struct {
	Verification ReviewVerification `json:"verification"`
	Reviewer     string             `json:"reviewer"`
	ReviewedAt   time.Time          `json:"reviewedAt"`
}

// ReviewedPreparation keeps review provenance outside the unchanged create DTO.
type ReviewedPreparation struct {
	PreparedExperiment
	ReviewProvenance PreparationReviewProvenance `json:"reviewProvenance"`
}

// VerifyPreparationReview is pure preflight for callers before evidence access.
// Consistent reject/request_changes artifacts cannot link preparation.
func VerifyPreparationReview(p Proposal, r ReviewRecord, workspace, environment string) (*PreparationReviewProvenance, error) {
	v, err := VerifyReview(p, r, workspace, environment)
	if err != nil || r.Decision != ReviewApprove {
		return nil, ErrReviewVerification
	}
	return &PreparationReviewProvenance{Verification: *v, Reviewer: r.Reviewer, ReviewedAt: r.ReviewedAt.UTC().Round(0)}, nil
}

// CompileReviewedCreateRequest is pure. Fresh evidence must be supplied separately
// by trusted caller code. Historical readiness is not reestablished; current
// readiness and every settings/compiler restriction remain CompileCreateRequest's
// responsibility. This does not authenticate reviewers or authorize submission.
func CompileReviewedCreateRequest(p Proposal, r ReviewRecord, workspace, environment string, evidence *Envelope, in PreparationInputs) (*ReviewedPreparation, error) {
	provenance, err := VerifyPreparationReview(p, r, workspace, environment)
	if err != nil {
		return nil, err
	}
	prepared, err := CompileCreateRequest(p, evidence, in)
	if err != nil {
		return nil, err
	}
	return &ReviewedPreparation{PreparedExperiment: *prepared, ReviewProvenance: *provenance}, nil
}
