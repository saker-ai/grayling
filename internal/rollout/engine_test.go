// Package rollout_test verifies the staged-release auto-advance engine
// (docs/技术方案.md §5.3 P4) end to end inside a real server: pre-registered
// plan steps, deterministic clock advancement, decision-gate-driven advance /
// pause / rollback, idempotent actions, and the manual takeover switch —
// plus the admin API surface that operates plans.
package rollout_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gconfig "github.com/godeps/gonacos/pkg/config"
	"github.com/saker-ai/grayling/internal/admin"
	"github.com/saker-ai/grayling/internal/api"
	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/internal/rollout"
	"github.com/saker-ai/grayling/pkg/bucketing"
	"github.com/saker-ai/grayling/pkg/evaluation"
	"github.com/saker-ai/grayling/pkg/model"
	"github.com/saker-ai/grayling/test/pgtest"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	t         *testing.T
	store     *platform.Store
	api       *api.Server
	srv       *httptest.Server
	ws        platform.Workspace
	env       platform.Environment
	owner     string
	publisher string // second actor for high-risk approvals (二人规则)
	sdkTok    string
	colTok    string

	fakeNow  time.Time
	eventSeq int
}

func TestMultiReplicaRunFailover(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

	makeEngine := func(store *platform.Store) *rollout.Engine {
		e := rollout.NewEngine(store, admin.NewService(store, nil, nil), nil)
		e.LeaseDuration = 180 * time.Millisecond
		return e
	}
	var runs sync.WaitGroup
	start := func(ctx context.Context, store *platform.Store) {
		runs.Go(func() { makeEngine(store).Run(ctx, 40*time.Millisecond) })
	}
	ctxA, cancelA := context.WithCancel(t.Context())
	defer cancelA()
	start(ctxA, a)
	waitHolder := func(wantDifferent string) platform.RolloutLeadership {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			status, err := a.RolloutLeadership(t.Context())
			if err == nil && status.Holder != "" && status.Holder != wantDifferent && status.LastRunAt != nil {
				return status
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("rollout holder did not change")
		return platform.RolloutLeadership{}
	}
	first := waitHolder("")
	ctxB, cancelB := context.WithCancel(t.Context())
	defer cancelB()
	start(ctxB, b)
	time.Sleep(80 * time.Millisecond)
	before, err := b.RolloutLeadership(t.Context())
	if err != nil || before.Holder != first.Holder || before.Generation != first.Generation {
		t.Fatalf("dual replica changed live holder: %+v, %v", before, err)
	}
	cancelA()
	second := waitHolder(first.Holder)
	if second.Generation <= first.Generation || !second.LastTakeoverAt.After(*first.LastTakeoverAt) {
		t.Fatalf("takeover not recorded: first=%+v second=%+v", first, second)
	}
	cancelB()
	ctxRestart, stopRestart := context.WithCancel(t.Context())
	defer func() { cancelA(); cancelB(); stopRestart(); runs.Wait() }()
	start(ctxRestart, a)
	third := waitHolder(second.Holder)
	if third.Generation <= second.Generation {
		t.Fatalf("restart did not obtain new generation: %+v", third)
	}
}

func TestMultiReplicaPlanStartTakeoverAndManualAdvance(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	h := newHarnessOnStore(t, a)
	expID := h.runningExperiment(t, "cluster-rollout", nil)
	h.createPlan(t, expID,
		step{Name: "canary", Ratio: 20, MinRuntime: int64(time.Hour), AutoAdvance: true},
		step{Name: "half", Ratio: 50, MinRuntime: int64(time.Hour), AutoAdvance: true})
	b, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	second := rollout.NewEngine(b, admin.NewService(b, h.api.Config, nil), nil)
	for _, engine := range []*rollout.Engine{h.api.Rollout, second} {
		engine.LeaseDuration = 250 * time.Millisecond
	}
	ctxA, cancelA := context.WithCancel(t.Context())
	ctxB, cancelB := context.WithCancel(t.Context())
	var runs sync.WaitGroup
	runs.Go(func() { h.api.Rollout.Run(ctxA, 40*time.Millisecond) })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := a.ReloadFromBackend(); err != nil {
			t.Fatal(err)
		}
		plan := h.getPlan(t, expID)
		if plan.State == platform.ReleasePlanRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	first, err := a.RolloutLeadership(t.Context())
	if err != nil || first.Holder == "" {
		t.Fatalf("initial leader: %+v %v", first, err)
	}
	var observed platform.RolloutLeadership
	if code := h.do("GET", "/api/v1/admin/rollout/leadership", h.owner, nil, &observed); code != http.StatusOK || observed.Generation != first.Generation {
		t.Fatalf("leadership API: %d %+v", code, observed)
	}
	runs.Go(func() { second.Run(ctxB, 40*time.Millisecond) })
	time.Sleep(100 * time.Millisecond)
	plan := h.getPlan(t, expID)
	starts := 0
	for _, event := range plan.History {
		if event.Action == "plan.start" {
			starts++
		}
	}
	if starts != 1 || plan.CurrentStep != 0 || h.publishedEligibility(t, "cluster-rollout") != 20 {
		t.Fatalf("duplicate or missing start: %+v", plan)
	}
	cancelA()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, err := b.RolloutLeadership(t.Context())
		if err == nil && status.Holder != "" && status.Holder != first.Holder {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	status, err := b.RolloutLeadership(t.Context())
	if err != nil || status.Holder == first.Holder || status.Holder == "" {
		t.Fatalf("no takeover: %+v %v", status, err)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/takeover",
		h.owner, map[string]any{"enabled": true, "reason": "incident freeze"}, nil); code != http.StatusOK {
		t.Fatalf("takeover: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/advance",
		h.owner, map[string]any{"reason": "operator decided"}, nil); code != http.StatusOK {
		t.Fatalf("manual advance: %d", code)
	}
	time.Sleep(100 * time.Millisecond)
	if err := a.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	plan = h.getPlan(t, expID)
	starts, manual := 0, 0
	for _, event := range plan.History {
		if event.Action == "plan.start" {
			starts++
		}
		if event.Action == "manual.advance" {
			manual++
		}
	}
	if starts != 1 || manual != 1 || plan.CurrentStep != 1 || !plan.ManualOverride {
		t.Fatalf("switch duplicated plan history: %+v", plan)
	}
	for _, audit := range a.AuditList(h.ws.ID, 100) {
		if audit.Action == "release_plan.manual_advance" && audit.Object == expID {
			manual--
		}
	}
	if manual != 0 {
		t.Fatalf("manual advance audit count mismatch: %d", manual)
	}
	cancelB()
	runs.Wait()
}

func TestMultiReplicaDecisionGateAfterFailover(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	h := newHarnessOnStore(t, a)
	expID := h.runningExperiment(t, "gate-failover", nil)
	h.createPlan(t, expID,
		step{Name: "canary", Ratio: 20, MinRuntime: int64(time.Hour), AutoAdvance: true},
		step{Name: "half", Ratio: 50, MinRuntime: int64(time.Hour), AutoAdvance: true})
	b, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	second := rollout.NewEngine(b, admin.NewService(b, h.api.Config, nil), nil)
	var clock atomic.Int64
	clock.Store(h.fakeNow.UnixNano())
	for _, engine := range []*rollout.Engine{h.api.Rollout, second} {
		engine.LeaseDuration = time.Second
		engine.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	}
	ctxA, cancelA := context.WithCancel(t.Context())
	ctxB, cancelB := context.WithCancel(t.Context())
	var runs sync.WaitGroup
	defer func() { cancelA(); cancelB(); runs.Wait() }()
	runs.Go(func() { h.api.Rollout.Run(ctxA, 40*time.Millisecond) })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := a.ReloadFromBackend(); err != nil {
			t.Fatal(err)
		}
		if h.getPlan(t, expID).State == platform.ReleasePlanRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h.getPlan(t, expID).State != platform.ReleasePlanRunning {
		t.Fatal("step 0 did not start")
	}
	if n := h.feedTraffic(t, expID, 0, 600, 0.3, 0.1, nil); n < 60 {
		t.Fatalf("enrollments: %d", n)
	}
	cancelA()
	clock.Store(h.fakeNow.Add(11 * 24 * time.Hour).UnixNano())
	runs.Go(func() { second.Run(ctxB, 40*time.Millisecond) })
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := a.ReloadFromBackend(); err != nil {
			t.Fatal(err)
		}
		if h.getPlan(t, expID).CurrentStep == 1 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	plan := h.getPlan(t, expID)
	if plan.CurrentStep != 1 {
		t.Fatalf("new leader did not advance healthy gate: %+v", plan)
	}
	for time.Now().Before(deadline) {
		if err := a.ReloadFromBackend(); err != nil {
			t.Fatal(err)
		}
		if h.publishedEligibility(t, "gate-failover") == 50 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	starts, advances := 0, 0
	for _, event := range plan.History {
		if event.Action == "plan.start" {
			starts++
		}
		if event.Action == "step.advance" {
			advances++
		}
	}
	if starts != 1 || advances != 1 || h.publishedEligibility(t, "gate-failover") != 50 {
		t.Fatalf("duplicate/missing step after failover: starts=%d advances=%d plan=%+v", starts, advances, plan)
	}
}

func TestMultiReplicaBlockedGatesAfterFailover(t *testing.T) {
	for _, scenario := range []string{"srm", "guardrail"} {
		t.Run(scenario, func(t *testing.T) {
			dsn := pgtest.DSN(t)
			a, err := platform.NewPGStore(t.Context(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = a.Close() })
			h := newHarnessOnStore(t, a)
			var mutate func(map[string]any)
			if scenario == "guardrail" {
				mutate = func(m map[string]any) {
					m["guardrails"] = []map[string]any{{"metricKey": "latency", "type": "mean", "direction": "decrease", "maxRelativeRegression": 0.05, "minSample": 10}}
				}
			}
			expID := h.runningExperiment(t, "blocked-"+scenario, mutate)
			h.createPlan(t, expID,
				step{Name: "canary", Ratio: 50, MinRuntime: int64(time.Hour), AutoAdvance: true},
				step{Name: "full", Ratio: 100, MinRuntime: int64(time.Hour), AutoAdvance: true})
			b, err := platform.NewPGStore(t.Context(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = b.Close() })
			second := rollout.NewEngine(b, admin.NewService(b, h.api.Config, nil), nil)
			var clock atomic.Int64
			clock.Store(h.fakeNow.UnixNano())
			for _, engine := range []*rollout.Engine{h.api.Rollout, second} {
				engine.LeaseDuration = time.Second
				engine.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			}
			ctxA, cancelA := context.WithCancel(t.Context())
			ctxB, cancelB := context.WithCancel(t.Context())
			var runs sync.WaitGroup
			defer func() { cancelA(); cancelB(); runs.Wait() }()
			runs.Go(func() { h.api.Rollout.Run(ctxA, 40*time.Millisecond) })
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if err := a.ReloadFromBackend(); err != nil {
					t.Fatal(err)
				}
				if h.getPlan(t, expID).State == platform.ReleasePlanRunning {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if h.getPlan(t, expID).State != platform.ReleasePlanRunning {
				t.Fatal("step 0 did not start")
			}
			if scenario == "srm" {
				h.feedSkewedExposures(t, expID, 120, 20)
			} else {
				h.feedTraffic(t, expID, 0, 400, 0.1, 0.1,
					&extraMetricSpec{Key: "latency", ControlValue: 100, TreatmentValue: 200})
			}
			cancelA()
			clock.Store(h.fakeNow.Add(11 * 24 * time.Hour).UnixNano())
			runs.Go(func() { second.Run(ctxB, 40*time.Millisecond) })
			want := platform.ReleasePlanPaused
			if scenario == "guardrail" {
				want = platform.ReleasePlanRolledBack
			}
			deadline = time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if err := a.ReloadFromBackend(); err != nil {
					t.Fatal(err)
				}
				if h.getPlan(t, expID).State == want {
					break
				}
				time.Sleep(30 * time.Millisecond)
			}
			plan := h.getPlan(t, expID)
			if plan.State != want || plan.CurrentStep != 0 {
				t.Fatalf("blocked gate: %+v", plan)
			}
			starts, terminal := 0, 0
			for _, event := range plan.History {
				if event.Action == "plan.start" {
					starts++
				}
				if event.Action == "plan.pause" || event.Action == "plan.rollback" {
					terminal++
				}
			}
			if starts != 1 || terminal != 1 {
				t.Fatalf("duplicate history: %+v", plan.History)
			}
			if scenario == "guardrail" {
				auditCount := 0
				for _, audit := range a.AuditList(h.ws.ID, 100) {
					if audit.Action == "release_plan.rollback" && audit.Object == expID {
						auditCount++
					}
				}
				if auditCount != 1 {
					t.Fatalf("rollback audit count = %d", auditCount)
				}
			}
		})
	}
}

func TestMultiReplicaManualPauseSerialized(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ws, err := a.CreateWorkspace("w", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.CreateEnvironment(ws.ID, "e", "")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := a.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: 1,
		Unit: "user_id", Variants: []string{"a", "b"},
		Proportions:   map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: platform.MetricSpec{Key: "m", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetReleasePlan(exp.ID, &platform.ReleasePlan{
		State: platform.ReleasePlanRunning, Steps: []platform.ReleaseStep{{Name: "first", Ratio: 10}},
	}); err != nil {
		t.Fatal(err)
	}
	b, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	actor := admin.Actor{Kind: platform.TokenAdmin, Role: platform.RoleOwner}
	engines := []*rollout.Engine{
		rollout.NewEngine(a, admin.NewService(a, nil, nil), nil),
		rollout.NewEngine(b, admin.NewService(b, nil, nil), nil),
	}
	start := make(chan struct{})
	results := make(chan error, len(engines))
	var wg sync.WaitGroup
	for _, engine := range engines {
		wg.Go(func() {
			<-start
			_, err := engine.Pause(actor, exp.ID, "operator pause")
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, platform.ErrInvalidState) {
			t.Fatalf("unexpected pause error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful pauses = %d, want exactly one", successes)
	}
	if err := a.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	got, err := a.GetExperiment(exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReleasePlan.State != platform.ReleasePlanPaused || len(got.ReleasePlan.History) != 1 {
		t.Fatalf("persisted plan = %+v, want one pause", got.ReleasePlan)
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return newHarnessOnStore(t, store)
}

func newHarnessOnStore(t *testing.T, store *platform.Store) *harness {
	t.Helper()
	cfg := configstore.NewGonacosStore(gconfig.NewService())
	apiSrv := api.NewServer(store, cfg, api.Options{})
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)

	h := &harness{t: t, store: store, api: apiSrv, srv: srv, fakeNow: time.Now().UTC()}
	// Deterministic engine clock: tests advance fakeNow explicitly.
	apiSrv.Rollout.Now = func() time.Time { return h.fakeNow }

	ct, err := store.CreateToken(platform.TokenAdmin, "bootstrap-owner", platform.RoleOwner, "", "", "bootstrap")
	if err != nil {
		t.Fatalf("owner token: %v", err)
	}
	h.owner = ct.Secret
	h.setupWorkspace()
	return h
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
	var pubCT platform.CreatedToken
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/tokens", h.owner,
		map[string]any{"name": "publisher-b", "kind": "admin", "role": "publisher"}, &pubCT); code != http.StatusCreated {
		h.t.Fatalf("create publisher token: %d", code)
	}
	h.publisher = pubCT.Secret
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

func (h *harness) advance(d time.Duration) { h.fakeNow = h.fakeNow.Add(d) }

func (h *harness) tick() {
	h.api.Rollout.Now = func() time.Time { return h.fakeNow }
	h.api.Rollout.Tick()
}

// runningExperiment publishes a two-arm 50/50 flag, registers a
// pre-registered experiment, binds + approves (second actor) + publishes the
// binding and transitions to Running — the state a release plan acts on.
func (h *harness) runningExperiment(t *testing.T, flagKey string, mutateExp func(map[string]any)) string {
	t.Helper()
	flag := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           flagKey, Type: model.FlagString,
		Fields:      []string{"user_id"},
		Bucketing:   &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: flagKey + "-salt"},
		Variations:  map[string]any{"control": "c", "treatment": "t"},
		DefaultRule: &model.Rule{Variation: "control"},
		Targeting: []model.Rule{
			{Name: "randomized", Percentage: map[string]float64{"control": 50, "treatment": 50}},
		},
	}
	h.publishFlag(flag, platform.RiskNormal, h.owner)

	expReq := map[string]any{
		"flagKey": flagKey, "unit": "user_id",
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
	if mutateExp != nil {
		mutateExp(expReq)
	}
	var exp platform.Experiment
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/experiments",
		h.owner, expReq, &exp); code != http.StatusCreated {
		t.Fatalf("create experiment: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/review",
		h.owner, map[string]any{"note": "pre-registered"}, nil); code != http.StatusOK {
		t.Fatalf("review: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/bind",
		h.owner, nil, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	// High-risk binding draft: approval by the second actor (二人规则).
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/"+flagKey+"/approve",
		h.publisher, map[string]any{"note": "second pair of eyes"}, nil); code != http.StatusOK {
		t.Fatalf("approve binding: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/"+flagKey+"/publish",
		h.owner, map[string]any{"note": "bind experiment"}, nil); code != http.StatusOK {
		t.Fatalf("publish binding: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/transition",
		h.owner, map[string]any{"to": "running", "reason": "launch"}, nil); code != http.StatusOK {
		t.Fatalf("start experiment: %d", code)
	}
	return exp.ID
}

func (h *harness) publishFlag(flag *model.Flag, risk platform.Risk, approver string) {
	h.t.Helper()
	base := fmt.Sprintf("/api/v1/admin/workspaces/acme/environments/prod/flags/%s", flag.Key)
	if code := h.do("PUT", base+"/draft", h.owner,
		map[string]any{"content": flag, "risk": risk, "baseRevision": 0, "expectedDraftHash": ""}, nil); code != http.StatusOK {
		h.t.Fatalf("save draft: %d", code)
	}
	if code := h.do("POST", base+"/submit", h.owner, map[string]any{"reason": "rollout test"}, nil); code != http.StatusOK {
		h.t.Fatalf("submit: %d", code)
	}
	if approver == h.owner {
		approver = h.publisher
	}
	if code := h.do("POST", base+"/approve", approver, map[string]any{"note": "ok"}, nil); code != http.StatusOK {
		h.t.Fatalf("approve: %d", code)
	}
	if code := h.do("POST", base+"/publish", h.owner, map[string]any{"note": "ship"}, nil); code != http.StatusOK {
		h.t.Fatalf("publish: %d", code)
	}
}

type step struct {
	Name              string  `json:"name"`
	Ratio             float64 `json:"ratio"`
	MinRuntime        int64   `json:"minRuntime"`
	ObservationWindow int64   `json:"observationWindow,omitempty"`
	AutoAdvance       bool    `json:"autoAdvance"`
}

func (h *harness) createPlan(t *testing.T, expID string, steps ...step) {
	t.Helper()
	var approval platform.Approval
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/approvals", h.owner,
		map[string]any{"action": "release_plan.create", "object": expID,
			"payload": map[string]any{"steps": steps}, "reason": "pre-registered rollout"}, &approval); code != http.StatusCreated {
		t.Fatalf("request release plan: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/approvals/"+approval.ID+"/approve", h.publisher,
		map[string]any{"note": "plan checked"}, nil); code != http.StatusOK {
		t.Fatalf("approve release plan: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan",
		h.owner, map[string]any{"steps": steps, "approvalId": approval.ID}, nil); code != http.StatusCreated {
		t.Fatalf("create release plan: %d", code)
	}
}

func (h *harness) getPlan(t *testing.T, expID string) platform.ReleasePlan {
	t.Helper()
	var plan platform.ReleasePlan
	if code := h.do("GET", "/api/v1/admin/experiments/"+expID+"/release-plan",
		h.owner, nil, &plan); code != http.StatusOK {
		t.Fatalf("get release plan: %d", code)
	}
	return plan
}

func (h *harness) getExperiment(t *testing.T, expID string) platform.Experiment {
	t.Helper()
	var exp platform.Experiment
	if code := h.do("GET", "/api/v1/admin/experiments/"+expID, h.owner, nil, &exp); code != http.StatusOK {
		t.Fatalf("get experiment: %d", code)
	}
	return exp
}

// publishedEligibility reads the eligibility of the in-force flag binding.
func (h *harness) publishedEligibility(t *testing.T, flagKey string) float64 {
	t.Helper()
	cur := h.store.CurrentFlagContents(h.ws.ID, h.env.ID)[flagKey]
	if cur == nil || cur.Content.Experiment == nil {
		t.Fatalf("flag %s has no published experiment binding", flagKey)
	}
	return cur.Content.Experiment.Eligibility
}

func (h *harness) manifestRevision() int64 {
	return h.store.ManifestRevision(h.ws.ID, h.env.ID)
}

// feedTraffic evaluates n fresh units against the CURRENT published snapshot
// and ingests the resulting split exposures (plus deterministic conversions)
// directly into the store — the same contract the SDK/collect path fulfills,
// but fully deterministic. Units are indexed from startAt so repeated calls
// never collide. Conversion rule: treatment converts when i%10 < treatPct*10,
// control when i%10 < ctrlPct*10. Optional extra metric events are attached
// per unit via extraMetric (value chosen by variant).
func (h *harness) feedTraffic(t *testing.T, expID string, startAt, n int,
	treatPct, ctrlPct float64, extraMetric *extraMetricSpec) int {
	t.Helper()
	exp, err := h.store.GetExperiment(expID)
	if err != nil {
		t.Fatalf("get experiment: %v", err)
	}
	snapshot, err := h.api.Config.LoadSnapshot(h.env.NamespaceID)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	ev, err := evaluation.NewEvaluator(snapshot)
	if err != nil {
		t.Fatalf("evaluator: %v", err)
	}
	now := time.Now().UTC()
	var events []*platform.Event
	enrolled := 0
	for i := startAt; i < startAt+n; i++ {
		unit := fmt.Sprintf("user-%d", i)
		res := ev.Evaluate(exp.FlagKey, model.EvaluationContext{Attributes: map[string]any{"user_id": unit}})
		if res.Experiment == nil || res.Experiment.Source != "split" || !res.TrackEvents {
			continue
		}
		enrolled++
		unitHash := h.store.HashUnit(h.ws.ID, unit)
		h.eventSeq++
		events = append(events, &platform.Event{
			EventID: fmt.Sprintf("test-exp-%d", h.eventSeq), Type: platform.EventExposure,
			WorkspaceID: h.ws.ID, EnvironmentID: h.env.ID,
			ExperimentID: expID, Epoch: exp.Epoch, FlagKey: exp.FlagKey,
			Variant: res.Experiment.Variant, UnitHash: unitHash, Source: "split",
			ClientTime: now,
		})
		treat := res.Experiment.Variant == "treatment"
		convert := (treat && float64(i%10) < treatPct*10) || (!treat && float64(i%10) < ctrlPct*10)
		if convert {
			h.eventSeq++
			events = append(events, &platform.Event{
				EventID: fmt.Sprintf("test-conv-%d", h.eventSeq), Type: platform.EventConversion,
				WorkspaceID: h.ws.ID, EnvironmentID: h.env.ID,
				ExperimentID: expID, Epoch: exp.Epoch, FlagKey: exp.FlagKey,
				Variant: res.Experiment.Variant, UnitHash: unitHash, Source: "split",
				MetricKey: exp.PrimaryMetric.Key, Value: 1, ClientTime: now,
			})
		}
		if extraMetric != nil {
			v := extraMetric.ControlValue
			if treat {
				v = extraMetric.TreatmentValue
			}
			h.eventSeq++
			events = append(events, &platform.Event{
				EventID: fmt.Sprintf("test-m-%d", h.eventSeq), Type: platform.EventConversion,
				WorkspaceID: h.ws.ID, EnvironmentID: h.env.ID,
				ExperimentID: expID, Epoch: exp.Epoch, FlagKey: exp.FlagKey,
				Variant: res.Experiment.Variant, UnitHash: unitHash, Source: "split",
				MetricKey: extraMetric.Key, Value: v, ClientTime: now,
			})
		}
	}
	res, err := h.store.IngestEvents(events)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Rejected > 0 {
		t.Fatalf("synthetic events rejected: %v", res.RejectMsgs)
	}
	return enrolled
}

// feedSkewedExposures ingests exposures with WRONG variant labels to trigger
// an SRM alarm (simulates a broken assignment pipeline — the anomaly the
// engine must block on instead of advancing).
func (h *harness) feedSkewedExposures(t *testing.T, expID string, nControl, nTreatment int) {
	t.Helper()
	exp, err := h.store.GetExperiment(expID)
	if err != nil {
		t.Fatalf("get experiment: %v", err)
	}
	now := time.Now().UTC()
	var events []*platform.Event
	for i, n := range []int{nControl, nTreatment} {
		variant := "control"
		if i == 1 {
			variant = "treatment"
		}
		for j := 0; j < n; j++ {
			unit := fmt.Sprintf("srm-%d-%d", i, j)
			h.eventSeq++
			events = append(events, &platform.Event{
				EventID: fmt.Sprintf("test-srm-%d", h.eventSeq), Type: platform.EventExposure,
				WorkspaceID: h.ws.ID, EnvironmentID: h.env.ID,
				ExperimentID: expID, Epoch: exp.Epoch, FlagKey: exp.FlagKey,
				Variant: variant, UnitHash: h.store.HashUnit(h.ws.ID, unit), Source: "split",
				ClientTime: now,
			})
		}
	}
	if _, err := h.store.IngestEvents(events); err != nil {
		t.Fatalf("ingest skewed: %v", err)
	}
}

type extraMetricSpec struct {
	Key            string
	ControlValue   float64
	TreatmentValue float64
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRolloutRampAdvancesOnHealthyData walks a 3-step ramp (20% → 50% →
// 100%) with a deterministic engine clock: steps only advance after the
// pre-registered dwell (minRuntime + observation window) AND a healthy
// decision gate; the final ship verdict completes the plan without touching
// the flag ratio (no automatic victory claim).
func TestRolloutRampAdvancesOnHealthyData(t *testing.T) {
	h := newHarness(t)
	expID := h.runningExperiment(t, "ramp-flag", nil)
	h.createPlan(t, expID,
		step{Name: "canary", Ratio: 20, MinRuntime: int64(time.Hour), ObservationWindow: int64(30 * time.Minute), AutoAdvance: true},
		step{Name: "half", Ratio: 50, MinRuntime: int64(time.Hour), AutoAdvance: true},
		step{Name: "full", Ratio: 100, MinRuntime: int64(time.Hour), AutoAdvance: true},
	)

	// Armed plan + running experiment → first tick starts step 0.
	baseRev := h.manifestRevision()
	h.tick()
	plan := h.getPlan(t, expID)
	if plan.State != platform.ReleasePlanRunning || plan.CurrentStep != 0 {
		t.Fatalf("plan should be running step 0: %+v", plan)
	}
	if elig := h.publishedEligibility(t, "ramp-flag"); elig != 20 {
		t.Fatalf("step-0 eligibility = %v, want 20", elig)
	}
	step0Rev := h.manifestRevision()
	if step0Rev <= baseRev {
		t.Fatalf("starting step 0 must republish the manifest (%d → %d)", baseRev, step0Rev)
	}

	// Waiting: dwell not elapsed → the gate is never consulted (§10.3 no peeking).
	if n := h.feedTraffic(t, expID, 0, 600, 0.3, 0.1, nil); n < 60 {
		t.Fatalf("expected ≥60 enrollments at 20%%, got %d", n)
	}
	h.advance(45 * time.Minute)
	h.tick()
	plan = h.getPlan(t, expID)
	if plan.CurrentStep != 0 || h.manifestRevision() != step0Rev {
		t.Fatalf("engine must wait out minRuntime+window: step=%d rev=%d", plan.CurrentStep, h.manifestRevision())
	}
	// Idempotence: repeated ticks while waiting change nothing.
	h.tick()
	h.tick()
	if h.manifestRevision() != step0Rev {
		t.Fatalf("waiting ticks must be idempotent (rev %d → %d)", step0Rev, h.manifestRevision())
	}

	// Dwell elapsed (45m + 2h > 1h30m), data healthy, ship verdict → advance.
	h.advance(2 * time.Hour)
	h.tick()
	plan = h.getPlan(t, expID)
	if plan.CurrentStep != 1 || plan.State != platform.ReleasePlanRunning {
		t.Fatalf("plan should have advanced to step 1: %+v", plan)
	}
	if elig := h.publishedEligibility(t, "ramp-flag"); elig != 50 {
		t.Fatalf("step-1 eligibility = %v, want 50", elig)
	}
	halfRev := h.manifestRevision()

	// Second step: fresh traffic, dwell, advance to the final step.
	h.feedTraffic(t, expID, 1000, 600, 0.3, 0.1, nil)
	h.advance(time.Hour + time.Minute)
	h.tick()
	plan = h.getPlan(t, expID)
	if plan.CurrentStep != 2 {
		t.Fatalf("plan should be on the final step: %+v", plan)
	}
	if elig := h.publishedEligibility(t, "ramp-flag"); elig != 100 {
		t.Fatalf("final eligibility = %v, want 100", elig)
	}

	// Final step + ship → completed; ratio stays at 100 (human decides full rollout).
	h.feedTraffic(t, expID, 2000, 600, 0.3, 0.1, nil)
	h.advance(time.Hour + time.Minute)
	h.tick()
	plan = h.getPlan(t, expID)
	if plan.State != platform.ReleasePlanCompleted {
		t.Fatalf("plan should be completed: %+v", plan)
	}
	if elig := h.publishedEligibility(t, "ramp-flag"); elig != 100 {
		t.Fatalf("completion must not change the ratio: %v", elig)
	}
	// Terminal: further ticks are no-ops.
	completedRev := h.manifestRevision()
	h.advance(5 * time.Hour)
	h.tick()
	if h.manifestRevision() != completedRev {
		t.Fatalf("completed plan must not publish again")
	}
	if exp := h.getExperiment(t, expID); exp.State != model.ExperimentRunning {
		t.Fatalf("completion must not stop the experiment: %s", exp.State)
	}

	// History recorded every automatic action.
	var actions []string
	for _, ev := range plan.History {
		actions = append(actions, ev.Action)
	}
	want := []string{"plan.create", "plan.start", "step.advance", "step.advance", "plan.complete"}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("history actions = %v, want %v", actions, want)
	}
	_ = halfRev
}

// TestRolloutWaitsForExperimentMinRuntimeAndSample: with healthy data but
// the experiment-level minimum runtime unmet or the per-arm sample too
// small, the gate answers hold and the engine simply waits — no pause, no
// advance.
func TestRolloutWaitsForExperimentMinRuntimeAndSample(t *testing.T) {
	h := newHarness(t)
	expID := h.runningExperiment(t, "slow-flag", func(m map[string]any) {
		m["minRuntime"] = int64(3 * time.Hour)
	})
	h.createPlan(t, expID,
		step{Name: "canary", Ratio: 50, MinRuntime: int64(time.Hour), AutoAdvance: true},
		step{Name: "full", Ratio: 100, MinRuntime: int64(time.Hour), AutoAdvance: true},
	)
	h.tick() // start step 0
	baseRev := h.manifestRevision()

	// Tiny traffic: sample below minSamplePerArm=20/arm AND exp minRuntime unmet.
	h.feedTraffic(t, expID, 0, 60, 0.3, 0.1, nil)
	h.advance(2 * time.Hour)
	h.tick()
	plan := h.getPlan(t, expID)
	if plan.CurrentStep != 0 || plan.State != platform.ReleasePlanRunning {
		t.Fatalf("hold verdict must mean waiting, got %+v", plan)
	}
	if h.manifestRevision() != baseRev {
		t.Fatalf("waiting must not republish")
	}

	// Enough sample, experiment min runtime elapsed → advance.
	h.feedTraffic(t, expID, 1000, 400, 0.3, 0.1, nil)
	h.advance(2 * time.Hour) // total +4h > exp minRuntime 3h
	h.tick()
	plan = h.getPlan(t, expID)
	if plan.CurrentStep != 1 {
		t.Fatalf("plan should advance once runtime+sample gates pass: %+v", plan)
	}
}

// TestRolloutPausesOnSRM: an SRM alarm (broken assignment pipeline) must
// pause the plan instead of advancing; the pause is idempotent and only a
// human resume (which restarts the observation window) re-arms the engine.
func TestRolloutPausesOnSRM(t *testing.T) {
	h := newHarness(t)
	expID := h.runningExperiment(t, "srm-flag", nil)
	h.createPlan(t, expID,
		step{Name: "canary", Ratio: 50, MinRuntime: int64(time.Hour), AutoAdvance: true},
		step{Name: "full", Ratio: 100, MinRuntime: int64(time.Hour), AutoAdvance: true},
	)
	h.tick() // start step 0
	baseRev := h.manifestRevision()

	h.feedSkewedExposures(t, expID, 120, 20) // 50/50 expected → SRM alarm
	h.advance(2 * time.Hour)
	h.tick()

	plan := h.getPlan(t, expID)
	if plan.State != platform.ReleasePlanPaused {
		t.Fatalf("SRM must pause the plan: %+v", plan)
	}
	if plan.PausedReason == "" {
		t.Fatal("pause must record a reason")
	}
	if exp := h.getExperiment(t, expID); exp.State != model.ExperimentRunning {
		t.Fatalf("SRM pause must NOT stop the experiment: %s", exp.State)
	}
	if h.manifestRevision() != baseRev {
		t.Fatalf("pause must not republish")
	}

	// Idempotent: further ticks add no history and change nothing.
	historyLen := len(plan.History)
	h.advance(time.Hour)
	h.tick()
	h.tick()
	plan = h.getPlan(t, expID)
	if plan.State != platform.ReleasePlanPaused || len(plan.History) != historyLen {
		t.Fatalf("paused plan must be inert: %+v", plan)
	}

	// Operator resume → running again with a RESTARTED step clock: an
	// immediate tick must not advance even though wall time passed.
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/resume",
		h.owner, nil, nil); code != http.StatusOK {
		t.Fatalf("resume: %d", code)
	}
	plan = h.getPlan(t, expID)
	if plan.State != platform.ReleasePlanRunning || plan.PausedReason != "" {
		t.Fatalf("resume must restore running: %+v", plan)
	}
	resumedAt := plan.StepStartedAt
	h.tick()
	plan = h.getPlan(t, expID)
	if plan.CurrentStep != 0 {
		t.Fatalf("resumed plan must serve a fresh observation window: %+v", plan)
	}
	if plan.StepStartedAt == nil || resumedAt == nil || !plan.StepStartedAt.Equal(*resumedAt) {
		t.Fatalf("resume must restart the step clock")
	}
}

// TestRolloutRollsBackOnGuardrailBreach: a breached guardrail fires the
// rollback verdict → the engine stops enrollment (experiment Stopped, bound
// flag republished) and marks the plan rolled_back — terminal.
func TestRolloutRollsBackOnGuardrailBreach(t *testing.T) {
	h := newHarness(t)
	expID := h.runningExperiment(t, "guard-flag", func(m map[string]any) {
		m["guardrails"] = []map[string]any{{
			"metricKey": "latency", "type": "mean", "direction": "decrease",
			"maxRelativeRegression": 0.05, "minSample": 10,
		}}
	})
	h.createPlan(t, expID,
		step{Name: "canary", Ratio: 50, MinRuntime: int64(time.Hour), AutoAdvance: true},
		step{Name: "full", Ratio: 100, MinRuntime: int64(time.Hour), AutoAdvance: true},
	)
	h.tick() // start step 0

	// Treatment latency 2× control → 100% regression vs the 5% threshold.
	h.feedTraffic(t, expID, 0, 400, 0.1, 0.1,
		&extraMetricSpec{Key: "latency", ControlValue: 100, TreatmentValue: 200})
	h.advance(2 * time.Hour)
	h.tick()

	plan := h.getPlan(t, expID)
	if plan.State != platform.ReleasePlanRolledBack {
		t.Fatalf("guardrail breach must roll the plan back: %+v", plan)
	}
	exp := h.getExperiment(t, expID)
	if exp.State != model.ExperimentStopped {
		t.Fatalf("rollback must stop the experiment: %s", exp.State)
	}

	// The republished snapshot must no longer enroll anyone.
	snapshot, err := h.api.Config.LoadSnapshot(h.env.NamespaceID)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	ev, err := evaluation.NewEvaluator(snapshot)
	if err != nil {
		t.Fatalf("evaluator: %v", err)
	}
	res := ev.Evaluate("guard-flag", model.EvaluationContext{Attributes: map[string]any{"user_id": "user-1"}})
	if res.TrackEvents || res.Experiment != nil {
		t.Fatalf("stopped experiment must not enroll: %+v", res)
	}

	// Terminal: ticks are no-ops.
	rev := h.manifestRevision()
	h.advance(10 * time.Hour)
	h.tick()
	h.tick()
	if h.manifestRevision() != rev {
		t.Fatalf("rolled-back plan must not publish again")
	}
	// Rollback is auditable.
	var audits []platform.AuditEntry
	if code := h.do("GET", "/api/v1/admin/workspaces/acme/audit?limit=50", h.owner, nil, &audits); code != http.StatusOK {
		t.Fatalf("audit: %d", code)
	}
	found := false
	for _, a := range audits {
		if a.Action == "release_plan.rollback" {
			found = true
		}
	}
	if !found {
		t.Fatalf("rollback must appear in the audit trail: %v", audits)
	}
}

// TestRolloutManualTakeoverPauseAdvance covers the operator surface: the
// 人工接管 switch fully stands the engine down, pause/resume behave like the
// automatic path, and manual advance is a human publish decision that is
// recorded with the gate verdict for traceability.
func TestRolloutManualTakeoverPauseAdvance(t *testing.T) {
	h := newHarness(t)
	expID := h.runningExperiment(t, "manual-flag", nil)
	h.createPlan(t, expID,
		step{Name: "canary", Ratio: 50, MinRuntime: int64(time.Hour), AutoAdvance: true},
		step{Name: "full", Ratio: 100, MinRuntime: int64(time.Hour), AutoAdvance: true},
	)
	h.tick() // start step 0
	h.feedTraffic(t, expID, 0, 400, 0.3, 0.1, nil)
	h.advance(2 * time.Hour)

	// Takeover ON: a tick that WOULD advance must do nothing.
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/takeover",
		h.owner, map[string]any{"enabled": true, "reason": "incident freeze"}, nil); code != http.StatusOK {
		t.Fatalf("takeover: %d", code)
	}
	rev := h.manifestRevision()
	h.tick()
	h.tick()
	plan := h.getPlan(t, expID)
	if plan.CurrentStep != 0 || !plan.ManualOverride || h.manifestRevision() != rev {
		t.Fatalf("manual override must stand the engine down: %+v", plan)
	}

	// Manual advance works during takeover (it IS the human action).
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/advance",
		h.owner, map[string]any{"reason": "operator decided"}, nil); code != http.StatusOK {
		t.Fatalf("manual advance: %d", code)
	}
	plan = h.getPlan(t, expID)
	if plan.CurrentStep != 1 {
		t.Fatalf("manual advance must move the step: %+v", plan)
	}
	if elig := h.publishedEligibility(t, "manual-flag"); elig != 100 {
		t.Fatalf("manual advance must apply the next ratio: %v", elig)
	}
	last := plan.History[len(plan.History)-1]
	if last.Action != "manual.advance" || last.Actor == "rollout-engine" {
		t.Fatalf("manual advance must be actor-attributed: %+v", last)
	}

	// Takeover OFF + pause: engine inert again while paused.
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/takeover",
		h.owner, map[string]any{"enabled": false}, nil); code != http.StatusOK {
		t.Fatalf("takeover off: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/pause",
		h.owner, map[string]any{"reason": "change freeze"}, nil); code != http.StatusOK {
		t.Fatalf("pause: %d", code)
	}
	h.advance(5 * time.Hour)
	rev = h.manifestRevision()
	h.tick()
	if plan := h.getPlan(t, expID); plan.State != platform.ReleasePlanPaused || h.manifestRevision() != rev {
		t.Fatalf("paused plan must be inert: %+v", plan)
	}

	// Resume + dwell + healthy ship data → engine completes the final step.
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/resume",
		h.owner, nil, nil); code != http.StatusOK {
		t.Fatalf("resume: %d", code)
	}
	h.feedTraffic(t, expID, 1000, 400, 0.3, 0.1, nil)
	h.advance(2 * time.Hour)
	h.tick()
	if plan := h.getPlan(t, expID); plan.State != platform.ReleasePlanCompleted {
		t.Fatalf("final ship verdict must complete the plan: %+v", plan)
	}
}

func TestOperatorActionsRequireAvailableReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	store, err := platform.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessOnStore(t, store)
	expID := h.runningExperiment(t, "report-gate", nil)
	h.createPlan(t, expID,
		step{Name: "canary", Ratio: 20, MinRuntime: int64(time.Hour)},
		step{Name: "full", Ratio: 100, MinRuntime: int64(time.Hour)})
	h.tick()
	if plan := h.getPlan(t, expID); plan.State != platform.ReleasePlanRunning {
		t.Fatalf("plan did not start: %+v", plan)
	}

	// Corrupt the frozen metric reference in the persisted fixture so report
	// construction fails while ordinary experiment and plan reads still work.
	setMissingMetric := func(missing bool) {
		t.Helper()
		dataBytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var data platform.Data
		if err := json.Unmarshal(dataBytes, &data); err != nil {
			t.Fatal(err)
		}
		if missing {
			data.Experiments[expID].PrimaryMetricRef = &platform.MetricRef{
				Key: "missing", Version: 1, DefinitionHash: "missing",
			}
		} else {
			data.Experiments[expID].PrimaryMetricRef = nil
		}
		dataBytes, err = json.Marshal(&data)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, dataBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.ReloadFromBackend(); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/v1/admin/experiments/" + expID + "/release-plan"
	setMissingMetric(true)
	if code := h.do("POST", base+"/advance", h.owner, map[string]any{"reason": "override"}, nil); code != http.StatusConflict {
		t.Fatalf("manual advance with unavailable report: %d", code)
	}
	if plan := h.getPlan(t, expID); plan.CurrentStep != 0 {
		t.Fatalf("failed advance changed step: %+v", plan)
	}
	if code := h.do("POST", base+"/pause", h.owner, map[string]any{"reason": "investigate"}, nil); code != http.StatusOK {
		t.Fatalf("pause should remain available: %d", code)
	}
	if code := h.do("POST", base+"/resume", h.owner, nil, nil); code != http.StatusConflict {
		t.Fatalf("resume with unavailable report: %d", code)
	}
	if code := h.do("POST", base+"/takeover", h.owner, map[string]any{"enabled": true, "reason": "freeze"}, nil); code != http.StatusOK {
		t.Fatalf("manual takeover should remain available: %d", code)
	}
	if code := h.do("POST", base+"/takeover", h.owner, map[string]any{"enabled": false}, nil); code != http.StatusConflict {
		t.Fatalf("return to automatic control with unavailable report: %d", code)
	}
	if plan := h.getPlan(t, expID); plan.State != platform.ReleasePlanPaused || !plan.ManualOverride {
		t.Fatalf("failed actions changed pause or takeover state: %+v", plan)
	}

	setMissingMetric(false)
	if code := h.do("POST", base+"/resume", h.owner, nil, nil); code != http.StatusOK {
		t.Fatalf("resume after report recovery: %d", code)
	}
	if code := h.do("POST", base+"/takeover", h.owner, map[string]any{"enabled": false}, nil); code != http.StatusOK {
		t.Fatalf("return to automatic control after recovery: %d", code)
	}
	if code := h.do("POST", base+"/advance", h.owner, map[string]any{"reason": "operator decision"}, nil); code != http.StatusOK {
		t.Fatalf("manual advance after report recovery: %d", code)
	}
	plan := h.getPlan(t, expID)
	if plan.CurrentStep != 1 || plan.ManualOverride {
		t.Fatalf("operator actions did not recover: %+v", plan)
	}
	if last := plan.History[len(plan.History)-1]; last.Action != "manual.advance" || last.Actor == "" {
		t.Fatalf("manual advance lacks actor-attributed history: %+v", last)
	}
	foundAudit := false
	for _, entry := range store.AuditList(h.ws.ID, 50) {
		foundAudit = foundAudit || entry.Action == "release_plan.manual_advance" && entry.Object == expID
	}
	if !foundAudit {
		t.Fatal("manual advance lacks audit record")
	}
}

// TestRolloutAutoAdvanceFalseHoldsAtBoundary: steps pre-registered WITHOUT
// autoAdvance hold at the boundary even with a passing gate — only an
// operator may move them.
func TestRolloutAutoAdvanceFalseHoldsAtBoundary(t *testing.T) {
	h := newHarness(t)
	expID := h.runningExperiment(t, "hold-flag", nil)
	h.createPlan(t, expID,
		step{Name: "canary", Ratio: 50, MinRuntime: int64(time.Hour), AutoAdvance: false},
		step{Name: "full", Ratio: 100, MinRuntime: int64(time.Hour), AutoAdvance: false},
	)
	h.tick()
	h.feedTraffic(t, expID, 0, 400, 0.3, 0.1, nil)
	h.advance(3 * time.Hour)
	rev := h.manifestRevision()
	h.tick()
	h.tick()
	plan := h.getPlan(t, expID)
	if plan.CurrentStep != 0 || plan.State != platform.ReleasePlanRunning {
		t.Fatalf("autoAdvance=false must hold at the boundary: %+v", plan)
	}
	if h.manifestRevision() != rev {
		t.Fatalf("holding must not republish")
	}
	// Operator completes the final... first step manually.
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/advance",
		h.owner, map[string]any{"reason": "gate passed, operator confirms"}, nil); code != http.StatusOK {
		t.Fatalf("manual advance: %d", code)
	}
	if plan := h.getPlan(t, expID); plan.CurrentStep != 1 {
		t.Fatalf("manual advance must move the step: %+v", plan)
	}
}

// TestRolloutWaitsForExperimentRunning: an armed plan does nothing until the
// experiment itself is Running — registration never changes live traffic.
func TestRolloutWaitsForExperimentRunning(t *testing.T) {
	h := newHarness(t)
	// Build the experiment, register the plan while Running, then pause the
	// experiment before the engine ever ticks: the armed plan must not touch
	// traffic until the experiment itself runs.
	flagKey := "scheduled-flag"
	expID := h.runningExperiment(t, flagKey, nil)
	h.createPlan(t, expID,
		step{Name: "canary", Ratio: 20, MinRuntime: int64(time.Hour), AutoAdvance: true},
		step{Name: "full", Ratio: 100, MinRuntime: int64(time.Hour), AutoAdvance: true},
	)
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/transition",
		h.owner, map[string]any{"to": "paused", "reason": "hold launch"}, nil); code != http.StatusOK {
		t.Fatalf("pause experiment: %d", code)
	}
	rev := h.manifestRevision()
	h.tick()
	h.advance(5 * time.Hour)
	h.tick()
	plan := h.getPlan(t, expID)
	if plan.State != platform.ReleasePlanArmed || h.manifestRevision() != rev {
		t.Fatalf("armed plan must wait for a running experiment: %+v", plan)
	}
	// Relaunch → the next tick starts step 0 at its pre-registered ratio.
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/transition",
		h.owner, map[string]any{"to": "running", "reason": "launch"}, nil); code != http.StatusOK {
		t.Fatalf("start: %d", code)
	}
	h.tick()
	plan = h.getPlan(t, expID)
	if plan.State != platform.ReleasePlanRunning || plan.CurrentStep != 0 {
		t.Fatalf("plan should start with the experiment: %+v", plan)
	}
	if elig := h.publishedEligibility(t, flagKey); elig != 20 {
		t.Fatalf("step-0 ratio not applied: %v", elig)
	}
	// After the dwell with healthy ship data the ramp advances normally.
	h.feedTraffic(t, expID, 0, 400, 0.3, 0.1, nil)
	h.advance(2 * time.Hour)
	h.tick()
	plan = h.getPlan(t, expID)
	if plan.CurrentStep != 1 {
		t.Fatalf("plan should resume advancing with the experiment: %+v", plan)
	}
	if elig := h.publishedEligibility(t, flagKey); elig != 100 {
		t.Fatalf("step-1 ratio not applied: %v", elig)
	}
}

// TestReleasePlanAPIValidation exercises the plan API contract: validation
// errors, state guards, audience separation and the sdk/revision projection
// endpoint used by the remote-evaluation SDKs.
func TestReleasePlanAPIValidation(t *testing.T) {
	h := newHarness(t)
	expID := h.runningExperiment(t, "api-flag", nil)

	// Decreasing ratios are rejected (ramps never shrink).
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan", h.owner,
		map[string]any{"steps": []step{
			{Name: "a", Ratio: 50, MinRuntime: int64(time.Hour)},
			{Name: "b", Ratio: 20, MinRuntime: int64(time.Hour)},
		}}, nil); code != http.StatusBadRequest {
		t.Fatalf("decreasing ramp must be rejected: %d", code)
	}
	// Zero minRuntime is rejected (gate could fire on a dataless step).
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan", h.owner,
		map[string]any{"steps": []step{{Name: "a", Ratio: 50, MinRuntime: 0}}}, nil); code != http.StatusBadRequest {
		t.Fatalf("minRuntime=0 must be rejected: %d", code)
	}
	// Ratio out of range.
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan", h.owner,
		map[string]any{"steps": []step{{Name: "a", Ratio: 120, MinRuntime: int64(time.Hour)}}}, nil); code != http.StatusBadRequest {
		t.Fatalf("ratio>100 must be rejected: %d", code)
	}

	// No plan yet → 404.
	if code := h.do("GET", "/api/v1/admin/experiments/"+expID+"/release-plan", h.owner, nil, nil); code != http.StatusNotFound {
		t.Fatalf("plan GET without plan: %d", code)
	}

	h.createPlan(t, expID, step{Name: "canary", Ratio: 50, MinRuntime: int64(time.Hour), AutoAdvance: true})
	// Duplicate plan → conflict (plans are pre-registered once).
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan", h.owner,
		map[string]any{"steps": []step{{Name: "x", Ratio: 10, MinRuntime: int64(time.Hour)}}}, nil); code != http.StatusConflict {
		t.Fatalf("duplicate plan: %d", code)
	}
	// Pause without reason → 400.
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/pause", h.owner,
		map[string]any{"reason": ""}, nil); code != http.StatusBadRequest {
		t.Fatalf("pause without reason: %d", code)
	}
	// Audience separation: sdk tokens cannot operate plans.
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/pause", h.sdkTok,
		map[string]any{"reason": "nope"}, nil); code != http.StatusForbidden {
		t.Fatalf("sdk token on admin endpoint: %d", code)
	}
	// Viewer-role token cannot pause (publish permission required).
	var viewCT platform.CreatedToken
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/tokens", h.owner,
		map[string]any{"name": "viewer", "kind": "admin", "role": "viewer"}, &viewCT); code != http.StatusCreated {
		t.Fatalf("create viewer: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+expID+"/release-plan/pause", viewCT.Secret,
		map[string]any{"reason": "nope"}, nil); code != http.StatusForbidden {
		t.Fatalf("viewer pause: %d", code)
	}

	// sdk/revision projection: matches the manifest revision and carries no
	// flag content whatsoever (§8 受限投影).
	var revResp map[string]any
	if code := h.do("GET", "/api/v1/sdk/revision", h.sdkTok, nil, &revResp); code != http.StatusOK {
		t.Fatalf("sdk revision: %d", code)
	}
	if int64(revResp["revision"].(float64)) != h.manifestRevision() {
		t.Fatalf("sdk revision %v != manifest %d", revResp["revision"], h.manifestRevision())
	}
	if len(revResp) != 2 {
		t.Fatalf("revision projection must stay minimal: %v", revResp)
	}
	// Admin token on sdk endpoint → wrong audience.
	if code := h.do("GET", "/api/v1/sdk/revision", h.owner, nil, nil); code != http.StatusForbidden {
		t.Fatalf("admin token on sdk endpoint: %d", code)
	}
}

// TestSDKRevisionZeroWithoutManifest: a fresh environment with nothing
// published reports revision 0 so polling SDKs treat their cache as empty.
func TestSDKRevisionZeroWithoutManifest(t *testing.T) {
	h := newHarness(t)
	var env platform.Environment
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments", h.owner,
		map[string]any{"slug": "staging", "name": "Staging"}, &env); code != http.StatusCreated {
		t.Fatalf("create env: %d", code)
	}
	var ct platform.CreatedToken
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/tokens", h.owner,
		map[string]any{"name": "sdk-staging", "kind": "sdk", "environment": "staging"}, &ct); code != http.StatusCreated {
		t.Fatalf("create token: %d", code)
	}
	var revResp map[string]any
	if code := h.do("GET", "/api/v1/sdk/revision", ct.Secret, nil, &revResp); code != http.StatusOK {
		t.Fatalf("sdk revision: %d", code)
	}
	if int64(revResp["revision"].(float64)) != 0 {
		t.Fatalf("unpublished env must report revision 0: %v", revResp)
	}
}
