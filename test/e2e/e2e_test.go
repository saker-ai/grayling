// Package e2e exercises the full GoLab loop over real HTTP against the real
// gonacos config substrate: create → review → publish → SSE/poll sync →
// local evaluation → exposures/conversions → rollback → kill switch → stale
// fallback (docs/技术方案.md §13 P1/P2 acceptance).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/godeps/grayling/internal/api"
	"github.com/godeps/grayling/internal/configstore"
	"github.com/godeps/grayling/internal/platform"
	"github.com/godeps/grayling/pkg/bucketing"
	"github.com/godeps/grayling/pkg/model"
	"github.com/godeps/grayling/pkg/sdk"
	gconfig "github.com/godeps/gonacos/pkg/config"
)

type harness struct {
	t        *testing.T
	store    *platform.Store
	api      *api.Server
	rawCfg   *gconfig.Service
	srv      *httptest.Server
	ws       platform.Workspace
	env      platform.Environment
	owner    string // admin secret (global owner)
	reviewer string // independent publisher for every approval
	sdkTok   string
	colTok   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rawCfg := gconfig.NewService()
	cfg := configstore.NewGonacosStore(rawCfg)
	apiSrv := api.NewServer(store, cfg, api.Options{})
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)

	h := &harness{t: t, store: store, api: apiSrv, rawCfg: rawCfg, srv: srv}

	// Bootstrap global owner credential (cmd/golab does this at startup).
	ct, err := store.CreateToken(platform.TokenAdmin, "bootstrap-owner", platform.RoleOwner, "", "", "bootstrap")
	if err != nil {
		t.Fatalf("owner token: %v", err)
	}
	h.owner = ct.Secret
	return h
}
func (h *harness) do(method, path, token string, body any, out any) int {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal: %v", err)
		}
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
	// Decode whatever JSON came back — including error statuses like the
	// 423 revocation gate, whose payload tests assert on.
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil && resp.StatusCode < 400 {
			h.t.Fatalf("decode %s %s: %v (%s)", method, path, err, data)
		}
	}
	if resp.StatusCode >= 400 {
		h.t.Logf("%s %s → %d: %s", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode
}

func (h *harness) setupWorkspace() {
	h.t.Helper()
	var ws platform.Workspace
	if code := h.do("POST", "/api/v1/admin/workspaces", h.owner,
		map[string]any{"slug": "acme", "name": "Acme"}, &ws); code != http.StatusCreated {
		h.t.Fatalf("create workspace: %d", code)
	}
	h.ws = ws
	var env platform.Environment
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments", h.owner,
		map[string]any{"slug": "prod", "name": "Production"}, &env); code != http.StatusCreated {
		h.t.Fatalf("create env: %d", code)
	}
	h.env = env

	var sdkCT platform.CreatedToken
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/tokens", h.owner,
		map[string]any{"name": "sdk-prod", "kind": "sdk", "environment": "prod"}, &sdkCT); code != http.StatusCreated {
		h.t.Fatalf("create sdk token: %d", code)
	}
	h.sdkTok = sdkCT.Secret
	var colCT platform.CreatedToken
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/tokens", h.owner,
		map[string]any{"name": "collect-prod", "kind": "collect", "environment": "prod"}, &colCT); code != http.StatusCreated {
		h.t.Fatalf("create collect token: %d", code)
	}
	h.colTok = colCT.Secret
	var reviewer platform.CreatedToken
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/tokens", h.owner,
		map[string]any{"name": "reviewer", "kind": "admin", "role": "publisher"}, &reviewer); code != http.StatusCreated {
		h.t.Fatalf("create reviewer token: %d", code)
	}
	h.reviewer = reviewer.Secret
}

func testFlag(key string) *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           key, Type: model.FlagString,
		Fields:    []string{"user_id"},
		Bucketing: &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: key + "-salt"},
		Variations: map[string]any{
			"control": "control-value", "candidate": "candidate-value",
		},
		DefaultRule: &model.Rule{Variation: "control"},
		Targeting: []model.Rule{
			{Name: "ramp", Percentage: map[string]float64{"control": 90, "candidate": 10}},
		},
	}
}

// publishFlag walks the full review pipeline over HTTP and returns the
// publish result.
func (h *harness) publishFlag(t *testing.T, flag *model.Flag, risk platform.Risk, approver string) adminPublishResult {
	t.Helper()
	base := fmt.Sprintf("/api/v1/admin/workspaces/acme/environments/prod/flags/%s", flag.Key)
	if code := h.do("PUT", base+"/draft", h.owner, map[string]any{
		"content": flag, "risk": risk,
	}, nil); code != http.StatusOK {
		t.Fatalf("save draft: %d", code)
	}
	if code := h.do("POST", base+"/submit", h.owner, map[string]any{"reason": "e2e publish"}, nil); code != http.StatusOK {
		t.Fatalf("submit: %d", code)
	}
	if approver == h.owner {
		approver = h.reviewer
	}
	if code := h.do("POST", base+"/approve", approver, map[string]any{"note": "ok"}, nil); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	var res adminPublishResult
	if code := h.do("POST", base+"/publish", h.owner, map[string]any{"note": "ship"}, &res); code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}
	return res
}

func (h *harness) approveOperation(action, object string, payload any) string {
	h.t.Helper()
	base := "/api/v1/admin/workspaces/acme/environments/prod/approvals"
	var a platform.Approval
	if code := h.do("POST", base, h.owner,
		map[string]any{"action": action, "object": object, "payload": payload, "reason": "e2e verification"}, &a); code != http.StatusCreated {
		h.t.Fatalf("request approval: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/approvals/"+a.ID+"/approve",
		h.reviewer, map[string]any{"note": "verified"}, &a); code != http.StatusOK {
		h.t.Fatalf("approve operation: %d", code)
	}
	return a.ID
}

type adminPublishResult struct {
	FlagRevision     int64  `json:"flagRevision"`
	ManifestRevision int64  `json:"manifestRevision"`
	Hash             string `json:"hash"`
	DataID           string `json:"dataId"`
}

func newSDK(t *testing.T, h *harness, mutate func(*sdk.Options)) *sdk.Client {
	t.Helper()
	opts := sdk.Options{
		BaseURL: h.srv.URL, Token: h.sdkTok, CollectToken: h.colTok,
		PollInterval: 200 * time.Millisecond, MaxStaleness: 2 * time.Minute,
		ExposureFlushInterval: 100 * time.Millisecond, ExposureBatchSize: 1000,
	}
	if mutate != nil {
		mutate(&opts)
	}
	c, err := sdk.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("sdk.New: %v", err)
	}
	c.Start(context.Background())
	t.Cleanup(c.Close)
	return c
}

// TestE2EPublishSubscribeEvaluate is the P1 acceptance loop:
// create→review→publish→subscribe→evaluate, with push-based propagation.
func TestE2EPublishSubscribeEvaluate(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()

	client := newSDK(t, h, nil)
	if st := client.Status(); st.FlagCount != 0 || !st.Ready {
		// An empty environment still has a manifest after first publish only;
		// before any publish, /sdk/config returns 404 (no manifest). The SDK
		// must survive that gracefully.
		t.Logf("initial status: %+v", st)
	}

	res := h.publishFlag(t, testFlag("checkout"), platform.RiskNormal, h.owner)
	if res.FlagRevision != 1 || res.ManifestRevision != 1 {
		t.Fatalf("publish result: %+v", res)
	}

	// Push visibility: the SSE event fires inside the publish call, so the
	// client must converge quickly (docs P1 target: 2s; CI-safe bound: 5s).
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		v, r := client.StringVariation("checkout",
			model.EvaluationContext{Attributes: map[string]any{"user_id": "user-42"}}, "?")
		if r.ErrorCode != model.ErrFlagNotFound {
			got = v
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got != "control-value" && got != "candidate-value" {
		t.Fatalf("flag not visible after publish (got %q); status %+v", got, client.Status())
	}
	pushLatency := time.Since(deadline.Add(-5 * time.Second))
	t.Logf("publish→visible latency ≤ %s", pushLatency)

	// Distribution sanity over the synchronized snapshot.
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		unit := fmt.Sprintf("u-%d", i)
		v, r := client.StringVariation("checkout",
			model.EvaluationContext{Attributes: map[string]any{"user_id": unit}}, "?")
		if r.Failed {
			t.Fatalf("eval %d failed: %+v", i, r)
		}
		counts[v]++
	}
	if counts["candidate-value"] < 120 || counts["candidate-value"] > 280 {
		t.Fatalf("10%% split out of range: %v", counts)
	}

	// ETag revalidation: repeated syncs return 304.
	before := client.Status().NotModifiedCount
	for i := 0; i < 3; i++ {
		if err := client.Sync(context.Background()); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	if client.Status().NotModifiedCount <= before {
		t.Fatalf("expected 304 revalidations: %d → %d", before, client.Status().NotModifiedCount)
	}
}

// TestE2EKillSwitchAndRollback covers disable propagation and rollback to a
// verified retained revision.
func TestE2EKillSwitchAndRollback(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()
	h.publishFlag(t, testFlag("ks"), platform.RiskNormal, h.owner)
	client := newSDK(t, h, nil)

	waitVisible := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			v, _ := client.StringVariation("ks",
				model.EvaluationContext{Attributes: map[string]any{"user_id": "u1"}}, "?")
			if v == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("value %q never became visible; status %+v", want, client.Status())
	}
	waitVisible("control-value")

	// Revision 2: kill switch ON → default rule serves control deterministically.
	disabled := testFlag("ks")
	disabled.Disable = true
	disabled.Variations = map[string]any{"control": "KILLED", "candidate": "candidate-value"}
	disabled.DefaultRule = &model.Rule{Variation: "control"}
	h.publishFlag(t, disabled, platform.RiskNormal, h.owner)
	waitVisible("KILLED")

	// Rollback to revision 1 (the split version serving control-value).
	var rb adminPublishResult
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/ks/rollback",
		h.owner, map[string]any{"revision": 1, "reason": "kill switch drill", "emergency": true}, &rb); code != http.StatusOK {
		t.Fatalf("rollback: %d", code)
	}
	if rb.FlagRevision != 1 || rb.ManifestRevision != 3 {
		t.Fatalf("rollback result: %+v", rb)
	}
	waitVisible("control-value")
}

// TestE2EStaleFallback: server gone → last good snapshot serves until
// MaxStaleness, then caller defaults with reason STALE (§5.2 kill-switch
// window).
func TestE2EStaleFallback(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()
	h.publishFlag(t, testFlag("stale"), platform.RiskNormal, h.owner)

	client := newSDK(t, h, func(o *sdk.Options) {
		o.MaxStaleness = 400 * time.Millisecond
		o.PollInterval = 100 * time.Millisecond
	})
	// Wait until synchronized.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if client.Status().FlagCount > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if client.Status().FlagCount == 0 {
		t.Fatal("client never synchronized")
	}

	// Kill the server; evaluations keep serving the last good snapshot…
	h.srv.Close()
	v, r := client.StringVariation("stale",
		model.EvaluationContext{Attributes: map[string]any{"user_id": "u1"}}, "safe")
	if v == "safe" || r.Failed {
		t.Fatalf("must serve last good snapshot briefly: %q %+v", v, r)
	}

	// …and fall back to the caller default once MaxStaleness passes.
	time.Sleep(600 * time.Millisecond)
	v, r = client.StringVariation("stale",
		model.EvaluationContext{Attributes: map[string]any{"user_id": "u1"}}, "safe")
	if v != "safe" {
		t.Fatalf("stale window exceeded but value served: %q", v)
	}
	if r.Reason != model.ReasonStale || !r.Failed {
		t.Fatalf("stale result: %+v", r)
	}
	if st := client.Status(); !st.Stale {
		t.Fatalf("status must report stale: %+v", st)
	}
}

// TestE2EColdStartWithoutServer: a client that never reached the server
// returns caller defaults with an explicit reason (§3).
func TestE2EColdStartWithoutServer(t *testing.T) {
	client, err := sdk.New(context.Background(), sdk.Options{
		BaseURL: "http://127.0.0.1:1", Token: "glab_sdk_invalid",
		PollInterval: time.Hour, MaxStaleness: time.Minute,
	})
	if err == nil {
		defer client.Close()
	}
	// New reports the sync failure but the client remains usable.
	if client == nil {
		t.Fatal("client must exist to serve safe defaults")
	}
	v, r := client.StringVariation("anything", model.EvaluationContext{}, "safe")
	if v != "safe" || !r.Failed {
		t.Fatalf("cold start must serve caller default: %q %+v", v, r)
	}
	if !strings.Contains(r.ErrorDetails, "no snapshot") {
		t.Fatalf("reason must explain cold start: %+v", r)
	}
}

// TestE2EAudienceSeparation: credentials are audience-bound (§9).
func TestE2EAudienceSeparation(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()
	h.publishFlag(t, testFlag("aud"), platform.RiskNormal, h.owner)

	// SDK token on admin API → 403.
	if code := h.do("GET", "/api/v1/admin/workspaces", h.sdkTok, nil, nil); code != http.StatusForbidden {
		t.Fatalf("sdk token on admin API: %d", code)
	}
	// Collect token on SDK config → 403.
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/sdk/config", nil)
	req.Header.Set("Authorization", "Bearer "+h.colTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("collect token on sdk config: %d", resp.StatusCode)
	}
	// Admin token on collect → 403.
	collectBody, _ := json.Marshal(map[string]any{"events": []any{}})
	req2, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/collect", bytes.NewReader(collectBody))
	req2.Header.Set("Authorization", "Bearer "+h.owner)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("admin token on collect: %d", resp2.StatusCode)
	}
	// No token → 401.
	resp3, err := http.Get(h.srv.URL + "/api/v1/sdk/config")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", resp3.StatusCode)
	}
	// SDK config must not accept tenant selection via query parameters.
	req4, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/sdk/config?workspace=other&environment=x", nil)
	req4.Header.Set("Authorization", "Bearer "+h.sdkTok)
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body4, _ := io.ReadAll(resp4.Body)
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("scoped config: %d", resp4.StatusCode)
	}
	var snap model.Snapshot
	if err := json.Unmarshal(body4, &snap); err != nil {
		t.Fatalf("snapshot decode: %v", err)
	}
	if snap.Workspace != h.ws.ID || snap.Environment != h.env.ID {
		t.Fatalf("query params must not change scope: %s/%s", snap.Workspace, snap.Environment)
	}
}

// TestE2EExperimentExposureAndReport runs the P2 loop: experiment
// registration → bind → run → SDK exposures/conversions → report with SRM
// and a decision.
func TestE2EExperimentExposureAndReport(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()

	flag := testFlag("exp-checkout")
	flag.Variations = map[string]any{"control": "c", "treatment": "t"}
	flag.Targeting = []model.Rule{
		{Name: "randomized", Percentage: map[string]float64{"control": 50, "treatment": 50}},
	}
	flag.DefaultRule = &model.Rule{Variation: "control"}
	h.publishFlag(t, flag, platform.RiskNormal, h.owner)

	// Register the experiment via HTTP.
	var exp platform.Experiment
	expReq := map[string]any{
		"flagKey": "exp-checkout", "unit": "user_id",
		"variants":    []string{"control", "treatment"},
		"proportions": map[string]float64{"control": 50, "treatment": 50},
		"primaryMetric": map[string]any{
			"key": "conversion", "type": "proportion", "direction": "increase",
		},
		"alpha": 0.05, "power": 0.8, "mde": 0.02,
		"minSamplePerArm": 20,
		"minRuntime":      0,
		"analysisWindow":  int64(7 * 24 * time.Hour),
		"hypothesis":      "treatment lifts conversion",
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/experiments",
		h.owner, expReq, &exp); code != http.StatusCreated {
		t.Fatalf("create experiment: %d", code)
	}
	if exp.State != model.ExperimentDraft || exp.Epoch != 1 {
		t.Fatalf("experiment: %+v", exp)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/review",
		h.owner, map[string]any{"note": "pre-registered"}, nil); code != http.StatusOK {
		t.Fatalf("review: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/bind",
		h.owner, nil, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	// Bind created a high-risk submitted draft; owner (not submitter when
	// done by a different actor — here owner both binds and approves, which
	// is allowed only because bind used the OWNER actor name and approval
	// compares token names… bind actor == owner == approver → must FAIL).
	code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/exp-checkout/approve",
		h.owner, map[string]any{"note": "self"}, nil)
	if code != http.StatusForbidden {
		t.Fatalf("owner self-approval of high-risk binding must be denied, got %d", code)
	}
	// Create a second publisher token and approve with it.
	var pubCT platform.CreatedToken
	if c := h.do("POST", "/api/v1/admin/workspaces/acme/tokens", h.owner,
		map[string]any{"name": "publisher-b", "kind": "admin", "role": "publisher", "environment": ""},
		&pubCT); c != http.StatusCreated {
		t.Fatalf("create publisher token: %d", c)
	}
	if c := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/exp-checkout/approve",
		pubCT.Secret, map[string]any{"note": "second pair of eyes"}, nil); c != http.StatusOK {
		t.Fatalf("approve by second actor: %d", c)
	}
	if c := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/exp-checkout/publish",
		h.owner, map[string]any{"note": "bind experiment"}, nil); c != http.StatusOK {
		t.Fatalf("publish binding: %d", c)
	}
	if c := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/transition",
		h.owner, map[string]any{"to": "running", "reason": "launch"}, nil); c != http.StatusOK {
		t.Fatalf("start experiment: %d", c)
	}

	client := newSDK(t, h, nil)
	// Wait for the running binding to reach the client.
	deadline := time.Now().Add(5 * time.Second)
	var evalCtx model.EvaluationContext
	for time.Now().Before(deadline) {
		evalCtx = model.EvaluationContext{Attributes: map[string]any{"user_id": "probe-0"}}
		_, r := client.StringVariation("exp-checkout", evalCtx, "?")
		if r.Experiment != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Simulate traffic: treatment converts at 30%, control at 10%.
	converted := map[string]bool{}
	nPerArm := map[string]int{}
	for i := 0; i < 400; i++ {
		unit := fmt.Sprintf("user-%d", i)
		ctx := model.EvaluationContext{Attributes: map[string]any{"user_id": unit}}
		v, r := client.StringVariation("exp-checkout", ctx, "?")
		if r.Experiment == nil {
			continue // not enrolled (shouldn't happen at 100% eligibility)
		}
		nPerArm[r.Experiment.Variant]++
		p := 0.1
		if v == "t" {
			p = 0.3
		}
		// Deterministic pseudo-conversion by unit index to avoid RNG flakes:
		// every k-th unit converts according to the arm rate.
		if (v == "t" && i%10 < 3) || (v == "c" && i%10 == 0) {
			_ = p
			if !converted[unit] {
				converted[unit] = true
				if err := sdk.TrackConversion(client, ctx, r, "conversion", 1, "sess-"+unit, ""); err != nil {
					t.Fatalf("TrackConversion: %v", err)
				}
			}
		}
	}
	if nPerArm["control"]+nPerArm["treatment"] < 300 {
		t.Fatalf("too few enrollments: %v", nPerArm)
	}
	t.Logf("enrolled: %v, converted units: %d", nPerArm, len(converted))

	// Closing the client flushes exposures and conversions.
	client.Close()

	// The report must be computable and healthy.
	var rep map[string]any
	if code := h.do("GET", "/api/v1/admin/experiments/"+exp.ID+"/report", h.owner, nil, &rep); code != http.StatusOK {
		t.Fatalf("report: %d", code)
	}
	if rep["healthy"] != true {
		t.Fatalf("report unhealthy: %v (issues %v, primaryError %v)",
			rep["healthy"], rep["healthIssues"], rep["primaryError"])
	}
	srm, _ := rep["srm"].(map[string]any)
	if srm == nil || srm["healthy"] != true {
		t.Fatalf("SRM must be healthy: %v", srm)
	}
	decision, _ := rep["decision"].(map[string]any)
	if decision == nil {
		t.Fatalf("decision missing: %v", rep)
	}
	verdict, _ := decision["Verdict"].(string)
	if verdict == "" {
		// stats.Decision fields may serialize without json tags.
		b, _ := json.Marshal(decision)
		t.Logf("decision payload: %s", b)
	}
	primary, _ := rep["primary"].(map[string]any)
	if primary == nil || primary["significant"] != true {
		t.Fatalf("30%% vs 10%% lift on ~400 units must be significant: %v", primary)
	}
}

// TestE2ETokenRevokeScoping: workspace owners revoke only their own tokens;
// global credentials require the global owner.
func TestE2ETokenRevokeScoping(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()

	// Second workspace with its own owner token.
	var ws2 platform.Workspace
	if code := h.do("POST", "/api/v1/admin/workspaces", h.owner,
		map[string]any{"slug": "globex"}, &ws2); code != http.StatusCreated {
		t.Fatalf("ws2: %d", code)
	}
	var owner2 platform.CreatedToken
	if code := h.do("POST", "/api/v1/admin/workspaces/globex/tokens", h.owner,
		map[string]any{"name": "globex-owner", "kind": "admin", "role": "owner"}, &owner2); code != http.StatusCreated {
		t.Fatalf("owner2: %d", code)
	}
	// A viewer token in acme to act as the revocation target.
	var viewer platform.CreatedToken
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/tokens", h.owner,
		map[string]any{"name": "acme-viewer", "kind": "admin", "role": "viewer"}, &viewer); code != http.StatusCreated {
		t.Fatalf("viewer: %d", code)
	}

	// Globex owner must NOT revoke acme's token.
	if code := h.do("DELETE", "/api/v1/admin/tokens/"+viewer.Token.ID, owner2.Secret, nil, nil); code != http.StatusForbidden {
		t.Fatalf("cross-workspace revoke: %d, want 403", code)
	}
	// Globex owner must NOT revoke the global bootstrap token either.
	globalID := ""
	for _, tk := range h.store.ListTokens("") {
		if tk.Role == platform.RoleOwner && tk.WorkspaceID == "" {
			globalID = tk.ID
		}
	}
	if globalID == "" {
		t.Fatal("global owner token not found")
	}
	if code := h.do("DELETE", "/api/v1/admin/tokens/"+globalID, owner2.Secret, nil, nil); code != http.StatusForbidden {
		t.Fatalf("scoped owner revoking global token: %d, want 403", code)
	}
	// The global owner CAN revoke the acme viewer token.
	if code := h.do("DELETE", "/api/v1/admin/tokens/"+viewer.Token.ID, h.owner, nil, nil); code != http.StatusOK {
		t.Fatalf("global revoke: %d", code)
	}
	// Revoked credential is dead immediately.
	if code := h.do("GET", "/api/v1/admin/workspaces", viewer.Secret, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked token still authenticates: %d", code)
	}
}

// TestE2ERemoteEval covers /sdk/eval for thin clients, including
// server-side exposure capture.
func TestE2ERemoteEval(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()
	h.publishFlag(t, testFlag("remote"), platform.RiskNormal, h.owner)

	body, _ := json.Marshal(map[string]any{
		"flagKey": "remote",
		"context": map[string]any{
			"targetingKey": "u1",
			"attributes":   map[string]any{"user_id": "u1"},
		},
	})
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/sdk/eval", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.sdkTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("eval status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Result model.Result `json:"result"`
		Value  string       `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Value != "control-value" && out.Value != "candidate-value" {
		t.Fatalf("remote eval value: %q (%+v)", out.Value, out.Result)
	}
	if out.Result.Reason != model.ReasonSplit {
		t.Fatalf("remote eval reason: %s", out.Result.Reason)
	}
}

// TestE2EDebugEvalNoExposure verifies the console debugger never records
// exposures (§11).
func TestE2EDebugEvalNoExposure(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()
	h.publishFlag(t, testFlag("dbg"), platform.RiskNormal, h.owner)

	before := h.store.EventCount()
	var out map[string]any
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/debug/eval",
		h.owner, map[string]any{
			"flagKey": "dbg",
			"context": map[string]any{"attributes": map[string]any{"user_id": "u1"}},
		}, &out); code != http.StatusOK {
		t.Fatalf("debug eval: %d", code)
	}
	result, _ := out["result"].(map[string]any)
	if result == nil {
		t.Fatalf("no result: %v", out)
	}
	if result["trackEvents"] == true || result["experiment"] != nil {
		t.Fatalf("debug eval must strip exposure: %v", result)
	}
	if h.store.EventCount() != before {
		t.Fatal("debug eval recorded events")
	}
}

// TestE2ELocalRemoteParity proves the cross-language contract on the server
// side: the Go SDK's LOCAL evaluation and the remote /sdk/eval path (the one
// the Python/TypeScript SDKs use) produce identical values, variants and
// reasons for identical contexts — P4 退出验收「跨语言用例同值」. Combined
// with the golden-vector tests in sdk/python and sdk/typescript, all three
// languages agree on the same assignments.
func TestE2ELocalRemoteParity(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()
	flag := testFlag("parity-flag")
	h.publishFlag(t, flag, platform.RiskNormal, h.owner)

	client := newSDK(t, h, nil)
	// Wait for the first snapshot to sync.
	probe := model.EvaluationContext{TargetingKey: "probe", Attributes: map[string]any{"user_id": "probe"}}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, r := client.StringVariation("parity-flag", probe, "?"); !r.Failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client never synced")
		}
		time.Sleep(50 * time.Millisecond)
	}

	var candidates, controls int
	for i := 0; i < 60; i++ {
		unit := fmt.Sprintf("parity-user-%d", i)
		evalCtx := model.EvaluationContext{
			TargetingKey: unit,
			Attributes:   map[string]any{"user_id": unit},
		}
		localVal, localRes := client.StringVariation("parity-flag", evalCtx, "?")
		if localRes.Failed {
			t.Fatalf("local eval failed for %s: %+v", unit, localRes)
		}

		var remote struct {
			Result model.Result `json:"result"`
			Value  string       `json:"value"`
		}
		if code := h.do("POST", "/api/v1/sdk/eval", h.sdkTok, map[string]any{
			"flagKey": "parity-flag", "context": evalCtx,
		}, &remote); code != http.StatusOK {
			t.Fatalf("remote eval %s: %d", unit, code)
		}
		if remote.Value != localVal {
			t.Fatalf("unit %s: local=%q remote=%q", unit, localVal, remote.Value)
		}
		if remote.Result.Reason != localRes.Reason {
			t.Fatalf("unit %s: local reason=%s remote reason=%s", unit, localRes.Reason, remote.Result.Reason)
		}
		if remote.Result.Bucket != localRes.Bucket {
			t.Fatalf("unit %s: local bucket=%d remote bucket=%d", unit, localRes.Bucket, remote.Result.Bucket)
		}
		if (localRes.Experiment == nil) != (remote.Result.Experiment == nil) {
			t.Fatalf("unit %s: experiment attribution mismatch", unit)
		}
		if localRes.Experiment != nil &&
			*localRes.Experiment != *remote.Result.Experiment {
			t.Fatalf("unit %s: local exp=%+v remote exp=%+v", unit,
				*localRes.Experiment, *remote.Result.Experiment)
		}
		switch localVal {
		case "candidate-value":
			candidates++
		case "control-value":
			controls++
		default:
			t.Fatalf("unit %s: unexpected value %q", unit, localVal)
		}
	}
	// The 90/10 ramp must produce BOTH arm values across 60 units —
	// otherwise the parity assertions above would be vacuous.
	if candidates == 0 || controls == 0 {
		t.Fatalf("test coverage degenerate: candidates=%d controls=%d", candidates, controls)
	}
	t.Logf("parity over 60 units: control=%d candidate=%d", controls, candidates)
}
