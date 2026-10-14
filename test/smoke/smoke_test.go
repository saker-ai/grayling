package smoke

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/godeps/gonacos/pkg/config"
	"github.com/godeps/gonacos/pkg/server"
	gffeval "github.com/thomaspoignant/go-feature-flag/modules/core/evaluation"
	"github.com/thomaspoignant/go-feature-flag/modules/core/ffcontext"
	gffflag "github.com/thomaspoignant/go-feature-flag/modules/core/flag"
)

// gonacosRoot resolves the gonacos contract root (api/openapi/upstream).
// It PREFERS the pinned module's own directory from the module cache so the
// contract test exercises exactly the dependency go.mod pins (docs/技术方案.md
// §12.1); the sibling checkout is only a development fallback.
func gonacosRoot(t *testing.T) string {
	t.Helper()
	if out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}",
		"github.com/godeps/gonacos").Output(); err == nil {
		dir := strings.TrimSpace(string(out))
		if _, err := os.Stat(filepath.Join(dir, "api", "openapi", "upstream")); err == nil {
			return dir
		}
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "gonacos")
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(abs, "api", "openapi", "upstream")); err != nil {
		t.Fatalf("gonacos contract root not found (module cache or %s): %v", abs, err)
	}
	return abs
}

// TestEmbeddedGonacosRoundtrip verifies the pinned gonacos sibling module can
// start in-process (embedded miniredis) and serve Config.Publish/Get, which is
// the storage substrate Grayling builds on (docs/技术方案.md §2, §12.1).
func TestEmbeddedGonacosRoundtrip(t *testing.T) {
	srv, err := server.New(
		server.WithAddr("127.0.0.1:0"),
		server.WithGRPCAddr("127.0.0.1:0"),
		server.WithRoot(gonacosRoot(t)),
		server.WithDataDir(t.TempDir()),
	)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	bundle := srv.Services()
	if bundle == nil || bundle.Config == nil || bundle.AI == nil || bundle.Auth == nil {
		t.Fatal("Services() returned incomplete bundle")
	}

	req := config.PublishRequest{
		NamespaceID: "grayling-smoke",
		GroupName:   "GRAYLING_FLAGS",
		DataID:      "grayling.flag.smoke.r1",
		Content:     `{"schemaVersion":1,"key":"smoke"}`,
		Type:        "json",
	}
	if err := bundle.Config.Publish(req); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	item, err := bundle.Config.Get(req.NamespaceID, req.GroupName, req.DataID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.Content != req.Content {
		t.Fatalf("Get content = %q, want %q", item.Content, req.Content)
	}

	// AI service must be reachable for prompt version pinning later (§7).
	if _, err := bundle.AI.CreatePromptDraft("smoke-p", "smoke", "hello", "tester", nil, nil, "", nil); err != nil {
		t.Fatalf("CreatePromptDraft: %v", err)
	}
	if _, err := bundle.AI.SubmitPrompt("smoke-p"); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	if _, err := bundle.AI.PublishPrompt("smoke-p", false); err != nil {
		t.Fatalf("PublishPrompt: %v", err)
	}
	content, err := bundle.AI.DownloadPromptVersion("smoke-p", "v1")
	if err != nil {
		t.Fatalf("DownloadPromptVersion: %v", err)
	}
	if content != "hello" {
		t.Fatalf("prompt content = %q, want %q", content, "hello")
	}
}

// TestGffCoreLegacyEvaluate verifies the gff core module compiles against this
// toolchain and its native evaluation path is callable for legacy-gff-v1 mode
// (docs/技术方案.md §5.2). It is NOT used for Grayling native fixed-bucket
// semantics.
func TestGffCoreLegacyEvaluate(t *testing.T) {
	var trueAny any = true
	var falseAny any = false
	variations := map[string]*any{
		"true":  &trueAny,
		"false": &falseAny,
	}
	variationName := "true"
	f := &gffflag.InternalFlag{
		Variations:  &variations,
		DefaultRule: &gffflag.Rule{VariationResult: &variationName},
	}
	ctx := ffcontext.NewEvaluationContext("user-1")
	res, err := gffeval.Evaluate[bool](f, "smoke", ctx, gffflag.Context{}, "bool", false)
	if err != nil {
		t.Fatalf("legacy Evaluate: %v", err)
	}
	if res.Value != true {
		t.Fatalf("legacy Evaluate value = %v, want true", res.Value)
	}
}
