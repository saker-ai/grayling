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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	gconfig "github.com/godeps/gonacos/pkg/config"
	"github.com/saker-ai/grayling/internal/api"
	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/experiment"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/internal/recovery"
	"github.com/saker-ai/grayling/pkg/model"
	"github.com/saker-ai/grayling/pkg/sdk"
	"github.com/saker-ai/grayling/test/pgtest"
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
	b, err := platform.NewGORMPostgresStore(t.Context(), dsn)
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
	if rows := pgtest.Query(t, dsn, `SELECT count(*) FROM database_events`); rows[0][0] != "1" {
		t.Fatalf("database accepted=%v", rows)
	}
}

func newPGHarness(t *testing.T) (string, *harness) {
	t.Helper()
	dsn := pgtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := platform.NewGORMPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rawCfg := gconfig.NewService()
	cfg := configstore.NewGonacosStore(rawCfg)
	apiSrv := api.NewServer(store, cfg, api.Options{})
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)

	h := &harness{t: t, store: store, api: apiSrv, rawCfg: rawCfg, srv: srv}
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
	// Binding is high-risk; a second publisher approves it, which exercises
	// a cross-actor grant against the Postgres backend.
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
	wantValue, wantEval := client.StringVariation("pg-checkout",
		model.EvaluationContext{Attributes: map[string]any{"user_id": "recovery-probe"}}, "?")
	if wantEval.Failed || wantEval.Experiment == nil {
		t.Fatalf("pre-recovery SDK evaluation: %+v", wantEval)
	}
	client.Close()
	duplicate := map[string]any{"events": []any{map[string]any{
		"eventId": "recovery-dedup-probe", "type": "exposure", "experimentId": exp.ID,
		"epoch": 1, "variant": "control", "unit": "recovery-unit", "source": "split", "clientTime": time.Now().UTC(),
	}}}
	var firstIngest map[string]any
	if code := h.do("POST", "/api/v1/collect", h.colTok, duplicate, &firstIngest); code != 200 || firstIngest["accepted"] != float64(1) {
		t.Fatalf("pre-recovery dedup fixture: %d %+v", code, firstIngest)
	}

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

	// Back up the quiesced PostgreSQL database and gonacos Config snapshot,
	// then restore into an isolated fresh database and service instance.
	point, err := recovery.Capture(t.Context(), dsn, "recovery-test-auth", h.store, h.api.Config)
	if err != nil {
		t.Fatalf("capture recovery point: %v", err)
	}
	configSnapshot, err := h.rawCfg.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	point.GonacosSHA256, err = recovery.DigestSnapshot(map[string]any{"config": configSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	pointPath := filepath.Join(t.TempDir(), "point.json")
	if err := recovery.Write(pointPath, point); err != nil {
		t.Fatal(err)
	}
	configJSON, err := json.Marshal(configSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	targetDSN := pgtest.DSN(t)
	dumpRestorePostgres(t, dsn, targetDSN)
	var decoded map[string]any
	if err := json.Unmarshal(configJSON, &decoded); err != nil {
		t.Fatal(err)
	}
	restoredConfig := gconfig.NewService()
	if err := restoredConfig.Restore(decoded); err != nil {
		t.Fatal(err)
	}
	restoredCfg := configstore.NewGonacosStore(restoredConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reloaded, err := platform.NewGORMPostgresStore(ctx, targetDSN)
	if err != nil {
		t.Fatalf("reopen on same database: %v", err)
	}
	defer reloaded.Close()
	actual, err := recovery.Capture(t.Context(), targetDSN, "recovery-test-auth", reloaded, restoredCfg)
	if err != nil {
		t.Fatalf("capture restored state: %v", err)
	}
	restoredSnapshot, err := restoredConfig.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	actual.GonacosSHA256, err = recovery.DigestSnapshot(map[string]any{"config": restoredSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Verify(pointPath, actual); err != nil {
		t.Fatalf("restored recovery point: %v\nbefore=%+v\nafter=%+v", err, point, actual)
	}
	if _, err := recovery.Capture(t.Context(), targetDSN, "recovery-test-auth", reloaded,
		configstore.NewGonacosStore(gconfig.NewService())); err == nil {
		t.Fatal("single-component gonacos rollback passed recovery capture")
	}
	wrongKey := *actual
	wrongKey.AuthSHA256 = "wrong"
	if err := recovery.Verify(pointPath, &wrongKey); err == nil {
		t.Fatal("wrong auth secret passed recovery gate")
	}
	apiSrv2 := api.NewServer(reloaded, restoredCfg, api.Options{})
	h.srv.Close()
	h.srv = httptest.NewServer(apiSrv2.Handler())
	t.Cleanup(h.srv.Close)
	h.api = apiSrv2
	h.store = reloaded
	h.rawCfg = restoredConfig
	// This probe checks restored evaluation only. Collecting a fresh onDecision
	// exposure would mutate the recovered fixture during the replay comparison.
	eventsBeforeProbe := pgtest.Query(t, targetDSN, `SELECT to_jsonb(e)::text FROM database_events e ORDER BY to_jsonb(e)::text`)
	restoredClient := newSDK(t, h, func(opts *sdk.Options) {
		opts.CollectToken = ""
	})
	gotValue, gotEval := restoredClient.StringVariation("pg-checkout",
		model.EvaluationContext{Attributes: map[string]any{"user_id": "recovery-probe"}}, "?")
	restoredClient.Close()
	if gotEval.Failed || gotEval.Experiment == nil || gotValue != wantValue || gotEval.Experiment.Variant != wantEval.Experiment.Variant {
		t.Fatalf("SDK evaluation drift: before=%s %+v after=%s %+v", wantValue, wantEval, gotValue, gotEval)
	}
	if eventsAfterProbe := pgtest.Query(t, targetDSN, `SELECT to_jsonb(e)::text FROM database_events e ORDER BY to_jsonb(e)::text`); !reflect.DeepEqual(eventsBeforeProbe, eventsAfterProbe) {
		t.Fatal("restored evaluation probe changed stored events")
	}
	var secondIngest map[string]any
	if code := h.do("POST", "/api/v1/collect", h.colTok, duplicate, &secondIngest); code != 200 || secondIngest["duplicates"] != float64(1) {
		t.Fatalf("restored event dedup: %d %+v", code, secondIngest)
	}

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

func dumpRestorePostgres(t *testing.T, source, target string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "postgres.dump")
	var dump, restore *exec.Cmd
	if _, err := exec.LookPath("pg_dump"); err == nil {
		if _, err := exec.LookPath("pg_restore"); err == nil {
			dump = exec.Command("pg_dump", "-Fc", "-f", path, source)
			restore = exec.Command("pg_restore", "--exit-on-error", "--no-owner", "--no-acl", "-d", target, path)
		}
	}
	if dump == nil {
		if _, err := exec.LookPath("docker"); err != nil {
			t.Skip("recovery drill needs pg_dump/pg_restore or Docker")
		}
		mount := filepath.Dir(path) + ":/backup"
		dump = exec.Command("docker", "run", "--rm", "--network", "host", "-v", mount,
			"postgres:16-alpine", "pg_dump", "-Fc", "-f", "/backup/postgres.dump", source)
		restore = exec.Command("docker", "run", "--rm", "--network", "host", "-v", mount,
			"postgres:16-alpine", "pg_restore", "--exit-on-error", "--no-owner", "--no-acl",
			"-d", target, "/backup/postgres.dump")
	}
	for _, cmd := range []*exec.Cmd{dump, restore} {
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", cmd.Path, err, out)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("empty postgres dump: %v", err)
	}
}
