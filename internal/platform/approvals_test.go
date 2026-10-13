package platform

import (
	"errors"
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
	if _, err := s.ApproveChange(a.ID, "alice", "tok-a", "self"); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self approval: %v", err)
	}
	if _, err := s.ApproveChange(a.ID, "bob", "tok-b", "reviewed"); err != nil {
		t.Fatal(err)
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
}
