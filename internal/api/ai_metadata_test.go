package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/saker-ai/grayling/internal/platform"
)

func TestAIResourceDetailIsScopedMetadataOnly(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("metadata", "Metadata")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.RegisterAIResource(ws.ID, env.ID, platform.AITypePrompt, "tone", "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v1", "v2"} {
		if err := store.PinAIVersion(&platform.PinnedVersion{WorkspaceID: ws.ID, EnvironmentID: env.ID,
			Type: platform.AITypePrompt, LogicalID: "tone", PhysicalID: ref.PhysicalID,
			Version: version, Digest: "digest-" + version}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RevokeAIResource(ws.ID, env.ID, platform.AITypePrompt, "tone", "incident", "owner"); err != nil {
		t.Fatal(err)
	}
	viewer, err := store.CreateToken(platform.TokenAdmin, "viewer", platform.RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateWorkspace("outside-metadata", "Outside")
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := store.CreateToken(platform.TokenAdmin, "outsider", platform.RoleViewer, other.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{})
	path := "/api/v1/admin/workspaces/metadata/environments/prod/ai/resources/prompt/tone"
	call := func(secret string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	if got := call(outsider.Secret); got.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace metadata: %d %s", got.Code, got.Body.String())
	}
	response := call(viewer.Secret)
	if response.Code != http.StatusOK {
		t.Fatalf("viewer metadata: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Resource         platform.AIResourceRef   `json:"resource"`
		Pins             []platform.PinnedVersion `json:"pins"`
		Revoked          bool                     `json:"revoked"`
		RevocationReason string                   `json:"revocationReason"`
		SourceMode       string                   `json:"sourceMode"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Resource.LogicalID != "tone" || len(body.Pins) != 2 || body.Pins[0].Version != "v2" ||
		body.Pins[1].Version != "v1" || !body.Revoked || body.RevocationReason == "" || body.SourceMode != "unavailable" {
		t.Fatalf("metadata: %+v", body)
	}
	if bytes.Contains(response.Body.Bytes(), []byte(`"content"`)) {
		t.Fatalf("raw content leaked in metadata response: %s", response.Body.String())
	}
}
