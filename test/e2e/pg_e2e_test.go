// PostgreSQL-backed full-stack acceptance: the same control-plane and data-plane
// loop as TestE2EExperimentExposureAndReport, but the platform store is the
// production PostgreSQL backend (real server via test/pgtest), proving the
// admin pipeline, remote evaluation, collection and report building all run on
// the relational store (docs/技术方案.md §10.2).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/godeps/golab/internal/api"
	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/internal/experiment"
	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/pkg/model"
	"github.com/godeps/golab/pkg/sdk"
	"github.com/godeps/golab/test/pgtest"
	gconfig "github.com/godeps/gonacos/pkg/config"
)

func TestE2EPostgresConcurrentCollectCounts(t *testing.T) {
	dsn, h := newPGHarness(t)
	h.setupWorkspace()
	exp, err := h.store.CreateExperiment(&platform.Experiment{
		WorkspaceID: h.ws.ID, EnvironmentID: h.env.ID, FlagKey: "f", Epoch: 1,
		Unit: "user_id", Variants: []string{"control", "treatment"},
		Proportions:   map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: platform.MetricSpec{Key: "m", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	other := httptest.NewServer(api.NewServer(b, h.api.Config, api.Options{}).Handler())
	defer other.Close()
	body, err := json.Marshal(map[string]any{"events": []any{map[string]any{
		"eventId": "same-http-event", "type": "exposure", "experimentId": exp.ID,
		"epoch": 1, "variant": "control", "unit": "u1", "source": "split", "clientTime": time.Now().UTC(),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan map[string]any, 2)
	var wg sync.WaitGroup
	for _, endpoint := range []string{h.srv.URL, other.URL} {
		wg.Go(func() {
			<-start
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint+"/api/v1/collect", bytes.NewReader(body))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+h.colTok)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("collect status: %d", resp.StatusCode)
				return
			}
			var out map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Error(err)
				return
			}
			results <- out
		})
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, duplicates := 0.0, 0.0
	for result := range results {
		accepted += result["accepted"].(float64)
		duplicates += result["duplicates"].(float64)
	}
	if accepted != 1 || duplicates != 1 {
		t.Fatalf("API counts: accepted=%v duplicates=%v", accepted, duplicates)
	}
	if rows := pgtest.Query(t, dsn, `SELECT count(*) FROM events`); rows[0][0] != "1" {
		t.Fatalf("database accepted=%v", rows)
	}
}

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
	if rep["healthy"] != false {
		t.Fatalf("new window must await late-event watermark: %v", rep)
	}
	window, _ := rep["window"].(map[string]any)
	if window == nil || window["complete"] != false {
		t.Fatalf("incomplete window missing: %v", rep)
	}
	registered, err := h.store.GetExperiment(exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	completeAt := registered.StartedAt.Add(registered.AnalysisWindow + 72*time.Hour)
	completed, err := experiment.BuildReport(h.store, exp.ID, completeAt)
	if err != nil || !completed.Window.Complete || !completed.Healthy {
		t.Fatalf("completed report: %+v %v", completed, err)
	}
	splitCounts := h.store.SplitExposedUnitCounts(h.ws.ID, h.env.ID, exp.ID, exp.Epoch)
	metricAgg := h.store.MetricAggregate(h.ws.ID, h.env.ID, exp.ID, exp.Epoch, "conversion")
	for _, arm := range completed.PerVariant {
		converted := int64(0)
		if metricAgg[arm.Variant] != nil {
			converted = int64(len(metricAgg[arm.Variant].Units))
		}
		if arm.RandomizedUnits != splitCounts[arm.Variant] || arm.ConvertedUnits != converted {
			t.Fatalf("raw recomputation differs from incremental arm %s: report=%+v split=%d metric=%+v", arm.Variant, arm, splitCounts[arm.Variant], metricAgg[arm.Variant])
		}
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
	if rep2["healthy"] != false {
		t.Fatalf("reloaded live report must still await watermark: %v", rep2)
	}
	completed2, err := experiment.BuildReport(reloaded, exp.ID, completeAt)
	if err != nil || !completed2.Window.Complete || !completed2.Healthy {
		t.Fatalf("completed report after reload: %+v %v", completed2, err)
	}
	if !reflect.DeepEqual(completed.SRM, completed2.SRM) || !reflect.DeepEqual(completed.PerVariant, completed2.PerVariant) ||
		!reflect.DeepEqual(completed.Decision, completed2.Decision) {
		t.Fatalf("SRM, arms or decision changed on raw replay: before=%+v after=%+v", completed, completed2)
	}
	primary2, _ := rep2["primary"].(map[string]any)
	if primary2 == nil || primary2["significant"] != true {
		t.Fatalf("significance must survive reload: %v", primary2)
	}
}
