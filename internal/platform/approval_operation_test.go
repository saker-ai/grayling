package platform

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestApprovalOperationDurableConcurrentReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.sqlite")
	s, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	input := approvalFixture(t, s)
	forged := time.Now()
	input.UsedAt, input.UsedOperationID, input.UsedPrincipalID = &forged, "forged", "forged"
	a, err := s.RequestApproval(input)
	if err != nil {
		t.Fatal(err)
	}
	if a.UsedAt != nil || a.UsedOperationID != "" || a.UsedPrincipalID != "" {
		t.Fatal("accepted forged receipt")
	}
	if _, err = s.ApproveChange(a.ID, "reviewer", "review-token", ""); err != nil {
		t.Fatal(err)
	}
	req := ApprovalOperation{ApprovalID: a.ID, WorkspaceID: a.WorkspaceID, EnvironmentID: a.EnvironmentID, Action: a.Action, Object: a.Object, PayloadHash: a.PayloadHash, BaseRevision: a.BaseRevision, PrincipalID: "approval-owner", OperationID: "publication"}
	var first atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			r, e := s.UseApprovalForOperation(req)
			if e != nil {
				t.Error(e)
				return
			}
			if !r.Replayed {
				first.Add(1)
			}
			if r.Approval.UsedAt == nil {
				t.Error("missing consumption time")
			}
		})
	}
	wg.Wait()
	if first.Load() != 1 {
		t.Fatal("consumed more than once", first.Load())
	}
	for _, mutate := range []func(*ApprovalOperation){func(r *ApprovalOperation) { r.OperationID = "other" }, func(r *ApprovalOperation) { r.PrincipalID = "other" }, func(r *ApprovalOperation) { r.PayloadHash = "other" }, func(r *ApprovalOperation) { r.BaseRevision++ }, func(r *ApprovalOperation) { r.WorkspaceID = "other" }, func(r *ApprovalOperation) { r.Action = "other" }, func(r *ApprovalOperation) { r.Object = "other" }} {
		changed := req
		mutate(&changed)
		if _, err := s.UseApprovalForOperation(changed); err == nil {
			t.Fatal("accepted changed receipt")
		}
	}
	if _, err := s.UseApproval(a.ID, a.WorkspaceID, a.EnvironmentID, a.Action, a.Object, a.PayloadHash, a.BaseRevision); !errors.Is(err, ErrInvalidState) {
		t.Fatal("legacy use accepted consumed grant", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mu.Lock()
	past := time.Now().Add(-time.Hour)
	s.data.Approvals[a.ID].ExpiresAt = &past
	s.changes.markDirty(collApprovals, a.ID)
	s.mu.Unlock()
	r, err := s.UseApprovalForOperation(req)
	if err != nil || !r.Replayed || r.Approval.UsedOperationID != req.OperationID {
		t.Fatal("lost durable receipt", r, err)
	}
}

func TestApprovalOperationExpiredFirstUse(t *testing.T) {
	s, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.RequestApproval(approvalFixture(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveChange(a.ID, "reviewer", "token", ""); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	past := time.Now().Add(-time.Hour)
	s.data.Approvals[a.ID].ExpiresAt = &past
	s.changes.markDirty(collApprovals, a.ID)
	s.mu.Unlock()
	_, err = s.UseApprovalForOperation(ApprovalOperation{ApprovalID: a.ID, WorkspaceID: a.WorkspaceID, EnvironmentID: a.EnvironmentID, Action: a.Action, Object: a.Object, PayloadHash: a.PayloadHash, BaseRevision: a.BaseRevision, PrincipalID: "principal", OperationID: "op"})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
}
