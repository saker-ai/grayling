package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

func TestRejectApprovalAPI(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("review", "Review")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	requester, err := store.CreateToken(platform.TokenAdmin, "requester", platform.RoleOwner, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := store.CreateToken(platform.TokenAdmin, "reviewer", platform.RoleOwner, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateWorkspace("other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := store.CreateToken(platform.TokenAdmin, "outsider", platform.RoleOwner, other.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	approval, err := store.RequestApproval(platform.Approval{WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Action: "ai.pin", Object: "prompt/tone", PayloadHash: "hash", Reason: "candidate",
		SubmittedBy: requester.Token.Name, SubmittedTokenID: requester.Token.ID})
	if err != nil {
		t.Fatal(err)
	}
	// The submitter withdraws their own request; the reviewer rejects the
	// other one. Both end in the same terminal state.
	withdrawal, err := store.RequestApproval(platform.Approval{WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Action: "ai.pin", Object: "prompt/withdrawn", PayloadHash: "hash2", Reason: "second thoughts",
		SubmittedBy: requester.Token.Name, SubmittedTokenID: requester.Token.ID})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{})
	call := func(id, secret, reason string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"reason": reason})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/workspaces/review/approvals/"+id+"/reject", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	for _, check := range []struct {
		name, id, secret, reason string
		want                     int
	}{
		{"self withdrawal", withdrawal.ID, requester.Secret, "withdrawn", http.StatusOK},
		{"cross workspace", approval.ID, outsider.Secret, "reject", http.StatusForbidden},
		{"missing reason", approval.ID, reviewer.Secret, " ", http.StatusBadRequest},
		{"reviewer", approval.ID, reviewer.Secret, "risk too high", http.StatusOK},
		{"repeat", approval.ID, reviewer.Secret, "again", http.StatusConflict},
	} {
		t.Run(check.name, func(t *testing.T) {
			response := call(check.id, check.secret, check.reason)
			if response.Code != check.want {
				t.Fatalf("status %d, want %d: %s", response.Code, check.want, response.Body.String())
			}
		})
	}
	got, err := store.GetApproval(approval.ID)
	if err != nil || got.State != platform.ApprovalRejected || got.RejectionReason != "risk too high" {
		t.Fatalf("stored rejection: %+v %v", got, err)
	}
	withdrawn, err := store.GetApproval(withdrawal.ID)
	if err != nil || withdrawn.State != platform.ApprovalRejected ||
		withdrawn.RejectedBy != requester.Token.Name ||
		withdrawn.RejectedTokenID != requester.Token.ID ||
		withdrawn.RejectionReason != "withdrawn" {
		t.Fatalf("stored withdrawal: %+v %v", withdrawn, err)
	}
	foundAudit := false
	for _, record := range store.AuditList(ws.ID, 0) {
		if record.Action == "approval.reject" && record.Object == approval.ID {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatal("rejection audit missing")
	}
	foundWithdrawalAudit := false
	for _, record := range store.AuditList(ws.ID, 0) {
		if record.Action == "approval.reject" && record.Object == withdrawal.ID {
			foundWithdrawalAudit = true
		}
	}
	if !foundWithdrawalAudit {
		t.Fatal("withdrawal audit missing")
	}
}

func TestApprovalComparisonShowsRollbackDocumentsAndStaleness(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("compare", "Compare")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := store.CreateToken(platform.TokenAdmin, "viewer", platform.RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateWorkspace("outside", "Outside")
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := store.CreateToken(platform.TokenAdmin, "outsider", platform.RoleViewer, other.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range []int64{1, 2} {
		if err := store.AppendFlagRevision(&platform.FlagRevision{WorkspaceID: ws.ID, EnvironmentID: env.ID,
			Key: "release", Revision: revision, Hash: "hash", Content: &model.Flag{Key: "release",
				Variations: map[string]any{"on": revision == 2}}}); err != nil {
			t.Fatal(err)
		}
	}
	approval, err := store.RequestApproval(platform.Approval{WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Action: "flag.rollback", Object: "release", BaseRevision: 2, Payload: `{"revision":1}`,
		PayloadHash: "hash", Reason: "incident", SubmittedBy: "owner", SubmittedTokenID: "owner-token"})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{})
	path := "/api/v1/admin/workspaces/compare/approvals/" + approval.ID + "/comparison"
	call := func(secret string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	if got := call(outsider.Secret); got.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace comparison: %d %s", got.Code, got.Body.String())
	}
	check := func(wantStale bool) {
		t.Helper()
		response := call(viewer.Secret)
		if response.Code != http.StatusOK {
			t.Fatalf("comparison: %d %s", response.Code, response.Body.String())
		}
		var body struct {
			Available      bool       `json:"available"`
			Stale          bool       `json:"stale"`
			BeforeRevision int64      `json:"beforeRevision"`
			AfterRevision  int64      `json:"afterRevision"`
			Before         model.Flag `json:"before"`
			After          model.Flag `json:"after"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.Available || body.Stale != wantStale || body.BeforeRevision != 2 || body.AfterRevision != 1 ||
			body.Before.Variations["on"] != true || body.After.Variations["on"] != false {
			t.Fatalf("comparison: %+v", body)
		}
	}
	check(false)
	if err := store.AppendFlagRevision(&platform.FlagRevision{WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Key: "release", Revision: 3, Hash: "hash3", Content: &model.Flag{Key: "release"}}); err != nil {
		t.Fatal(err)
	}
	check(true)
	aiApproval, err := store.RequestApproval(platform.Approval{WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Action: "ai.pin", Object: "prompt/tone", BaseRevision: 0, PayloadHash: "secret-hash",
		Reason: "new prompt", SubmittedBy: "owner", SubmittedTokenID: "owner-token"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workspaces/compare/approvals/"+aiApproval.ID+"/comparison", nil)
	req.Header.Set("Authorization", "Bearer "+viewer.Secret)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"reason":"content_not_retained"`)) ||
		bytes.Contains(response.Body.Bytes(), []byte(`"after"`)) {
		t.Fatalf("AI comparison must not expose content: %d %s", response.Code, response.Body.String())
	}
}
