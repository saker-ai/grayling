package ai

import (
	"errors"
	"strings"
	"testing"

	"github.com/godeps/golab/internal/platform"
	gonacosai "github.com/godeps/gonacos/pkg/ai"
)

type fixture struct {
	svc   *Service
	store *platform.Store
	ws    *platform.Workspace
	env   *platform.Environment
	other *platform.Workspace
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	gai := gonacosai.NewService(nil)
	f := &fixture{svc: NewService(store, gai), store: store}
	f.ws, _ = store.CreateWorkspace("acme", "")
	f.env, _ = store.CreateEnvironment(f.ws.ID, "prod", "")
	f.other, _ = store.CreateWorkspace("globex", "")
	if _, err := store.CreateEnvironment(f.other.ID, "prod", ""); err != nil {
		t.Fatalf("other env: %v", err)
	}
	return f
}

func TestRegisterAndPublishAndResolve(t *testing.T) {
	f := newFixture(t)

	ref, err := f.svc.RegisterResource(f.ws.ID, f.env.ID, platform.AITypePrompt, "support-tone", "Support Tone", "alice")
	if err != nil {
		t.Fatalf("RegisterResource: %v", err)
	}
	if ref.PhysicalID == "" || strings.Contains(ref.PhysicalID, "support-tone") == false {
		t.Fatalf("physical id = %q", ref.PhysicalID)
	}
	// Duplicate registration rejected.
	if _, err := f.svc.RegisterResource(f.ws.ID, f.env.ID, platform.AITypePrompt, "support-tone", "x", "alice"); !errors.Is(err, platform.ErrExists) {
		t.Fatalf("duplicate register: %v", err)
	}

	pin, err := f.svc.PublishVersion(f.ws.ID, f.env.ID, platform.AITypePrompt, "support-tone",
		"You are a calm support agent. Greet {{name}}.", "alice")
	if err != nil {
		t.Fatalf("PublishVersion: %v", err)
	}
	if pin.Version == "" || len(pin.Digest) != 32 {
		t.Fatalf("pin: %+v", pin)
	}

	content, err := f.svc.Resolve(f.ws.ID, f.env.ID, platform.AITypePrompt, "support-tone", "")
	if err != nil {
		t.Fatalf("Resolve latest: %v", err)
	}
	if !strings.Contains(content.Content, "calm support agent") {
		t.Fatalf("content = %q", content.Content)
	}
	if content.Digest != pin.Digest || content.Version != pin.Version {
		t.Fatalf("resolved %+v != pinned %+v", content, pin)
	}

	// Second version: latest resolves to v2, explicit v1 still serves the
	// frozen content (immutable versions, no drift).
	pin2, err := f.svc.PublishVersion(f.ws.ID, f.env.ID, platform.AITypePrompt, "support-tone",
		"You are a concise support agent.", "alice")
	if err != nil {
		t.Fatalf("PublishVersion 2: %v", err)
	}
	if pin2.Version == pin.Version {
		t.Fatalf("second publish must create a new version: %s", pin2.Version)
	}
	latest, err := f.svc.Resolve(f.ws.ID, f.env.ID, platform.AITypePrompt, "support-tone", "")
	if err != nil {
		t.Fatalf("Resolve latest 2: %v", err)
	}
	if latest.Content != "You are a concise support agent." {
		t.Fatalf("latest = %q", latest.Content)
	}
	old, err := f.svc.Resolve(f.ws.ID, f.env.ID, platform.AITypePrompt, "support-tone", pin.Version)
	if err != nil {
		t.Fatalf("Resolve v1: %v", err)
	}
	if !strings.Contains(old.Content, "calm support agent") {
		t.Fatalf("v1 drifted: %q", old.Content)
	}
}

func TestResolveAuthorization(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.RegisterResource(f.ws.ID, f.env.ID, platform.AITypePrompt, "secret-prompt", "s", "alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := f.svc.PublishVersion(f.ws.ID, f.env.ID, platform.AITypePrompt, "secret-prompt", "TOP SECRET", "alice"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	otherEnv, _ := f.store.GetEnvironment(f.other.ID, "prod")
	// Different workspace, same logical ID → unauthorized, existence hidden.
	if _, err := f.svc.Resolve(f.other.ID, otherEnv.ID, platform.AITypePrompt, "secret-prompt", ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-workspace resolve: %v", err)
	}
	// Same workspace, wrong environment → unauthorized.
	devEnv, _ := f.store.CreateEnvironment(f.ws.ID, "dev", "")
	if _, err := f.svc.Resolve(f.ws.ID, devEnv.ID, platform.AITypePrompt, "secret-prompt", ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-environment resolve: %v", err)
	}
	// Unknown version → not pinned (never served from substrate directly).
	if _, err := f.svc.Resolve(f.ws.ID, f.env.ID, platform.AITypePrompt, "secret-prompt", "v999"); !errors.Is(err, ErrVersionNotPinned) {
		t.Fatalf("unpinned version resolve: %v", err)
	}
	// Unknown logical ID.
	if _, err := f.svc.Resolve(f.ws.ID, f.env.ID, platform.AITypePrompt, "ghost", ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown id resolve: %v", err)
	}
}

func TestRevocationBlocksImmediately(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.RegisterResource(f.ws.ID, f.env.ID, platform.AITypePrompt, "risky", "r", "alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := f.svc.PublishVersion(f.ws.ID, f.env.ID, platform.AITypePrompt, "risky", "dangerous content", "alice"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := f.svc.Resolve(f.ws.ID, f.env.ID, platform.AITypePrompt, "risky", ""); err != nil {
		t.Fatalf("resolve before revoke: %v", err)
	}

	if err := f.svc.Revoke(f.ws.ID, f.env.ID, platform.AITypePrompt, "risky", "prompt injection found", "sec-team"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := f.svc.Resolve(f.ws.ID, f.env.ID, platform.AITypePrompt, "risky", ""); !errors.Is(err, ErrRevoked) {
		t.Fatalf("resolve after revoke: %v", err)
	}
	reason, blocked := f.svc.CheckRevocation(f.ws.ID, f.env.ID, platform.AITypePrompt, "risky")
	if !blocked || !strings.Contains(reason, "prompt injection") {
		t.Fatalf("pre-execution gate: blocked=%v reason=%q", blocked, reason)
	}
	// A fresh Service instance (simulating restart) must still see the
	// revocation — it is persisted in the store, not only in memory.
	fresh := NewService(f.store, f.svc.Gonacos)
	if _, err := fresh.Resolve(f.ws.ID, f.env.ID, platform.AITypePrompt, "risky", ""); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revocation must survive restart: %v", err)
	}

	if err := f.svc.Reinstate(f.ws.ID, f.env.ID, platform.AITypePrompt, "risky"); err != nil {
		t.Fatalf("Reinstate: %v", err)
	}
	if _, err := f.svc.Resolve(f.ws.ID, f.env.ID, platform.AITypePrompt, "risky", ""); err != nil {
		t.Fatalf("resolve after reinstate: %v", err)
	}
}

func TestUnsupportedTypesRefused(t *testing.T) {
	f := newFixture(t)
	// mcp/pipeline lack immutable-version semantics in gonacos → hosting
	// is refused instead of faking pinning with name@v1 strings (§7).
	if _, err := f.svc.RegisterResource(f.ws.ID, f.env.ID, platform.AITypeMCP, "tool", "t", "alice"); !errors.Is(err, ErrTypeUnsupported) {
		t.Fatalf("mcp register: %v", err)
	}
	if _, err := f.svc.RegisterResource(f.ws.ID, f.env.ID, platform.AITypePipeline, "pipe", "p", "alice"); !errors.Is(err, ErrTypeUnsupported) {
		t.Fatalf("pipeline register: %v", err)
	}
	if _, err := f.svc.RegisterResource(f.ws.ID, f.env.ID, "model", "gpt-x", "m", "alice"); !errors.Is(err, ErrTypeUnsupported) {
		t.Fatalf("model register: %v", err)
	}
}

func TestSkillAndAgentSpecFlows(t *testing.T) {
	f := newFixture(t)
	for _, typ := range []string{platform.AITypeSkill, platform.AITypeAgentSpec} {
		if _, err := f.svc.RegisterResource(f.ws.ID, f.env.ID, typ, "res-1", "Resource One", "alice"); err != nil {
			t.Fatalf("%s register: %v", typ, err)
		}
		pin, err := f.svc.PublishVersion(f.ws.ID, f.env.ID, typ, "res-1", "content one", "alice")
		if err != nil {
			t.Fatalf("%s publish: %v", typ, err)
		}
		pin2, err := f.svc.PublishVersion(f.ws.ID, f.env.ID, typ, "res-1", "content two", "alice")
		if err != nil {
			t.Fatalf("%s publish 2: %v", typ, err)
		}
		if pin2.Version == pin.Version {
			t.Fatalf("%s: versions must differ", typ)
		}
		got, err := f.svc.Resolve(f.ws.ID, f.env.ID, typ, "res-1", "")
		if err != nil {
			t.Fatalf("%s resolve: %v", typ, err)
		}
		if got.Content != "content two" {
			t.Fatalf("%s content = %q", typ, got.Content)
		}
		old, err := f.svc.Resolve(f.ws.ID, f.env.ID, typ, "res-1", pin.Version)
		if err != nil || old.Content != "content one" {
			t.Fatalf("%s v1 = %q, %v", typ, old.Content, err)
		}
	}
}

func TestEmptyContentRefused(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.RegisterResource(f.ws.ID, f.env.ID, platform.AITypePrompt, "p", "p", "alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := f.svc.PublishVersion(f.ws.ID, f.env.ID, platform.AITypePrompt, "p", "   ", "alice"); err == nil {
		t.Fatal("empty content must be refused")
	}
}

func TestModelAllowlist(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SetModelAllowlist(f.ws.ID, []platform.ModelAllowEntry{
		{Provider: "qwen", Model: "qwen-max", Enabled: true},
		{Provider: "anthropic", Model: "claude-old", Enabled: false},
	}); err != nil {
		t.Fatalf("SetModelAllowlist: %v", err)
	}
	if _, err := f.svc.ResolveModelRef(f.ws.ID, "qwen", "qwen-max"); err != nil {
		t.Fatalf("allowed model: %v", err)
	}
	if _, err := f.svc.ResolveModelRef(f.ws.ID, "anthropic", "claude-old"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("disabled model must be refused: %v", err)
	}
	if _, err := f.svc.ResolveModelRef(f.ws.ID, "openai", "gpt-x"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("unlisted model must be refused: %v", err)
	}
	// Cross-workspace allowlists are separate.
	if _, err := f.svc.ResolveModelRef(f.other.ID, "qwen", "qwen-max"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("allowlist must not leak across workspaces: %v", err)
	}
}

func TestValidateFlagModelRefs(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SetModelAllowlist(f.ws.ID, []platform.ModelAllowEntry{
		{Provider: "qwen", Model: "qwen-max", Enabled: true},
	}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	ok := map[string]any{
		"control": map[string]any{"modelRef": map[string]any{"provider": "qwen", "model": "qwen-max"}},
	}
	if err := f.svc.ValidateFlagModelRefs(f.ws.ID, ok); err != nil {
		t.Fatalf("valid modelRef: %v", err)
	}
	bad := map[string]any{
		"candidate": map[string]any{"modelRef": map[string]any{"provider": "openai", "model": "gpt-x"}},
	}
	if err := f.svc.ValidateFlagModelRefs(f.ws.ID, bad); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("unlisted modelRef must fail publish validation: %v", err)
	}
	malformed := map[string]any{
		"x": map[string]any{"modelRef": map[string]any{"provider": "qwen"}},
	}
	if err := f.svc.ValidateFlagModelRefs(f.ws.ID, malformed); err == nil {
		t.Fatal("incomplete modelRef must fail")
	}
	// Variations without modelRef pass untouched.
	if err := f.svc.ValidateFlagModelRefs(f.ws.ID, map[string]any{"plain": true}); err != nil {
		t.Fatalf("plain variations: %v", err)
	}
}
