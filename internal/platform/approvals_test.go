package platform

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/saker-ai/grayling/test/pgtest"
)

func approvalFixture(t *testing.T, s *Store) Approval {
	t.Helper()
	ws, err := s.CreateWorkspace("approvals", "Approvals")
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	return Approval{WorkspaceID: ws.ID, EnvironmentID: env.ID, Action: "ai.pin",
		Object: "prompt/tone", PayloadHash: "hash-v1", BaseRevision: 3,
		Reason: "deploy candidate", SubmittedBy: "alice", SubmittedTokenID: "tok-a"}
}

func TestApprovalStateMachineAndConcurrentUse(t *testing.T) {
	s, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.RequestApproval(approvalFixture(t, s))
	if err != nil {
		t.Fatal(err)
	}
	// The applicant may grant their own request; the approver fields record
	// who actually pressed approve.
	self, err := s.ApproveChange(a.ID, "alice", "tok-a", "self")
	if err != nil {
		t.Fatalf("self approval must be allowed: %v", err)
	}
	if self.State != DraftApproved || self.ApprovedBy != "alice" || self.ApprovedTokenID != "tok-a" {
		t.Fatalf("self-approved change: %+v", self)
	}
	// A second grant on the same request is an invalid state, whether it
	// comes from the applicant or anyone else.
	if _, err := s.ApproveChange(a.ID, "bob", "tok-b", "reviewed"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("double approval: %v", err)
	}
	if _, err := s.UseApproval(a.ID, a.WorkspaceID, a.EnvironmentID, a.Action, a.Object, "different", 3); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed payload: %v", err)
	}
	if _, err := s.UseApproval(a.ID, "another", a.EnvironmentID, a.Action, a.Object, "hash-v1", 3); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("cross-workspace use: %v", err)
	}
	if _, err := s.UseApproval(a.ID, a.WorkspaceID, a.EnvironmentID, a.Action, a.Object, "hash-v1", 4); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed base revision: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := s.UseApproval(a.ID, a.WorkspaceID, a.EnvironmentID, a.Action, a.Object, "hash-v1", 3)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	success, denied := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrInvalidState) {
			denied++
		} else {
			t.Errorf("use: %v", err)
		}
	}
	if success != 1 || denied != 1 {
		t.Fatalf("concurrent uses: success=%d denied=%d", success, denied)
	}

	expired, err := s.RequestApproval(Approval{WorkspaceID: a.WorkspaceID, EnvironmentID: a.EnvironmentID,
		Action: a.Action, Object: a.Object, PayloadHash: "hash-v2", Reason: "later",
		SubmittedBy: "alice", SubmittedTokenID: "tok-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveChange(expired.ID, "bob", "tok-b", "reviewed"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	past := time.Now().Add(-time.Minute)
	s.data.Approvals[expired.ID].ExpiresAt = &past
	s.mu.Unlock()
	if _, err := s.UseApproval(expired.ID, a.WorkspaceID, a.EnvironmentID, a.Action, a.Object, "hash-v2", 0); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expired grant: %v", err)
	}
}

func TestPGApprovalRoundtrip(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openPGStore(t, dsn)
	request, err := a.RequestApproval(approvalFixture(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ApproveChange(request.ID, "bob", "tok-b", "checked"); err != nil {
		t.Fatal(err)
	}
	b := openPGStore(t, dsn)
	loaded, err := b.GetApproval(request.ID)
	if err != nil || loaded.State != DraftApproved || loaded.PayloadHash != request.PayloadHash || loaded.ExpiresAt == nil {
		t.Fatalf("reloaded approval: %+v %v", loaded, err)
	}
	if _, err := b.UseApproval(request.ID, request.WorkspaceID, request.EnvironmentID,
		request.Action, request.Object, request.PayloadHash, request.BaseRevision); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	loaded, err = a.GetApproval(request.ID)
	if err != nil || loaded.State != ApprovalUsed {
		t.Fatalf("cross-replica use: %+v %v", loaded, err)
	}
	rejection, err := a.RequestApproval(Approval{WorkspaceID: request.WorkspaceID,
		EnvironmentID: request.EnvironmentID, Action: "ai.pin", Object: "prompt/other",
		PayloadHash: "hash-v2", Reason: "candidate", SubmittedBy: "alice", SubmittedTokenID: "tok-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RejectChange(rejection.ID, "bob", "tok-b", "not ready"); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	loaded, err = b.GetApproval(rejection.ID)
	if err != nil || loaded.State != ApprovalRejected || loaded.RejectionReason != "not ready" {
		t.Fatalf("cross-replica rejection: %+v %v", loaded, err)
	}
}

func TestRejectApprovalPreventsUseAndPersists(t *testing.T) {
	s, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.RequestApproval(approvalFixture(t, s))
	if err != nil {
		t.Fatal(err)
	}
	// The applicant may reject their own request, which withdraws it.
	withdrawn, err := s.RequestApproval(Approval{WorkspaceID: a.WorkspaceID,
		EnvironmentID: a.EnvironmentID, Action: a.Action, Object: "prompt/withdrawn",
		PayloadHash: "hash-w", Reason: "changed my mind",
		SubmittedBy: "alice", SubmittedTokenID: "tok-a"})
	if err != nil {
		t.Fatal(err)
	}
	selfRejected, err := s.RejectChange(withdrawn.ID, "alice", "tok-a", "withdrawn")
	if err != nil {
		t.Fatalf("self rejection must be allowed: %v", err)
	}
	if selfRejected.State != ApprovalRejected || selfRejected.RejectedBy != "alice" ||
		selfRejected.RejectedTokenID != "tok-a" || selfRejected.RejectionReason != "withdrawn" {
		t.Fatalf("self-withdrawn approval: %+v", selfRejected)
	}
	if _, err := s.UseApproval(withdrawn.ID, withdrawn.WorkspaceID, withdrawn.EnvironmentID,
		withdrawn.Action, withdrawn.Object, withdrawn.PayloadHash, withdrawn.BaseRevision); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("use self-withdrawn request: %v", err)
	}
	if _, err := s.RejectChange(a.ID, "bob", "tok-b", ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty reason: %v", err)
	}
	rejected, err := s.RejectChange(a.ID, "bob", "tok-b", "risk exceeds policy")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.State != ApprovalRejected || rejected.RejectedAt == nil ||
		rejected.RejectedBy != "bob" || rejected.RejectionReason != "risk exceeds policy" {
		t.Fatalf("rejected approval: %+v", rejected)
	}
	if _, err := s.ApproveChange(a.ID, "carol", "tok-c", "later"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("approve rejected request: %v", err)
	}
	if _, err := s.UseApproval(a.ID, a.WorkspaceID, a.EnvironmentID, a.Action, a.Object,
		a.PayloadHash, a.BaseRevision); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("use rejected request: %v", err)
	}
	if _, err := s.RejectChange(a.ID, "carol", "tok-c", "again"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("reject twice: %v", err)
	}
}

func TestSQLiteRejectedApprovalRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.sqlite")
	store, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := store.RequestApproval(approvalFixture(t, store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RejectChange(a.ID, "bob", "tok-b", "risk exceeds policy"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	got, err := reopened.GetApproval(a.ID)
	if err != nil || got.State != ApprovalRejected || got.RejectedAt == nil ||
		got.RejectedBy != "bob" || got.RejectionReason != "risk exceeds policy" {
		t.Fatalf("reopened rejection: %+v %v", got, err)
	}
}
