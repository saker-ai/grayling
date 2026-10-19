package platform

import (
	"fmt"
	"slices"
	"time"

	"github.com/saker-ai/grayling/pkg/model"
)

// Approval is a durable, scoped grant for one exact operation and payload.
// Draft approvals use the same submitted -> approved -> consumed policy while
// retaining their existing draft endpoints for backwards compatibility.
type Approval struct {
	ID               string     `json:"id"`
	WorkspaceID      string     `json:"workspaceId"`
	EnvironmentID    string     `json:"environmentId"`
	Action           string     `json:"action"`
	Object           string     `json:"object"`
	PayloadHash      string     `json:"payloadHash"`
	BaseRevision     int64      `json:"baseRevision"`
	Risk             Risk       `json:"risk"`
	Reason           string     `json:"reason"`
	Impact           string     `json:"impact"`
	RollbackTarget   string     `json:"rollbackTarget"`
	Guardrails       string     `json:"guardrails"`
	Payload          string     `json:"payload,omitempty"` // non-secret execution input only
	State            DraftState `json:"state"`
	SubmittedBy      string     `json:"submittedBy"`
	SubmittedTokenID string     `json:"submittedTokenId"`
	SubmittedAt      time.Time  `json:"submittedAt"`
	ApprovedBy       string     `json:"approvedBy,omitempty"`
	ApprovedTokenID  string     `json:"approvedTokenId,omitempty"`
	ApprovedAt       *time.Time `json:"approvedAt,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
	ReviewNote       string     `json:"reviewNote,omitempty"`
	RejectedBy       string     `json:"rejectedBy,omitempty"`
	RejectedTokenID  string     `json:"rejectedTokenId,omitempty"`
	RejectedAt       *time.Time `json:"rejectedAt,omitempty"`
	RejectionReason  string     `json:"rejectionReason,omitempty"`
	UsedAt           *time.Time `json:"usedAt,omitempty"`
	UsedOperationID  string     `json:"usedOperationId,omitempty"`
	UsedPrincipalID  string     `json:"usedPrincipalId,omitempty"`
}

const (
	ApprovalRejected DraftState = "rejected"
	ApprovalUsed     DraftState = "used"
	// BreakglassReviewAction is the post-hoc review opened after an emergency
	// action (emergency-disable, emergency rollback, AI revoke). It is the one
	// approval the operator may not resolve themselves: an emergency action
	// already skipped the ordinary review, so its aftermath is the only place
	// where an independent reviewer is still required.
	BreakglassReviewAction = "breakglass.review"
)

func copyApproval(a *Approval) *Approval {
	out := *a
	if a.ApprovedAt != nil {
		v := *a.ApprovedAt
		out.ApprovedAt = &v
	}
	if a.ExpiresAt != nil {
		v := *a.ExpiresAt
		out.ExpiresAt = &v
	}
	if a.UsedAt != nil {
		v := *a.UsedAt
		out.UsedAt = &v
	}
	if a.RejectedAt != nil {
		v := *a.RejectedAt
		out.RejectedAt = &v
	}
	return &out
}

func (s *Store) RequestApproval(a Approval) (*Approval, error) {
	if a.WorkspaceID == "" || (a.EnvironmentID == "" && a.Action != "metric.publish") || a.Action == "" || a.Object == "" ||
		a.PayloadHash == "" || a.SubmittedBy == "" || a.SubmittedTokenID == "" || a.Reason == "" {
		return nil, fmt.Errorf("%w: incomplete approval request", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.EnvironmentID == "" {
		if s.data.Workspaces[a.WorkspaceID] == nil {
			return nil, fmt.Errorf("%w: workspace %q", ErrNotFound, a.WorkspaceID)
		}
	} else {
		if err := s.checkEnvLocked(a.WorkspaceID, a.EnvironmentID); err != nil {
			return nil, err
		}
	}
	a.ID = s.nextID("approval")
	a.State = DraftSubmitted
	a.UsedAt = nil
	a.UsedOperationID = ""
	a.UsedPrincipalID = ""
	a.SubmittedAt = s.now()
	a.Risk = RiskHigh
	s.data.Approvals[a.ID] = &a
	s.changes.markDirty(collApprovals, a.ID)
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return copyApproval(&a), nil
}

func (s *Store) GetApproval(id string) (*Approval, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a := s.data.Approvals[id]
	if a == nil {
		return nil, fmt.Errorf("%w: approval %s", ErrNotFound, id)
	}
	return copyApproval(a), nil
}

func (s *Store) ListApprovals(workspaceID string) []*Approval {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Approval, 0)
	for _, a := range s.data.Approvals {
		if a.WorkspaceID == workspaceID {
			out = append(out, copyApproval(a))
		}
	}
	slices.SortFunc(out, func(a, b *Approval) int { return b.SubmittedAt.Compare(a.SubmittedAt) })
	return out
}

// ApproveChange grants one approval. The applicant may approve their own
// request: a deployment with a single administrator must still be able to act.
// Applicant and approver are stored separately, so a self-approval stays
// visible in the audit trail. Emergency post-reviews are the exception —
// independence is the entire point of that review.
func (s *Store) ApproveChange(id, actor, tokenID, note string) (*Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.data.Approvals[id]
	if a == nil {
		return nil, fmt.Errorf("%w: approval %s", ErrNotFound, id)
	}
	if a.State != DraftSubmitted {
		return nil, fmt.Errorf("%w: approval is %s", ErrInvalidState, a.State)
	}
	if a.Action == BreakglassReviewAction && (actor == a.SubmittedBy || tokenID == a.SubmittedTokenID) {
		return nil, ErrSelfApproval
	}
	a.State = DraftApproved
	a.ApprovedBy, a.ApprovedTokenID, a.ReviewNote = actor, tokenID, note
	now := s.now()
	a.ApprovedAt = &now
	expires := now.Add(ApprovalTTL)
	a.ExpiresAt = &expires
	s.changes.markDirty(collApprovals, id)
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return copyApproval(a), nil
}

// RejectChange records a reviewer's reason and prevents any later use. The
// applicant may also reject their own request, which withdraws it.
func (s *Store) RejectChange(id, actor, tokenID, reason string) (*Approval, error) {
	if reason == "" {
		return nil, fmt.Errorf("%w: rejection reason required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.data.Approvals[id]
	if a == nil {
		return nil, fmt.Errorf("%w: approval %s", ErrNotFound, id)
	}
	if a.State != DraftSubmitted {
		return nil, fmt.Errorf("%w: approval is %s", ErrInvalidState, a.State)
	}
	a.State = ApprovalRejected
	a.RejectedBy, a.RejectedTokenID, a.RejectionReason = actor, tokenID, reason
	now := s.now()
	a.RejectedAt = &now
	s.changes.markDirty(collApprovals, id)
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return copyApproval(a), nil
}

// UseApproval atomically consumes one exact grant before the effect executes.
// A failed effect requires a new request, so no stale grant can be replayed.
func (s *Store) UseApproval(id, workspaceID, environmentID, action, object, hash string, baseRevision int64) (*Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.useApprovalLocked(id, workspaceID, environmentID, action, object, hash, baseRevision)
	if err != nil {
		return nil, err
	}
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return copyApproval(a), nil
}

func (s *Store) useApprovalLocked(id, workspaceID, environmentID, action, object, hash string, baseRevision int64) (*Approval, error) {
	a := s.data.Approvals[id]
	if a == nil {
		return nil, fmt.Errorf("%w: approval %s", ErrNotFound, id)
	}
	if a.WorkspaceID != workspaceID || a.EnvironmentID != environmentID || a.Action != action || a.Object != object {
		return nil, fmt.Errorf("%w: approval scope or operation mismatch", ErrAccessDenied)
	}
	if a.State != DraftApproved || a.ExpiresAt == nil || !s.now().Before(*a.ExpiresAt) {
		return nil, fmt.Errorf("%w: approval missing, used or expired", ErrInvalidState)
	}
	if a.PayloadHash != hash || a.BaseRevision != baseRevision {
		return nil, fmt.Errorf("%w: operation changed after review", ErrConflict)
	}
	now := s.now()
	a.State = ApprovalUsed
	a.UsedAt = &now
	s.changes.markDirty(collApprovals, id)
	return a, nil
}

// ApprovalOperation binds a durable approval receipt to the exact effect
// deduplication namespace. Authorization of the caller remains a service duty.
type ApprovalOperation struct {
	ApprovalID, WorkspaceID, EnvironmentID, Action, Object, PayloadHash string
	BaseRevision                                                        int64
	PrincipalID, OperationID                                            string
}
type ApprovalOperationReceipt struct {
	Approval *Approval
	Replayed bool
}

// UseApprovalForOperation consumes a grant once and can recover that same
// receipt after a failed/unknown effect commit. A receipt is not proof that the
// effect committed or that authorization is still live. The effect transaction
// must independently enforce the original ExpiresAt for a new write.
// Shared-backend callers must hold the scope publish lock and reload first.
func (s *Store) UseApprovalForOperation(request ApprovalOperation) (ApprovalOperationReceipt, error) {
	if !model.KeyPattern.MatchString(request.PrincipalID) || !model.KeyPattern.MatchString(request.OperationID) {
		return ApprovalOperationReceipt{}, ErrValidation
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.data.Approvals[request.ApprovalID]
	if a == nil {
		return ApprovalOperationReceipt{}, fmt.Errorf("%w: approval %s", ErrNotFound, request.ApprovalID)
	}
	if a.WorkspaceID != request.WorkspaceID || a.EnvironmentID != request.EnvironmentID || a.Action != request.Action || a.Object != request.Object {
		return ApprovalOperationReceipt{}, fmt.Errorf("%w: approval scope or operation mismatch", ErrAccessDenied)
	}
	if a.PayloadHash != request.PayloadHash || a.BaseRevision != request.BaseRevision {
		return ApprovalOperationReceipt{}, fmt.Errorf("%w: operation changed after review", ErrConflict)
	}
	replayed := a.State == ApprovalUsed
	if replayed {
		if a.UsedOperationID == "" || a.UsedPrincipalID == "" || a.UsedAt == nil {
			return ApprovalOperationReceipt{}, ErrInvalidState
		}
		if a.UsedOperationID != request.OperationID || a.UsedPrincipalID != request.PrincipalID {
			return ApprovalOperationReceipt{}, fmt.Errorf("%w: approval already bound to another operation", ErrConflict)
		}
	} else {
		now := s.now()
		if a.State != DraftApproved || a.ExpiresAt == nil || !now.Before(*a.ExpiresAt) {
			return ApprovalOperationReceipt{}, fmt.Errorf("%w: approval missing, used or expired", ErrInvalidState)
		}
		a.State = ApprovalUsed
		a.UsedAt = &now
		a.UsedOperationID = request.OperationID
		a.UsedPrincipalID = request.PrincipalID
		s.changes.markDirty(collApprovals, a.ID)
	}
	// A previous failed commit may have retained this mutation in the working set.
	// Even an exact receipt replay must finish that persistence before succeeding.
	if err := s.commitLocked(); err != nil {
		return ApprovalOperationReceipt{}, err
	}
	return ApprovalOperationReceipt{Approval: copyApproval(a), Replayed: replayed}, nil
}
