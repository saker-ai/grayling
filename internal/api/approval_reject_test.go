package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/saker-ai/grayling/internal/platform"
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
	server := NewServer(store, nil, Options{})
	path := "/api/v1/admin/workspaces/review/approvals/" + approval.ID + "/reject"
	call := func(secret, reason string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"reason": reason})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	for _, check := range []struct {
		name, secret, reason string
		want                 int
	}{
		{"self", requester.Secret, "reject", http.StatusForbidden},
		{"cross workspace", outsider.Secret, "reject", http.StatusForbidden},
		{"missing reason", reviewer.Secret, " ", http.StatusBadRequest},
		{"reviewer", reviewer.Secret, "risk too high", http.StatusOK},
		{"repeat", reviewer.Secret, "again", http.StatusConflict},
	} {
		t.Run(check.name, func(t *testing.T) {
			response := call(check.secret, check.reason)
			if response.Code != check.want {
				t.Fatalf("status %d, want %d: %s", response.Code, check.want, response.Body.String())
			}
		})
	}
	got, err := store.GetApproval(approval.ID)
	if err != nil || got.State != platform.ApprovalRejected || got.RejectionReason != "risk too high" {
		t.Fatalf("stored rejection: %+v %v", got, err)
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
}
