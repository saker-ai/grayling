// PostgreSQL-backed full-stack acceptance: the same control-plane and data-plane
// loop as TestE2EExperimentExposureAndReport, but the platform store is the
// production PostgreSQL backend (real server via test/pgtest), proving the
// admin pipeline, remote evaluation, collection and report building all run on
// the relational store (docs/技术方案.md §10.2).
package e2e

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/godeps/golab/internal/api"
	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/pkg/model"
	"github.com/godeps/golab/pkg/sdk"
	"github.com/godeps/golab/test/pgtest"
	gconfig "github.com/godeps/gonacos/pkg/config"
)

func newPGHarness(t *testing.T) (string, *harness) {
	t.Helper()
	dsn := pgtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := platform.NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := configstore.NewGonacosStore(gconfig.NewService())
	apiSrv := api.NewServer(store, cfg, api.Options{})
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)

	h := &harness{t: t, store: store, api: apiSrv, srv: srv}
	ct, err := store.CreateToken(platform.TokenAdmin, "bootstrap-owner", platform.RoleOwner, "", "", "bootstrap")
	if err != nil {
		t.Fatalf("owner token: %v", err)
	}
	h.owner = ct.Secret
	return dsn, h
}

func TestE2EPostgresExperimentLoop(t *testing.T) {
	dsn, h := newPGHarness(t)
	h.setupWorkspace()

	flag := testFlag("pg-checkout")
	flag.Variations = map[string]any{"control": "c", "treatment": "t"}
	flag.Targeting = []model.Rule{
		{Name: "randomized", Percentage: map[string]float64{"control": 50, "treatment": 50}},
	}
	flag.DefaultRule = &model.Rule{Variation: "control"}
	h.publishFlag(t, flag, platform.RiskNormal, h.owner)

	var exp platform.Experiment
	expReq := map[string]any{
		"flagKey": "pg-checkout", "unit": "user_id",
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
		h.owner, expReq, &exp); code != 201 {
		t.Fatalf("create experiment: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/review",
		h.owner, map[string]any{"note": "pre-registered"}, nil); code != 200 {
		t.Fatalf("review: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/bind",
		h.owner, nil, nil); code != 200 {
		t.Fatalf("bind: %d", code)
	}
	// Binding is high-risk: approve with a second actor (two-person rule).
	var pubCT platform.CreatedToken
	if c := h.do("POST", "/api/v1/admin/workspaces/acme/tokens", h.owner,
		map[string]any{"name": "publisher-b", "kind": "admin", "role": "publisher"},
		&pubCT); c != 201 {
		t.Fatalf("create publisher token: %d", c)
	}
	if c := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/pg-checkout/approve",
		pubCT.Secret, map[string]any{"note": "second pair of eyes"}, nil); c != 200 {
		t.Fatalf("approve: %d", c)
	}
	if c := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/pg-checkout/publish",
		h.owner, map[string]any{"note": "bind experiment"}, nil); c != 200 {
		t.Fatalf("publish binding: %d", c)
	}
	if c := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/transition",
		h.owner, map[string]any{"to": "running", "reason": "launch"}, nil); c != 200 {
		t.Fatalf("start experiment: %d", c)
	}

	client := newSDK(t, h, nil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, r := client.StringVariation("pg-checkout",
			model.EvaluationContext{Attributes: map[string]any{"user_id": "probe-0"}}, "?")
		if r.Experiment != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	nPerArm := map[string]int{}
	for i := 0; i < 400; i++ {
		unit := fmt.Sprintf("user-%d", i)
		ctx := model.EvaluationContext{Attributes: map[string]any{"user_id": unit}}
		v, r := client.StringVariation("pg-checkout", ctx, "?")
		if r.Experiment == nil {
			continue
		}
		nPerArm[r.Experiment.Variant]++
		if (v == "t" && i%10 < 3) || (v == "c" && i%10 == 0) {
			if err := sdk.TrackConversion(client, ctx, r, "conversion", 1, "sess-"+unit, ""); err != nil {
				t.Fatalf("TrackConversion: %v", err)
			}
		}
	}
	if nPerArm["control"]+nPerArm["treatment"] < 300 {
		t.Fatalf("too few enrollments: %v", nPerArm)
	}
	client.Close()

	var rep map[string]any
	if code := h.do("GET", "/api/v1/admin/experiments/"+exp.ID+"/report", h.owner, nil, &rep); code != 200 {
		t.Fatalf("report: %d", code)
	}
	if rep["healthy"] != true {
		t.Fatalf("report unhealthy: %v (issues %v)", rep["healthy"], rep["healthIssues"])
	}
	srm, _ := rep["srm"].(map[string]any)
	if srm == nil || srm["healthy"] != true {
		t.Fatalf("SRM must be healthy: %v", srm)
	}
	primary, _ := rep["primary"].(map[string]any)
	if primary == nil || primary["significant"] != true {
		t.Fatalf("30%% vs 10%% lift on ~400 units must be significant: %v", primary)
	}

	// The durable side must hold the analysis state: reopen the store on the
	// SAME database, rebuild the API on it, and produce an identical report
	// from PostgreSQL alone.
	if err := h.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reloaded, err := platform.NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen on same database: %v", err)
	}
	defer reloaded.Close()
	apiSrv2 := api.NewServer(reloaded, h.api.Config, api.Options{})
	h.srv.Close()
	h.srv = httptest.NewServer(apiSrv2.Handler())
	t.Cleanup(h.srv.Close)
	h.api = apiSrv2
	h.store = reloaded

	var rep2 map[string]any
	if code := h.do("GET", "/api/v1/admin/experiments/"+exp.ID+"/report", h.owner, nil, &rep2); code != 200 {
		t.Fatalf("report after reload: %d", code)
	}
	if rep2["healthy"] != true {
		t.Fatalf("report after reload unhealthy: %v (issues %v)", rep2["healthy"], rep2["healthIssues"])
	}
	primary2, _ := rep2["primary"].(map[string]any)
	if primary2 == nil || primary2["significant"] != true {
		t.Fatalf("significance must survive reload: %v", primary2)
	}
}
