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

func TestFlagDraftAPIRejectsStaleDraftHash(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("draft-conflict", "Draft conflict")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "dev", "Development")
	if err != nil {
		t.Fatal(err)
	}
	alice, err := store.CreateToken(platform.TokenAdmin, "alice", platform.RolePublisher, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.CreateToken(platform.TokenAdmin, "bob", platform.RolePublisher, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{})
	path := "/api/v1/admin/workspaces/" + ws.ID + "/environments/" + env.ID + "/flags/shared"
	call := func(method, secret string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload []byte
		if body != nil {
			payload, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		target := path
		if method == http.MethodPut {
			target += "/draft"
		}
		req := httptest.NewRequest(method, target, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp := httptest.NewRecorder()
		server.Handler().ServeHTTP(resp, req)
		return resp
	}
	readHash := func(secret string) string {
		t.Helper()
		resp := call(http.MethodGet, secret, nil)
		if resp.Code != http.StatusOK {
			t.Fatalf("GET flag = %d: %s", resp.Code, resp.Body.String())
		}
		var view struct {
			DraftHash string `json:"draftHash"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view.DraftHash
	}
	flag := &model.Flag{SchemaVersion: model.SchemaVersion, Key: "shared", Type: model.FlagBool,
		Variations: map[string]any{"off": false, "on": true}, DefaultRule: &model.Rule{Variation: "off"}}
	if got := readHash(alice.Secret); got != "" {
		t.Fatalf("new flag draft hash = %q, want empty", got)
	}
	if resp := call(http.MethodPut, alice.Secret, map[string]any{"content": flag, "baseRevision": 0}); resp.Code != http.StatusBadRequest {
		t.Fatalf("missing expectedDraftHash = %d: %s", resp.Code, resp.Body.String())
	}
	if resp := call(http.MethodPut, alice.Secret, map[string]any{"content": flag, "expectedDraftHash": ""}); resp.Code != http.StatusBadRequest {
		t.Fatalf("missing baseRevision = %d: %s", resp.Code, resp.Body.String())
	}
	if resp := call(http.MethodPut, alice.Secret, map[string]any{"content": flag, "baseRevision": 0,
		"expectedDraftHash": ""}); resp.Code != http.StatusOK {
		t.Fatalf("initial save = %d: %s", resp.Code, resp.Body.String())
	}
	staleHash := readHash(bob.Secret)
	if staleHash == "" {
		t.Fatal("saved draft must have a fingerprint")
	}
	bobEdit := *flag
	bobEdit.Disable = true
	if resp := call(http.MethodPut, bob.Secret, map[string]any{"content": &bobEdit,
		"baseRevision": 0, "expectedDraftHash": staleHash}); resp.Code != http.StatusOK {
		t.Fatalf("bob save = %d: %s", resp.Code, resp.Body.String())
	}
	if resp := call(http.MethodPut, alice.Secret, map[string]any{"content": flag,
		"baseRevision": 0, "expectedDraftHash": staleHash}); resp.Code != http.StatusConflict {
		t.Fatalf("stale editor save = %d: %s", resp.Code, resp.Body.String())
	}
	if got := readHash(alice.Secret); got == staleHash {
		t.Fatal("rejected save replaced the remote draft")
	}
	draft, err := store.GetFlagDraft(ws.ID, env.ID, flag.Key)
	if err != nil || !draft.Content.Disable {
		t.Fatalf("remote draft after rejected save: %+v, %v", draft, err)
	}
}
