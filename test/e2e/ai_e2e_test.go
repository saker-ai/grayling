package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	aisvc "github.com/godeps/grayling/internal/ai"
	"github.com/godeps/grayling/internal/api"
	"github.com/godeps/grayling/internal/configstore"
	"github.com/godeps/grayling/internal/platform"
	"github.com/godeps/grayling/pkg/bucketing"
	"github.com/godeps/grayling/pkg/model"
	gonacosai "github.com/godeps/gonacos/pkg/ai"
	gconfig "github.com/godeps/gonacos/pkg/config"
)

// aiHarness extends the base harness with the AI gate wired in.
type aiHarness struct {
	*harness
	ai *aisvc.Service
}

func newAIHarness(t *testing.T) *aiHarness {
	t.Helper()
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := configstore.NewGonacosStore(gconfig.NewService())
	apiSrv := api.NewServer(store, cfg, api.Options{})
	ai := aisvc.NewService(store, gonacosai.NewService(nil))
	apiSrv.SetAI(ai)
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)

	h := &aiHarness{harness: &harness{t: t, store: store, api: apiSrv, srv: srv}, ai: ai}
	ct, err := store.CreateToken(platform.TokenAdmin, "bootstrap-owner", platform.RoleOwner, "", "", "bootstrap")
	if err != nil {
		t.Fatalf("owner token: %v", err)
	}
	h.owner = ct.Secret
	h.setupWorkspace()
	return h
}

// aiFlag is a json flag whose variations carry promptRef + modelRef — the
// AI-resource gray-release shape from §7.
func aiFlag(key string) *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           key, Type: model.FlagJSON,
		Fields:    []string{"user_id"},
		Bucketing: &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: key},
		Variations: map[string]any{
			"control": map[string]any{
				"promptRef": map[string]any{"type": "prompt", "logicalId": "support-tone"},
				"modelRef":  map[string]any{"provider": "qwen", "model": "qwen-max"},
			},
			"candidate": map[string]any{
				"promptRef": map[string]any{"type": "prompt", "logicalId": "support-tone", "version": "v2"},
				"modelRef":  map[string]any{"provider": "qwen", "model": "qwen-plus"},
			},
		},
		DefaultRule: &model.Rule{Variation: "control"},
		Targeting: []model.Rule{
			{Name: "ramp", Percentage: map[string]float64{"control": 50, "candidate": 50}},
		},
	}
}

func TestAIPublishRequiresAllowlist(t *testing.T) {
	h := newAIHarness(t)

	// Register + pin prompt versions BEFORE the flag can publish.
	var ref platform.AIResourceRef
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/ai/resources",
		h.owner, map[string]any{"type": "prompt", "logicalId": "support-tone", "name": "Support Tone"},
		&ref); code != http.StatusCreated {
		t.Fatalf("register: %d", code)
	}
	if ref.PhysicalID == "" {
		t.Fatal("physical id must be server-generated")
	}
	var pin1 platform.PinnedVersion
	a1 := h.approveOperation("ai.pin", "prompt/support-tone", map[string]any{"content": "Calm tone v1"})
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/ai/resources/prompt/support-tone/pin",
		h.owner, map[string]any{"content": "Calm tone v1", "approvalId": a1}, &pin1); code != http.StatusCreated {
		t.Fatalf("pin v1: %d", code)
	}
	var pin2 platform.PinnedVersion
	a2 := h.approveOperation("ai.pin", "prompt/support-tone", map[string]any{"content": "Concise tone v2"})
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/ai/resources/prompt/support-tone/pin",
		h.owner, map[string]any{"content": "Concise tone v2", "approvalId": a2}, &pin2); code != http.StatusCreated {
		t.Fatalf("pin v2: %d", code)
	}
	if pin1.Version == pin2.Version {
		t.Fatalf("pins must be distinct versions: %s vs %s", pin1.Version, pin2.Version)
	}

	// Without a model allowlist the flag CANNOT publish (§7: modelRef 白名单).
	flag := aiFlag("ai-router")
	base := "/api/v1/admin/workspaces/acme/environments/prod/flags/ai-router"
	if code := h.do("PUT", base+"/draft", h.owner, map[string]any{"content": flag}, nil); code != http.StatusOK {
		t.Fatalf("draft: %d", code)
	}
	if code := h.do("POST", base+"/submit", h.owner, map[string]any{"reason": "AI route release"}, nil); code != http.StatusOK {
		t.Fatalf("submit: %d", code)
	}
	if code := h.do("POST", base+"/approve", h.reviewer, map[string]any{}, nil); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	if code := h.do("POST", base+"/publish", h.owner, map[string]any{}, nil); code == http.StatusOK {
		t.Fatal("publish must fail while models are not allowlisted")
	} else {
		t.Logf("publish correctly refused: %d", code)
	}

	// Configure the allowlist → publish succeeds.
	if code := h.do("PUT", "/api/v1/admin/workspaces/acme/model-allowlist", h.owner,
		map[string]any{"entries": []map[string]any{
			{"provider": "qwen", "model": "qwen-max", "enabled": true},
			{"provider": "qwen", "model": "qwen-plus", "enabled": true},
		}}, nil); code != http.StatusOK {
		t.Fatalf("allowlist: %d", code)
	}
	var pub adminPublishResult
	if code := h.do("POST", base+"/publish", h.owner, map[string]any{"note": "AI release"}, &pub); code != http.StatusOK {
		t.Fatalf("publish with allowlist: %d", code)
	}
	if pub.FlagRevision != 1 {
		t.Fatalf("publish result: %+v", pub)
	}
}

func TestAISessionFreezeAndRevocation(t *testing.T) {
	h := newAIHarness(t)

	// Setup: resource + allowlist + published flag (direct via services to
	// keep this test focused on the session path).
	if _, err := h.ai.RegisterResource(h.ws.ID, h.env.ID, platform.AITypePrompt, "support-tone", "Support Tone", "alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := h.ai.PublishVersion(h.ws.ID, h.env.ID, platform.AITypePrompt, "support-tone", "Calm tone v1", "alice"); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	if _, err := h.ai.PublishVersion(h.ws.ID, h.env.ID, platform.AITypePrompt, "support-tone", "Concise tone v2", "alice"); err != nil {
		t.Fatalf("pin v2: %v", err)
	}
	if err := h.store.SetModelAllowlist(h.ws.ID, []platform.ModelAllowEntry{
		{Provider: "qwen", Model: "qwen-max", Enabled: true},
		{Provider: "qwen", Model: "qwen-plus", Enabled: true},
	}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	h.publishFlag(t, aiFlag("ai-router"), platform.RiskNormal, h.owner)

	// Freeze a session: flags evaluated once, resource refs auto-discovered
	// from the served variation, digest-verified content frozen (§7).
	var frozen api.SessionSnapshot
	code := h.do("POST", "/api/v1/sdk/ai/session", h.sdkTok, map[string]any{
		"sessionId": "conv-1",
		"context": map[string]any{
			"targetingKey": "user-9",
			"attributes":   map[string]any{"user_id": "user-9"},
		},
		"flagKeys": []string{"ai-router"},
	}, &frozen)
	if code != http.StatusOK {
		t.Fatalf("session freeze: %d", code)
	}
	if frozen.SessionID != "conv-1" || frozen.ManifestRevision == 0 {
		t.Fatalf("frozen metadata: %+v", frozen)
	}
	eval, ok := frozen.Evaluations["ai-router"]
	if !ok || eval.Failed {
		t.Fatalf("evaluation missing/failed: %+v", eval)
	}
	if len(frozen.Resources) != 1 {
		t.Fatalf("promptRef must be auto-discovered: %+v", frozen.Resources)
	}
	res := frozen.Resources[0]
	if res.Type != "prompt" || res.LogicalID != "support-tone" {
		t.Fatalf("resource: %+v", res)
	}
	// The control variation pins no version → latest (v2); the candidate
	// variation pins v2 explicitly. Either way content must match digest.
	if res.Content != "Concise tone v2" && res.Content != "Calm tone v1" {
		t.Fatalf("frozen content: %q", res.Content)
	}
	if len(frozen.ModelRefs) != 1 {
		t.Fatalf("modelRef must be validated+recorded: %+v", frozen.ModelRefs)
	}

	// Freezing the same session again yields the SAME resource versions —
	// multi-turn consistency comes from pinned immutability (§7: 资源更新
	// 不改变已有实验快照).
	var frozen2 api.SessionSnapshot
	if code := h.do("POST", "/api/v1/sdk/ai/session", h.sdkTok, map[string]any{
		"sessionId": "conv-1",
		"context": map[string]any{
			"targetingKey": "user-9",
			"attributes":   map[string]any{"user_id": "user-9"},
		},
		"flagKeys": []string{"ai-router"},
	}, &frozen2); code != http.StatusOK {
		t.Fatalf("re-freeze: %d", code)
	}
	if frozen2.Resources[0].Version != res.Version || frozen2.Resources[0].Digest != res.Digest {
		t.Fatalf("re-freeze drifted: %+v vs %+v", frozen2.Resources[0], res)
	}
	if frozen2.Evaluations["ai-router"].VariationType != eval.VariationType {
		t.Fatal("same unit must get the same arm across turns")
	}

	// Emergency revocation: the pre-execution gate blocks immediately, and
	// new freezes fail (long tasks already holding content must consult the
	// gate before each use — §7).
	var revoked struct {
		ReviewID string `json:"reviewId"`
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/ai/resources/prompt/support-tone/revoke",
		h.owner, map[string]any{"reason": "prompt injection found"}, &revoked); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	if revoked.ReviewID == "" {
		t.Fatal("revocation must create post-action review")
	}
	var check struct {
		Resources []struct {
			Type      string `json:"type"`
			LogicalID string `json:"logicalId"`
			Blocked   bool   `json:"blocked"`
			Reason    string `json:"reason"`
		} `json:"resources"`
		AnyBlocked bool `json:"anyBlocked"`
	}
	if code := h.do("POST", "/api/v1/sdk/ai/check", h.sdkTok, map[string]any{
		"resources": []map[string]any{{"type": "prompt", "logicalId": "support-tone"}},
	}, &check); code != http.StatusLocked {
		t.Fatalf("revocation gate: %d", code)
	}
	if !check.AnyBlocked || !check.Resources[0].Blocked {
		t.Fatalf("gate must report blocked: %+v", check)
	}
	if code := h.do("POST", "/api/v1/sdk/ai/session", h.sdkTok, map[string]any{
		"sessionId": "conv-2",
		"context":   map[string]any{"attributes": map[string]any{"user_id": "user-10"}},
		"flagKeys":  []string{"ai-router"},
	}, nil); code == http.StatusOK {
		t.Fatal("freeze must fail while a referenced resource is revoked")
	}

	// An independent review is required before restoring a revoked resource.
	reinstate := "/api/v1/admin/workspaces/acme/environments/prod/ai/resources/prompt/support-tone/reinstate"
	if code := h.do("POST", reinstate, h.owner, map[string]any{"reason": "fixed"}, nil); code != http.StatusConflict {
		t.Fatalf("reinstatement before review: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/approvals/"+revoked.ReviewID+"/approve",
		h.reviewer, map[string]any{"note": "remediation verified"}, nil); code != http.StatusOK {
		t.Fatalf("post-review: %d", code)
	}
	if code := h.do("POST", reinstate, h.reviewer, map[string]any{"reason": "fixed"}, nil); code != http.StatusForbidden {
		t.Fatalf("publisher reinstatement: %d", code)
	}
	// Reinstate → freeze works again.
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/ai/resources/prompt/support-tone/reinstate",
		h.owner, map[string]any{"reason": "remediation reviewed"}, nil); code != http.StatusOK {
		t.Fatalf("reinstate: %d", code)
	}
	if code := h.do("POST", "/api/v1/sdk/ai/check", h.sdkTok, map[string]any{
		"resources": []map[string]any{{"type": "prompt", "logicalId": "support-tone"}},
	}, &check); code != http.StatusOK {
		t.Fatalf("gate after reinstate: %d", code)
	}
	if check.AnyBlocked {
		t.Fatal("reinstate must unblock")
	}
}

func TestAICrossTenantUnresolvable(t *testing.T) {
	h := newAIHarness(t)
	if _, err := h.ai.RegisterResource(h.ws.ID, h.env.ID, platform.AITypePrompt, "tenant-secret", "s", "alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := h.ai.PublishVersion(h.ws.ID, h.env.ID, platform.AITypePrompt, "tenant-secret", "acme secret prompt", "alice"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Second tenant with its own SDK token.
	var ws2 platform.Workspace
	if code := h.do("POST", "/api/v1/admin/workspaces", h.owner,
		map[string]any{"slug": "globex", "name": "Globex"}, &ws2); code != http.StatusCreated {
		t.Fatalf("ws2: %d", code)
	}
	var env2 platform.Environment
	if code := h.do("POST", "/api/v1/admin/workspaces/globex/environments", h.owner,
		map[string]any{"slug": "prod", "name": ""}, &env2); code != http.StatusCreated {
		t.Fatalf("env2: %d", code)
	}
	var tok2 platform.CreatedToken
	if code := h.do("POST", "/api/v1/admin/workspaces/globex/tokens", h.owner,
		map[string]any{"name": "sdk-globex", "kind": "sdk", "environment": "prod"}, &tok2); code != http.StatusCreated {
		t.Fatalf("token2: %d", code)
	}

	// Globex's SDK token cannot resolve acme's resource — the request scope
	// comes from the credential, so there is nothing to enumerate: 404.
	code := h.do("POST", "/api/v1/sdk/ai/resolve", tok2.Secret, map[string]any{
		"type": "prompt", "logicalId": "tenant-secret",
	}, nil)
	if code != http.StatusNotFound {
		t.Fatalf("cross-tenant resolve: %d, want 404 (existence hidden)", code)
	}
	// Even naming the physical ID doesn't help: the resolve endpoint is
	// keyed by (token scope, logical id) only.
	acmeRef, err := h.store.GetAIResource(h.ws.ID, h.env.ID, platform.AITypePrompt, "tenant-secret")
	if err != nil {
		t.Fatalf("GetAIResource: %v", err)
	}
	code = h.do("POST", "/api/v1/sdk/ai/resolve", tok2.Secret, map[string]any{
		"type": "prompt", "logicalId": acmeRef.PhysicalID,
	}, nil)
	if code != http.StatusNotFound {
		t.Fatalf("physical-id-as-logical resolve: %d, want 404", code)
	}
	// Acme's own SDK token resolves fine.
	var content aisvc.Content
	if code := h.do("POST", "/api/v1/sdk/ai/resolve", h.sdkTok, map[string]any{
		"type": "prompt", "logicalId": "tenant-secret",
	}, &content); code != http.StatusOK {
		t.Fatalf("own-tenant resolve: %d", code)
	}
	if content.Content != "acme secret prompt" {
		t.Fatalf("content = %q", content.Content)
	}
	// The frozen session response must serialize cleanly (console/debug use).
	if b, err := json.Marshal(content); err != nil || len(b) == 0 {
		t.Fatalf("content json: %v", err)
	}
}
