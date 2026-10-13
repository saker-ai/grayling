// OpenFeature contract validation for the golab provider. These tests drive
// the OFFICIAL OpenFeature Go SDK surface (isolated API → client → provider)
// against a REAL golab server (HTTP, gonacos config substrate, admin
// pipeline), asserting the spec-visible contract: values, variants, reasons,
// error codes, provider state transitions and exposure attribution
// (docs/技术方案.md §8).
package ofprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godeps/grayling/internal/api"
	"github.com/godeps/grayling/internal/configstore"
	"github.com/godeps/grayling/internal/platform"
	"github.com/godeps/grayling/pkg/bucketing"
	"github.com/godeps/grayling/pkg/model"
	"github.com/godeps/grayling/pkg/sdk"
	gconfig "github.com/godeps/gonacos/pkg/config"
	of "github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/isolated"
)

// ---------------------------------------------------------------------------
// Harness: a real golab server + admin provisioning
// ---------------------------------------------------------------------------

type harness struct {
	t        *testing.T
	store    *platform.Store
	srv      *httptest.Server
	api      *api.Server
	owner    string
	reviewer string
	sdkTok   string
	colTok   string
	ws       *platform.Workspace
	env      *platform.Environment
	baseURL  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := configstore.NewGonacosStore(gconfig.NewService())
	apiSrv := api.NewServer(store, cfg, api.Options{EvaluationSampleRate: 1})
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)

	h := &harness{t: t, store: store, srv: srv, api: apiSrv, baseURL: srv.URL}
	ct, err := store.CreateToken(platform.TokenAdmin, "root", platform.RoleOwner, "", "", "bootstrap")
	if err != nil {
		t.Fatalf("owner token: %v", err)
	}
	h.owner = ct.Secret
	h.ws, err = store.CreateWorkspace("acme", "Acme")
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	h.env, err = store.CreateEnvironment(h.ws.ID, "prod", "")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	reviewer, err := store.CreateToken(platform.TokenAdmin, "reviewer", platform.RolePublisher, h.ws.ID, "", "bootstrap")
	if err != nil {
		t.Fatalf("reviewer token: %v", err)
	}
	h.reviewer = reviewer.Secret
	sdkCT, err := store.CreateToken(platform.TokenSDK, "sdk", "", h.ws.ID, h.env.ID, "root")
	if err != nil {
		t.Fatalf("sdk token: %v", err)
	}
	h.sdkTok = sdkCT.Secret
	colCT, err := store.CreateToken(platform.TokenCollect, "col", "", h.ws.ID, h.env.ID, "root")
	if err != nil {
		t.Fatalf("collect token: %v", err)
	}
	h.colTok = colCT.Secret
	return h
}

func (h *harness) do(method, path, token string, body any) int {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		h.t.Logf("%s %s → %d: %s", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode
}

// publishFlag runs the draft→submit→approve→publish pipeline.
func (h *harness) publishFlag(flag *model.Flag) {
	h.t.Helper()
	base := fmt.Sprintf("/api/v1/admin/workspaces/acme/environments/prod/flags/%s", flag.Key)
	if code := h.do("PUT", base+"/draft", h.owner, map[string]any{"content": flag}); code != http.StatusOK {
		h.t.Fatalf("draft %s: %d", flag.Key, code)
	}
	if code := h.do("POST", base+"/submit", h.owner, map[string]any{"reason": "provider verification"}); code != http.StatusOK {
		h.t.Fatalf("submit %s: %d", flag.Key, code)
	}
	if code := h.do("POST", base+"/approve", h.reviewer, map[string]any{"note": "ok"}); code != http.StatusOK {
		h.t.Fatalf("approve %s: %d", flag.Key, code)
	}
	if code := h.do("POST", base+"/publish", h.owner, map[string]any{"note": "ship"}); code != http.StatusOK {
		h.t.Fatalf("publish %s: %d", flag.Key, code)
	}
}

func boolFlag(key string, disabled bool) *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion, Key: key, Type: model.FlagBool,
		Disable:     disabled,
		Variations:  map[string]any{"on": true, "off": false},
		DefaultRule: &model.Rule{Variation: "off"},
	}
}

// newOFAPI registers a golab provider on a fresh isolated OpenFeature API
// and waits for readiness. Returns the OF client and the provider.
func newOFAPI(t *testing.T, h *harness, mutate func(*sdk.Options)) (*of.Client, *Provider) {
	t.Helper()
	opts := sdk.Options{
		BaseURL: h.baseURL, Token: h.sdkTok, CollectToken: h.colTok,
		PollInterval: 100 * time.Millisecond, MaxStaleness: 2 * time.Minute,
		ExposureFlushInterval: 100 * time.Millisecond, ExposureBatchSize: 100,
	}
	if mutate != nil {
		mutate(&opts)
	}
	provider := NewProvider(Options{ClientOptions: opts, MonitorInterval: 50 * time.Millisecond})
	api := isolated.NewAPI()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := api.SetProviderAndWait(ctx, provider); err != nil {
		t.Fatalf("SetProviderAndWait: %v", err)
	}
	t.Cleanup(provider.Shutdown)
	return api.NewClient(), provider
}

// ---------------------------------------------------------------------------
// Contract tests
// ---------------------------------------------------------------------------

func TestOFReasonAndVariantMapping(t *testing.T) {
	h := newHarness(t)

	h.publishFlag(boolFlag("of-bool", false))
	h.publishFlag(boolFlag("of-killed", true))

	h.publishFlag(&model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "of-string", Type: model.FlagString,
		Fields:     []string{"tenant"},
		Variations: map[string]any{"base": "base-v", "targeted": "targeted-v"},
		Targeting: []model.Rule{{
			Name:        "acme-only",
			Constraints: []model.Constraint{{Field: "tenant", Op: model.OpEq, Value: "acme"}},
			Variation:   "targeted",
		}},
		DefaultRule: &model.Rule{Variation: "base"},
	})

	h.publishFlag(&model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "of-split", Type: model.FlagString,
		Fields:     []string{"user_id"},
		Bucketing:  &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: "of"},
		Variations: map[string]any{"a": "A", "b": "B"},
		Targeting: []model.Rule{{
			Name:       "fifty",
			Percentage: map[string]float64{"a": 50, "b": 50},
		}},
		DefaultRule: &model.Rule{Variation: "a"},
	})

	h.publishFlag(&model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "of-json", Type: model.FlagJSON,
		Variations:  map[string]any{"cfg": map[string]any{"mode": "fast", "retries": float64(3)}},
		DefaultRule: &model.Rule{Variation: "cfg"},
	})

	client, _ := newOFAPI(t, h, nil)
	ctx := context.Background()

	// DEFAULT reason, variant reported.
	bd, err := client.BooleanValueDetails(ctx, "of-bool", true, of.EvaluationContext{})
	if err != nil {
		t.Fatalf("bool eval: %v", err)
	}
	if bd.Value != false || bd.Variant != "off" || bd.Reason != of.DefaultReason {
		t.Fatalf("bool: %+v", bd)
	}
	if bd.FlagMetadata["golabReason"] != string(model.ReasonDefault) {
		t.Fatalf("golab reason metadata: %v", bd.FlagMetadata)
	}

	// DISABLED kill switch.
	bd, err = client.BooleanValueDetails(ctx, "of-killed", true, of.EvaluationContext{})
	if err != nil {
		t.Fatalf("killed eval: %v", err)
	}
	if bd.Value != false || bd.Reason != of.DisabledReason || bd.Variant != "off" {
		t.Fatalf("killed: %+v", bd)
	}

	// TARGETING_MATCH via constraint.
	sd, err := client.StringValueDetails(ctx, "of-string", "fallback",
		of.NewEvaluationContext("", map[string]any{"tenant": "acme"}))
	if err != nil {
		t.Fatalf("string eval: %v", err)
	}
	if sd.Value != "targeted-v" || sd.Reason != of.TargetingMatchReason || sd.Variant != "targeted" {
		t.Fatalf("targeting match: %+v", sd)
	}
	sd, err = client.StringValueDetails(ctx, "of-string", "fallback",
		of.NewEvaluationContext("", map[string]any{"tenant": "globex"}))
	if err != nil {
		t.Fatalf("string eval 2: %v", err)
	}
	if sd.Value != "base-v" || sd.Reason != of.DefaultReason {
		t.Fatalf("non-match: %+v", sd)
	}

	// SPLIT via targetingKey mirrored into the bucketing unit field.
	sd, err = client.StringValueDetails(ctx, "of-split", "fallback",
		of.NewEvaluationContext("user-42", nil))
	if err != nil {
		t.Fatalf("split eval: %v", err)
	}
	if sd.Reason != of.SplitReason || (sd.Value != "A" && sd.Value != "B") {
		t.Fatalf("split: %+v", sd)
	}
	// Determinism: same targetingKey → same value.
	sd2, _ := client.StringValueDetails(ctx, "of-split", "fallback",
		of.NewEvaluationContext("user-42", nil))
	if sd2.Value != sd.Value || sd2.Variant != sd.Variant {
		t.Fatalf("split not deterministic: %+v vs %+v", sd, sd2)
	}

	// Object evaluation.
	od, err := client.ObjectValueDetails(ctx, "of-json", map[string]any{}, of.EvaluationContext{})
	if err != nil {
		t.Fatalf("object eval: %v", err)
	}
	obj, ok := od.Value.(map[string]any)
	if !ok || obj["mode"] != "fast" {
		t.Fatalf("object: %+v", od)
	}
}

func TestOFErrorCodes(t *testing.T) {
	h := newHarness(t)
	h.publishFlag(&model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "need-unit", Type: model.FlagString,
		Fields:     []string{"user_id"},
		Bucketing:  &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: "s"},
		Variations: map[string]any{"a": "A", "b": "B"},
		Targeting: []model.Rule{{
			Name:       "fifty",
			Percentage: map[string]float64{"a": 50, "b": 50},
		}},
		DefaultRule: &model.Rule{Variation: "a"},
	})
	client, _ := newOFAPI(t, h, nil)
	ctx := context.Background()

	// FLAG_NOT_FOUND → caller default + error.
	sd, err := client.StringValueDetails(ctx, "does-not-exist", "safe", of.EvaluationContext{})
	if err == nil {
		t.Fatalf("unknown flag must error: %+v", sd)
	}
	if sd.Value != "safe" || sd.Reason != of.ErrorReason || sd.ErrorCode != of.FlagNotFoundCode {
		t.Fatalf("flag-not-found: %+v (%v)", sd, err)
	}

	// Missing randomization unit → TARGETING_KEY_MISSING (§6: 缺单元 →
	// MISSING_BUCKET_KEY 返回显式默认值、绝不随机), caller default served.
	sd, err = client.StringValueDetails(ctx, "need-unit", "safe", of.EvaluationContext{})
	if err == nil {
		t.Fatalf("missing unit must error: %+v", sd)
	}
	if sd.Value != "safe" || sd.ErrorCode != of.TargetingKeyMissingCode || sd.Reason != of.ErrorReason {
		t.Fatalf("targeting-key-missing: %+v (%v)", sd, err)
	}
}

func TestOFNumericMapping(t *testing.T) {
	h := newHarness(t)
	h.publishFlag(&model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "of-int", Type: model.FlagNumber,
		Variations:  map[string]any{"answer": float64(42)},
		DefaultRule: &model.Rule{Variation: "answer"},
	})
	h.publishFlag(&model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "of-frac", Type: model.FlagNumber,
		Variations:  map[string]any{"half": 0.5},
		DefaultRule: &model.Rule{Variation: "half"},
	})
	client, _ := newOFAPI(t, h, nil)
	ctx := context.Background()

	id, err := client.IntValueDetails(ctx, "of-int", 7, of.EvaluationContext{})
	if err != nil {
		t.Fatalf("int eval: %v", err)
	}
	if id.Value != 42 || id.Variant != "answer" || id.Reason != of.DefaultReason {
		t.Fatalf("int: %+v", id)
	}
	fd, err := client.FloatValueDetails(ctx, "of-frac", 1.0, of.EvaluationContext{})
	if err != nil {
		t.Fatalf("float eval: %v", err)
	}
	if fd.Value != 0.5 {
		t.Fatalf("float: %+v", fd)
	}
	// Fractional value on an Int evaluation → TYPE_MISMATCH + caller default.
	id, err = client.IntValueDetails(ctx, "of-frac", 7, of.EvaluationContext{})
	if err == nil {
		t.Fatalf("fractional int must error: %+v", id)
	}
	if id.Value != 7 || id.ErrorCode != of.TypeMismatchCode {
		t.Fatalf("type mismatch: %+v (%v)", id, err)
	}
}

// OpenFeature spec v0.9.0 §2.2 requires every typed resolution to return the
// caller default and an error code for abnormal resolution, while normal
// resolutions must leave the error code empty.
func TestOFTypedResolutionContract(t *testing.T) {
	h := newHarness(t)
	h.publishFlag(boolFlag("typed-bool", false))
	h.publishFlag(&model.Flag{SchemaVersion: model.SchemaVersion, Key: "typed-string", Type: model.FlagString,
		Variations: map[string]any{"v": "ready"}, DefaultRule: &model.Rule{Variation: "v"}})
	h.publishFlag(&model.Flag{SchemaVersion: model.SchemaVersion, Key: "typed-number", Type: model.FlagNumber,
		Variations: map[string]any{"v": float64(2)}, DefaultRule: &model.Rule{Variation: "v"}})
	h.publishFlag(&model.Flag{SchemaVersion: model.SchemaVersion, Key: "typed-object", Type: model.FlagJSON,
		Variations: map[string]any{"v": map[string]any{"ok": true}}, DefaultRule: &model.Rule{Variation: "v"}})
	h.publishFlag(&model.Flag{SchemaVersion: model.SchemaVersion, Key: "typed-null", Type: model.FlagJSON,
		Variations: map[string]any{"v": nil}, DefaultRule: &model.Rule{Variation: "v"}})
	client, _ := newOFAPI(t, h, nil)
	ctx := t.Context()
	ec := of.EvaluationContext{}
	bd, err := client.BooleanValueDetails(ctx, "typed-bool", true, ec)
	if err != nil || bd.Value || bd.Variant != "off" || bd.Reason != of.DefaultReason || bd.ErrorCode != "" {
		t.Fatalf("boolean normal resolution: %+v, %v", bd, err)
	}
	sd, err := client.StringValueDetails(ctx, "typed-string", "fallback", ec)
	if err != nil || sd.Value != "ready" || sd.Variant != "v" || sd.ErrorCode != "" {
		t.Fatalf("string normal resolution: %+v, %v", sd, err)
	}
	fd, err := client.FloatValueDetails(ctx, "typed-number", 9, ec)
	if err != nil || fd.Value != 2 || fd.Variant != "v" || fd.ErrorCode != "" {
		t.Fatalf("float normal resolution: %+v, %v", fd, err)
	}
	id, err := client.IntValueDetails(ctx, "typed-number", 9, ec)
	if err != nil || id.Value != 2 || id.Variant != "v" || id.ErrorCode != "" {
		t.Fatalf("integer normal resolution: %+v, %v", id, err)
	}
	od, err := client.ObjectValueDetails(ctx, "typed-object", map[string]any{"safe": true}, ec)
	if err != nil || od.Variant != "v" || od.ErrorCode != "" {
		t.Fatalf("object normal resolution: %+v, %v", od, err)
	}
	obj, ok := od.Value.(map[string]any)
	if !ok || obj["ok"] != true {
		t.Fatalf("object value: %+v", od)
	}
	bd, err = client.BooleanValueDetails(ctx, "typed-string", true, ec)
	if err == nil || !bd.Value || bd.Reason != of.ErrorReason || bd.ErrorCode != of.TypeMismatchCode {
		t.Fatalf("boolean type mismatch: %+v, %v", bd, err)
	}
	fd, err = client.FloatValueDetails(ctx, "typed-string", 9, ec)
	if err == nil || fd.Value != 9 || fd.Reason != of.ErrorReason || fd.ErrorCode != of.TypeMismatchCode {
		t.Fatalf("float type mismatch: %+v, %v", fd, err)
	}
	od, err = client.ObjectValueDetails(ctx, "typed-string", map[string]any{"safe": true}, ec)
	if err == nil || od.Reason != of.ErrorReason || od.ErrorCode != of.TypeMismatchCode {
		t.Fatalf("object type mismatch: %+v, %v", od, err)
	}
	obj, ok = od.Value.(map[string]any)
	if !ok || obj["safe"] != true {
		t.Fatalf("object caller default: %+v", od)
	}
	od, err = client.ObjectValueDetails(ctx, "typed-null", map[string]any{"safe": true}, ec)
	if err == nil || od.Reason != of.ErrorReason || od.ErrorCode != of.TypeMismatchCode {
		t.Fatalf("null is not an object: %+v, %v", od, err)
	}
}

func TestOFContextTransformation(t *testing.T) {
	h := newHarness(t)
	h.publishFlag(&model.Flag{SchemaVersion: model.SchemaVersion, Key: "context-unit", Type: model.FlagString,
		Fields: []string{"user_id"}, Variations: map[string]any{"matched": "yes", "default": "no"},
		Targeting:   []model.Rule{{Constraints: []model.Constraint{{Field: "user_id", Op: model.OpEq, Value: "explicit"}}, Variation: "matched"}},
		DefaultRule: &model.Rule{Variation: "default"}})
	client, _ := newOFAPI(t, h, nil)
	ctx := t.Context()
	for _, tc := range []struct {
		name string
		ec   of.EvaluationContext
		want string
	}{
		{name: "explicit attribute wins", ec: of.NewEvaluationContext("other", map[string]any{"user_id": "explicit"}), want: "yes"},
		{name: "targeting key mirrored", ec: of.NewEvaluationContext("explicit", nil), want: "yes"},
		{name: "unmatched targeting key", ec: of.NewEvaluationContext("other", nil), want: "no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail, err := client.StringValueDetails(ctx, "context-unit", "fallback", tc.ec)
			if err != nil || detail.Value != tc.want || detail.ErrorCode != "" {
				t.Fatalf("context resolution: %+v, %v", detail, err)
			}
		})
	}
}

// TestOFExperimentExposureAttribution proves the full chain: OpenFeature
// evaluation → golab local split → enrollment in a RUNNING experiment →
// exposure batched to /api/v1/collect → server-side aggregate under the
// tenant-scoped unit hash derived from the OpenFeature targetingKey.
func TestOFExperimentExposureAttribution(t *testing.T) {
	h := newHarness(t)
	flag := &model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "of-exp", Type: model.FlagString,
		Fields:     []string{"user_id"},
		Bucketing:  &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: "of-exp"},
		Variations: map[string]any{"control": "c", "treatment": "t"},
		Targeting: []model.Rule{{
			Name:       "fifty",
			Percentage: map[string]float64{"control": 50, "treatment": 50},
		}},
		DefaultRule: &model.Rule{Variation: "control"},
	}
	h.publishFlag(flag)

	// Register + run the experiment through the admin API (binding is
	// high-risk → two-person approval with a second publisher token).
	var exp platform.Experiment
	expReq := map[string]any{
		"flagKey": "of-exp", "unit": "user_id",
		"variants":    []string{"control", "treatment"},
		"proportions": map[string]float64{"control": 50, "treatment": 50},
		"primaryMetric": map[string]any{
			"key": "conversion", "type": "proportion", "direction": "increase",
		},
		"alpha": 0.05, "power": 0.8, "mde": 0.02, "minSamplePerArm": 10,
		"analysisWindow": int64(7 * 24 * time.Hour),
	}
	// Capture the created experiment from the response body.
	reqBody, _ := json.Marshal(expReq)
	req, _ := http.NewRequest("POST",
		h.srv.URL+"/api/v1/admin/workspaces/acme/environments/prod/experiments",
		bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+h.owner)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create experiment: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create experiment: %d %s", resp.StatusCode, respBody)
	}
	if err := json.Unmarshal(respBody, &exp); err != nil {
		t.Fatalf("experiment decode: %v", err)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/review",
		h.owner, map[string]any{"note": "pre"}); code != http.StatusOK {
		t.Fatalf("review: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/bind",
		h.owner, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	pubCT, err := h.store.CreateToken(platform.TokenAdmin, "publisher-b", platform.RolePublisher, h.ws.ID, "", "root")
	if err != nil {
		t.Fatalf("publisher token: %v", err)
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/of-exp/approve",
		pubCT.Secret, map[string]any{"note": "second pair of eyes"}); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/of-exp/publish",
		h.owner, map[string]any{"note": "bind"}); code != http.StatusOK {
		t.Fatalf("publish binding: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/transition",
		h.owner, map[string]any{"to": "running"}); code != http.StatusOK {
		t.Fatalf("run: %d", code)
	}

	client, provider := newOFAPI(t, h, nil)
	_ = provider
	ctx := context.Background()

	// Wait until the running binding reached the SDK snapshot.
	var sd of.StringEvaluationDetails
	deadline := time.Now().Add(5 * time.Second)
	evalCtx := of.NewEvaluationContext("user-77", nil)
	for time.Now().Before(deadline) {
		sd, err = client.StringValueDetails(ctx, "of-exp", "?", evalCtx)
		if err == nil && sd.Reason == of.SplitReason && sd.FlagMetadata["golabExperimentId"] == exp.ID {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sd.Reason != of.SplitReason {
		t.Fatalf("experiment split never observed: %+v (%v)", sd, err)
	}
	if sd.FlagMetadata["golabExperimentId"] != exp.ID || sd.FlagMetadata["golabExposureSource"] != "split" {
		t.Fatalf("experiment metadata: %v", sd.FlagMetadata)
	}

	// The exposure must land server-side under the HMAC of the OF
	// targetingKey (flush interval 100ms; generous CI bound).
	wantHash := h.store.HashUnit(h.ws.ID, "user-77")
	deadline = time.Now().Add(5 * time.Second)
	exposed := false
	for time.Now().Before(deadline) {
		set := h.store.SplitExposedUnitSet(h.ws.ID, h.env.ID, exp.ID, exp.Epoch)
		if units, ok := set[sd.Variant]; ok && units[wantHash] {
			exposed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !exposed {
		t.Fatalf("exposure from OpenFeature evaluation never reached the store (variant %s)", sd.Variant)
	}
	decisionID, ok := sd.FlagMetadata["golabDecisionId"].(string)
	if !ok || decisionID == "" {
		t.Fatalf("decision metadata: %+v", sd.FlagMetadata)
	}
	diagnostic, ok := provider.client.Diagnose(decisionID)
	if !ok || diagnostic.Result.VariationType != sd.Variant || diagnostic.Result.ManifestRevision == 0 {
		t.Fatalf("local diagnostic: %+v, %v", diagnostic, ok)
	}
	client.Track(ctx, "conversion", evalCtx,
		of.NewTrackingEventDetails(1).Add("decisionId", decisionID))
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, conversions := h.store.EventMetricTotals(h.ws.ID, h.env.ID)
		if conversions > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("OpenFeature Track conversion did not reach collection")
}

func TestOFProviderStateLifecycle(t *testing.T) {
	h := newHarness(t)
	h.publishFlag(boolFlag("of-state", false))

	client, provider := newOFAPI(t, h, func(o *sdk.Options) {
		o.MaxStaleness = 400 * time.Millisecond
		o.PollInterval = 100 * time.Millisecond
	})
	if st := client.State(); st != of.ReadyState {
		t.Fatalf("state after SetProviderAndWait: %s, want READY", st)
	}

	// Kill the server: after MaxStaleness the monitor must degrade the
	// provider to STALE (§5.2 kill-switch window) and evaluations must fall
	// back to caller defaults with an error.
	h.srv.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && client.State() != of.StaleState {
		time.Sleep(50 * time.Millisecond)
	}
	if st := client.State(); st != of.StaleState {
		t.Fatalf("state after staleness: %s, want STALE", st)
	}
	bd, err := client.BooleanValueDetails(context.Background(), "of-state", true, of.EvaluationContext{})
	if err == nil {
		t.Fatalf("stale evaluation must error: %+v", bd)
	}
	if bd.Value != true || bd.Reason != of.ErrorReason {
		t.Fatalf("stale details: %+v", bd)
	}

	// Shutdown must be clean and idempotent-ish (monitor stopped, client closed).
	provider.Shutdown()
}

func TestOFNotReadyWithoutInit(t *testing.T) {
	// Evaluations before Init (or with a failed provider) must serve caller
	// defaults with PROVIDER_NOT_READY, never a random assignment.
	p := NewProvider(Options{ClientOptions: sdk.Options{BaseURL: "http://127.0.0.1:1", Token: "x"}})
	d := p.BooleanEvaluation(context.Background(), "anything", true, of.FlattenedContext{})
	if d.Value != true || d.Reason != of.ErrorReason || d.ResolutionError.Error() == "" {
		t.Fatalf("uninitialized provider: %+v", d)
	}
}

func TestOFInitValidation(t *testing.T) {
	p := NewProvider(Options{}) // no client, no BaseURL/Token
	if err := p.Init(of.EvaluationContext{}); err == nil {
		t.Fatal("Init without BaseURL/Token must fail")
	}
}

func TestRemoteEvaluationSampleHasNoContextOrValue(t *testing.T) {
	h := newHarness(t)
	h.publishFlag(&model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "sampled", Type: model.FlagString,
		Variations:  map[string]any{"chosen": "private-flag-value"},
		DefaultRule: &model.Rule{Variation: "chosen"},
	})
	body := []byte(`{"flagKey":"sampled","context":{"targetingKey":"private-user"}}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		h.srv.URL+"/api/v1/sdk/eval", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.sdkTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("eval status %d", resp.StatusCode)
	}
	var result struct {
		Result model.Result `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-h.api.SampledEvents():
		if event.DecisionID != result.Result.DecisionID ||
			event.Attributes["feature_flag.result.variant"] != "chosen" {
			t.Fatalf("sampled event: %+v", event)
		}
		encoded, _ := json.Marshal(event)
		if bytes.Contains(encoded, []byte("private-user")) ||
			bytes.Contains(encoded, []byte("private-flag-value")) {
			t.Fatalf("sampled event leaked private data: %s", encoded)
		}
	case <-time.After(time.Second):
		t.Fatal("missing server sampled event")
	}
}

func TestProviderLifecycleEvents(t *testing.T) {
	var revision atomic.Int64
	revision.Store(1)
	var failing atomic.Bool
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sdk/config" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if failing.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		snap := model.Snapshot{
			SchemaVersion: model.SchemaVersion, Workspace: "w", Environment: "e",
			ManifestRevision: revision.Load(), GeneratedAt: time.Now().UTC(),
			Flags: map[string]*model.Flag{}, Segments: map[string]*model.Segment{},
		}
		_ = json.NewEncoder(w).Encode(snap)
	}))
	defer srv.Close()
	p := NewProvider(Options{ClientOptions: sdk.Options{
		BaseURL: srv.URL, Token: "test", PollInterval: time.Hour,
		MaxStaleness: 180 * time.Millisecond,
	}, MonitorInterval: 10 * time.Millisecond})
	if err := p.Init(of.EvaluationContext{}); err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown()
	next := func(want of.EventType) of.Event {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case ev := <-p.EventChannel():
				if ev.EventType == want {
					return ev
				}
			case <-deadline:
				t.Fatalf("missing %s event", want)
				return of.Event{}
			}
		}
	}
	startupError := next(of.ProviderError)
	if startupError.Message == "" {
		t.Fatal("startup error event lacked a reason")
	}
	failing.Store(false)
	if err := p.client.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	next(of.ProviderReady)
	revision.Store(2)
	if err := p.client.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	changed := next(of.ProviderConfigChange)
	if changed.EventMetadata["manifestRevision"] != int64(2) {
		t.Fatalf("revision metadata: %+v", changed)
	}
	failing.Store(true)
	if err := p.client.Sync(t.Context()); err == nil {
		t.Fatal("upstream error was ignored")
	}
	errorEvent := next(of.ProviderError)
	if errorEvent.Message == "" || errorEvent.ErrorCode != of.GeneralCode {
		t.Fatalf("error event: %+v", errorEvent)
	}
	_ = p.client.Sync(t.Context())
	staleSeen := false
	select {
	case ev := <-p.EventChannel():
		if ev.EventType == of.ProviderError {
			t.Fatalf("repeated failure emitted error storm: %+v", ev)
		}
		staleSeen = ev.EventType == of.ProviderStale
	case <-time.After(30 * time.Millisecond):
	}
	if !staleSeen {
		next(of.ProviderStale)
	}
	failing.Store(false)
	if err := p.client.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	next(of.ProviderReady)
	select {
	case ev := <-p.EventChannel():
		if ev.EventType == of.ProviderConfigChange {
			t.Fatalf("unchanged revision emitted config change: %+v", ev)
		}
	case <-time.After(50 * time.Millisecond):
	}
}
