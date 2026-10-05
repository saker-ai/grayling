package platform

import (
	"fmt"
	"slices"
	"time"
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
	UsedAt           *time.Time `json:"usedAt,omitempty"`
}

const ApprovalUsed DraftState = "used"

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
	return &out
}

func (s *Store) RequestApproval(a Approval) (*Approval, error) {
	if a.WorkspaceID == "" || a.EnvironmentID == "" || a.Action == "" || a.Object == "" ||
		a.PayloadHash == "" || a.SubmittedBy == "" || a.SubmittedTokenID == "" || a.Reason == "" {
		return nil, fmt.Errorf("%w: incomplete approval request", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkEnvLocked(a.WorkspaceID, a.EnvironmentID); err != nil {
		return nil, err
	}
	a.ID = s.nextID("approval")
	a.State = DraftSubmitted
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
	if actor == a.SubmittedBy || tokenID == a.SubmittedTokenID {
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

// UseApproval atomically consumes one exact grant before the effect executes.
// A failed effect requires a new request, so no stale grant can be replayed.
func (s *Store) UseApproval(id, workspaceID, environmentID, action, object, hash string, baseRevision int64) (*Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return copyApproval(a), nil
}
