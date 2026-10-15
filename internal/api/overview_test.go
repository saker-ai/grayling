package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/admin"
	"github.com/saker-ai/grayling/internal/experiment"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

// overviewFixture wires an in-memory store with two owners (so one can see
// the other's grants as reviewable work), a viewer (role trimming), an
// outsider workspace (403) and two environments (env filtering).
type overviewFixture struct {
	store    *platform.Store
	server   *Server
	ws       *platform.Workspace
	env      *platform.Environment
	otherEnv *platform.Environment
	owner1   *platform.CreatedToken
	owner2   *platform.CreatedToken
	viewer   *platform.CreatedToken
	outsider *platform.CreatedToken
}

func newOverviewFixture(t *testing.T) *overviewFixture {
	t.Helper()
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("overview", "Overview")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "production", "Production")
	if err != nil {
		t.Fatal(err)
	}
	otherEnv, err := store.CreateEnvironment(ws.ID, "staging", "Staging")
	if err != nil {
		t.Fatal(err)
	}
	mint := func(name string, role platform.Role, workspaceID string) *platform.CreatedToken {
		t.Helper()
		tok, tokErr := store.CreateToken(platform.TokenAdmin, name, role, workspaceID, "", "test")
		if tokErr != nil {
			t.Fatal(tokErr)
		}
		return tok
	}
	otherWS, err := store.CreateWorkspace("overview-outsider", "Outsider")
	if err != nil {
		t.Fatal(err)
	}
	f := &overviewFixture{
		store: store, server: NewServer(store, nil, Options{}),
		ws: ws, env: env, otherEnv: otherEnv,
		owner1:   mint("owner1", platform.RoleOwner, ws.ID),
		owner2:   mint("owner2", platform.RoleOwner, ws.ID),
		viewer:   mint("viewer", platform.RoleViewer, ws.ID),
		outsider: mint("outsider", platform.RoleViewer, otherWS.ID),
	}
	return f
}

func (f *overviewFixture) call(t *testing.T, query, secret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workspaces/"+f.ws.ID+"/overview"+query, nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *overviewFixture) overview(t *testing.T, query, secret string) overviewResponse {
	t.Helper()
	rec := f.call(t, query, secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp overviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// createExperiment registers an experiment and walks it through the given
// state transitions (the store enforces the legal state machine order).
func (f *overviewFixture) createExperiment(t *testing.T, envID, flag string, to ...model.ExperimentState) *platform.Experiment {
	t.Helper()
	exp, err := f.store.CreateExperiment(&platform.Experiment{
		WorkspaceID: f.ws.ID, EnvironmentID: envID, FlagKey: flag, Unit: "user_id",
		Epoch: 1, Variants: []string{"control", "treatment"},
		Proportions:   map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: platform.MetricSpec{Key: "complete", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range to {
		if _, err := f.store.TransitionExperiment(exp.ID, state, "test"); err != nil {
			t.Fatal(err)
		}
	}
	return exp
}

func (f *overviewFixture) requestApproval(t *testing.T, submitter *platform.CreatedToken, envID, action, object string) *platform.Approval {
	t.Helper()
	a, err := f.store.RequestApproval(platform.Approval{
		WorkspaceID: f.ws.ID, EnvironmentID: envID, Action: action, Object: object,
		PayloadHash: "hash-" + object, BaseRevision: 1, Reason: "test change",
		SubmittedBy: submitter.Token.Name, SubmittedTokenID: submitter.Token.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *overviewFixture) audit(t *testing.T, action, envID, object string) {
	t.Helper()
	if err := f.store.Audit("operator", platform.RoleOwner, action, f.ws.ID, envID, object, "{}"); err != nil {
		t.Fatal(err)
	}
}

func findRisk(items []overviewRiskItem, id, kind string) *overviewRiskItem {
	for i := range items {
		if items[i].ID == id && items[i].Kind == kind {
			return &items[i]
		}
	}
	return nil
}

// Matrix 1: 200 aggregation with real modules, counts and coverage.
func TestWorkspaceOverviewAggregatesRealModules(t *testing.T) {
	f := newOverviewFixture(t)
	running := f.createExperiment(t, f.env.ID, "overview-running",
		model.ExperimentReviewed, model.ExperimentRunning)
	paused := f.createExperiment(t, f.env.ID, "overview-paused",
		model.ExperimentReviewed, model.ExperimentRunning, model.ExperimentPaused)

	rec := f.call(t, "?env="+f.env.ID, f.owner2.Secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache policy = %q", got)
	}
	var resp overviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.WorkspaceID != f.ws.ID || resp.EnvironmentID != f.env.ID {
		t.Fatalf("scope = %s/%s", resp.WorkspaceID, resp.EnvironmentID)
	}
	if resp.GeneratedAt.IsZero() {
		t.Fatal("generatedAt is zero")
	}
	c := resp.Counts
	if c.PausedExperiments == nil || *c.PausedExperiments != 1 ||
		c.RunningExperiments == nil || *c.RunningExperiments != 1 ||
		c.PendingApprovals == nil || *c.PendingApprovals != 0 ||
		c.PublishedFlags == nil || *c.PublishedFlags != 0 {
		t.Fatalf("counts = %+v", c)
	}
	if resp.Risks.Status != "ok" || resp.Risks.ReportCoverage != "complete" {
		t.Fatalf("risks status = %s/%s", resp.Risks.Status, resp.Risks.ReportCoverage)
	}
	cov := resp.Risks.Coverage
	if cov.Inspected != 2 || cov.Total != 2 || !cov.Complete {
		t.Fatalf("risks coverage = %+v", cov)
	}
	unhealthy := findRisk(resp.Risks.Items, running.ID, "experiment_unhealthy")
	if unhealthy == nil || unhealthy.Severity != "block" ||
		unhealthy.Text != "no randomized exposures recorded yet" {
		t.Fatalf("running risk = %+v in %+v", unhealthy, resp.Risks.Items)
	}
	if item := findRisk(resp.Risks.Items, paused.ID, "experiment_paused"); item == nil ||
		item.Severity != "warn" || item.EnvironmentID != f.env.ID {
		t.Fatalf("paused risk = %+v", item)
	}
	if resp.PendingApprovals.Status != "ok" || resp.PendingApprovals.ActionableTotal != 0 ||
		len(resp.PendingApprovals.Items) != 0 {
		t.Fatalf("pending = %+v", resp.PendingApprovals)
	}
	if resp.RecentReleases.Status != "ok" || !resp.RecentReleases.Coverage.Complete ||
		resp.RecentReleases.Coverage.Note != "audit-scan-window" {
		t.Fatalf("releases = %+v", resp.RecentReleases)
	}
}

// Matrix 2: a submitter sees their own request as reviewable work — the
// server no longer withholds it as a self-approval — and so does a peer.
func TestWorkspaceOverviewShowsSelfSubmittedApprovalAsReviewable(t *testing.T) {
	f := newOverviewFixture(t)
	a := f.requestApproval(t, f.owner1, f.env.ID, "flag.rollback", "flag-a")

	resp := f.overview(t, "", f.owner1.Secret)
	if resp.Counts.PendingApprovals == nil || *resp.Counts.PendingApprovals != 1 {
		t.Fatalf("submitted count = %+v", resp.Counts.PendingApprovals)
	}
	if len(resp.PendingApprovals.Items) != 1 {
		t.Fatalf("items = %+v", resp.PendingApprovals.Items)
	}
	item := resp.PendingApprovals.Items[0]
	if item.ID != a.ID || item.MyAction != "review" || item.BlockedReason != "" {
		t.Fatalf("submitter view = %+v", item)
	}
	if resp.PendingApprovals.ActionableTotal != 1 {
		t.Fatalf("submitter actionableTotal = %d", resp.PendingApprovals.ActionableTotal)
	}

	resp = f.overview(t, "", f.owner2.Secret)
	item = resp.PendingApprovals.Items[0]
	if item.MyAction != "review" || item.BlockedReason != "" {
		t.Fatalf("reviewer view = %+v", item)
	}
	if resp.PendingApprovals.ActionableTotal != 1 {
		t.Fatalf("reviewer actionableTotal = %d", resp.PendingApprovals.ActionableTotal)
	}
}

// Matrix 3: viewer keeps report-backed risks but has no approval actions.
func TestWorkspaceOverviewTrimsViewerRole(t *testing.T) {
	f := newOverviewFixture(t)
	running := f.createExperiment(t, f.env.ID, "viewer-flag",
		model.ExperimentReviewed, model.ExperimentRunning)
	f.requestApproval(t, f.owner1, f.env.ID, "flag.rollback", "flag-a")

	resp := f.overview(t, "", f.viewer.Secret)
	if resp.Risks.Status != "ok" || resp.Risks.ReportCoverage != "complete" {
		t.Fatalf("viewer risks = %s/%s", resp.Risks.Status, resp.Risks.ReportCoverage)
	}
	if findRisk(resp.Risks.Items, running.ID, "experiment_unhealthy") == nil {
		t.Fatalf("viewer risk items = %+v", resp.Risks.Items)
	}
	if len(resp.PendingApprovals.Items) != 1 {
		t.Fatalf("viewer items = %+v", resp.PendingApprovals.Items)
	}
	item := resp.PendingApprovals.Items[0]
	if item.MyAction != "" || item.BlockedReason != "role" {
		t.Fatalf("viewer pending item = %+v", item)
	}
	if resp.PendingApprovals.ActionableTotal != 0 {
		t.Fatalf("viewer actionableTotal = %d", resp.PendingApprovals.ActionableTotal)
	}
}

// Matrix 4 (HTTP half): an approved, unexpired grant is executable by the
// approver. Expired and break-glass grants are covered by the pure-function
// tests below (the store never mints them directly).
func TestWorkspaceOverviewApprovedGrantExecution(t *testing.T) {
	f := newOverviewFixture(t)
	a := f.requestApproval(t, f.owner2, f.env.ID, "flag.rollback", "flag-a")
	if _, err := f.store.ApproveChange(a.ID, f.owner1.Token.Name, f.owner1.Token.ID, "verified"); err != nil {
		t.Fatal(err)
	}
	resp := f.overview(t, "", f.owner1.Secret)
	if len(resp.PendingApprovals.Items) != 1 {
		t.Fatalf("items = %+v", resp.PendingApprovals.Items)
	}
	item := resp.PendingApprovals.Items[0]
	if item.MyAction != "execute" || item.BlockedReason != "" || item.ExpiresAt == nil {
		t.Fatalf("approved item = %+v", item)
	}
	if resp.PendingApprovals.ActionableTotal != 1 {
		t.Fatalf("actionableTotal = %d", resp.PendingApprovals.ActionableTotal)
	}
	if resp.Counts.PendingApprovals == nil || *resp.Counts.PendingApprovals != 0 {
		t.Fatalf("submitted count after approval = %+v", resp.Counts.PendingApprovals)
	}
}

// Matrix 5: cross-workspace callers are forbidden; anonymous callers get 401.
func TestWorkspaceOverviewAuthScope(t *testing.T) {
	f := newOverviewFixture(t)
	if got := f.call(t, "", f.outsider.Secret).Code; got != http.StatusForbidden {
		t.Fatalf("cross-workspace status = %d", got)
	}
	if got := f.call(t, "", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", got)
	}
}

// Matrix 6: limit and env validation.
func TestWorkspaceOverviewParameterValidation(t *testing.T) {
	f := newOverviewFixture(t)
	for _, query := range []string{"?limit=0", "?limit=51", "?limit=abc"} {
		if got := f.call(t, query, f.owner1.Secret).Code; got != http.StatusBadRequest {
			t.Errorf("query %q status = %d", query, got)
		}
	}
	if got := f.call(t, "?env=does-not-exist", f.owner1.Secret).Code; got != http.StatusNotFound {
		t.Errorf("unknown env status = %d", got)
	}
}

// Matrix 7: env narrows risks/counts/releases; approvals stay ws-level.
func TestWorkspaceOverviewEnvironmentFilter(t *testing.T) {
	f := newOverviewFixture(t)
	expA := f.createExperiment(t, f.env.ID, "env-a-flag",
		model.ExperimentReviewed, model.ExperimentRunning)
	expB := f.createExperiment(t, f.otherEnv.ID, "env-b-flag",
		model.ExperimentReviewed, model.ExperimentRunning, model.ExperimentPaused)
	f.audit(t, "flag.publish", f.env.ID, "flag-a")
	f.audit(t, "flag.publish", f.otherEnv.ID, "flag-b")
	f.requestApproval(t, f.owner2, f.env.ID, "flag.rollback", "flag-a")
	f.requestApproval(t, f.owner2, f.otherEnv.ID, "flag.rollback", "flag-b")

	resp := f.overview(t, "?env="+f.env.ID, f.owner1.Secret)
	if resp.Risks.Coverage.Total != 1 || resp.Risks.Coverage.Inspected != 1 {
		t.Fatalf("risks coverage = %+v", resp.Risks.Coverage)
	}
	if findRisk(resp.Risks.Items, expA.ID, "experiment_unhealthy") == nil {
		t.Fatalf("env A risk missing: %+v", resp.Risks.Items)
	}
	if findRisk(resp.Risks.Items, expB.ID, "experiment_paused") != nil {
		t.Fatalf("env B leaked into risks: %+v", resp.Risks.Items)
	}
	if resp.Counts.RunningExperiments == nil || *resp.Counts.RunningExperiments != 1 ||
		resp.Counts.PausedExperiments == nil || *resp.Counts.PausedExperiments != 0 {
		t.Fatalf("counts = %+v", resp.Counts)
	}
	if len(resp.RecentReleases.Items) != 1 ||
		resp.RecentReleases.Items[0].Object != "flag-a" ||
		resp.RecentReleases.Items[0].EnvironmentID != f.env.ID {
		t.Fatalf("releases = %+v", resp.RecentReleases.Items)
	}
	// Approvals are workspace-level: both environments' grants are listed
	// and each carries its environmentId.
	if len(resp.PendingApprovals.Items) != 2 || resp.PendingApprovals.ActionableTotal != 2 {
		t.Fatalf("pending = %+v", resp.PendingApprovals)
	}
	envs := map[string]bool{}
	for _, item := range resp.PendingApprovals.Items {
		envs[item.EnvironmentID] = true
		if item.MyAction != "review" {
			t.Fatalf("pending item = %+v", item)
		}
	}
	if !envs[f.env.ID] || !envs[f.otherEnv.ID] {
		t.Fatalf("pending envs = %v", envs)
	}
	if resp.Counts.PendingApprovals == nil || *resp.Counts.PendingApprovals != 2 {
		t.Fatalf("submitted count = %+v", resp.Counts.PendingApprovals)
	}
}

// Matrix 8: the release whitelist, newest-first order, limit truncation and
// the bounded audit scan window.
func TestWorkspaceOverviewReleaseWhitelistAndScanWindow(t *testing.T) {
	f := newOverviewFixture(t)
	for _, seed := range []struct{ action, object string }{
		{"flag.draft.save", "flag-noise"},     // not whitelisted
		{"experiment.create", "exp-noise"},    // not whitelisted
		{"flag.publish", "flag-first"},        // engine republish path also lands here
		{"segment.publish", "segment-a"},      //
		{"release_plan.apply_ratio", "exp-1"}, // engine/manual rollout step
		{"flag.rollback", "flag-second"},      //
	} {
		f.audit(t, seed.action, f.env.ID, seed.object)
	}
	resp := f.overview(t, "", f.owner1.Secret)
	want := []struct{ action, object string }{
		{"flag.rollback", "flag-second"},
		{"release_plan.apply_ratio", "exp-1"},
		{"segment.publish", "segment-a"},
		{"flag.publish", "flag-first"},
	}
	items := resp.RecentReleases.Items
	if len(items) != len(want) {
		t.Fatalf("release items = %+v", items)
	}
	for i, w := range want {
		if items[i].Action != w.action || items[i].Object != w.object {
			t.Fatalf("release item %d = %+v, want %v", i, items[i], w)
		}
	}
	if !resp.RecentReleases.Coverage.Complete || resp.RecentReleases.Coverage.Note != "audit-scan-window" {
		t.Fatalf("coverage = %+v", resp.RecentReleases.Coverage)
	}
	resp = f.overview(t, "?limit=2", f.owner1.Secret)
	if len(resp.RecentReleases.Items) != 2 || resp.RecentReleases.Items[0].Object != "flag-second" {
		t.Fatalf("limited items = %+v", resp.RecentReleases.Items)
	}

	// Push the scan window past its bound with non-whitelisted noise: the
	// module reports incomplete coverage instead of inventing a total.
	for i := 0; i < overviewAuditScanLimit; i++ {
		f.audit(t, "flag.draft.save", f.env.ID, fmt.Sprintf("noise-%d", i))
	}
	resp = f.overview(t, "", f.owner1.Secret)
	cov := resp.RecentReleases.Coverage
	if cov.Inspected != overviewAuditScanLimit || cov.Total != overviewAuditScanLimit {
		t.Fatalf("scan window coverage = %+v", cov)
	}
	if cov.Complete || cov.Note != "audit-scan-window" {
		t.Fatalf("full window must be incomplete: %+v", cov)
	}
	if len(resp.RecentReleases.Items) != 0 {
		t.Fatalf("older releases must fall out of the window: %+v", resp.RecentReleases.Items)
	}
}

// Matrix 9: pure-function degradation without touching the store.
func TestBuildOverviewRisksDegradation(t *testing.T) {
	now := time.Now()
	running := &platform.Experiment{
		ID: "exp-1", EnvironmentID: "env-1", State: model.ExperimentRunning, UpdatedAt: now,
	}
	mustNotBuild := func(string, time.Time) (*experiment.Report, error) {
		t.Fatal("report built when it must not be")
		return nil, nil
	}

	// No report.read → restricted: only state/plan facts, never a health claim.
	mod := buildOverviewRisks(admin.Actor{}, []*platform.Experiment{running}, false, now, mustNotBuild)
	if mod.Status != "restricted" || mod.ReportCoverage != "restricted" {
		t.Fatalf("restricted module = %+v", mod)
	}
	if len(mod.Items) != 0 {
		t.Fatalf("restricted items = %+v", mod.Items)
	}
	if mod.Coverage.Inspected != 1 || mod.Coverage.Total != 1 || !mod.Coverage.Complete {
		t.Fatalf("restricted coverage = %+v", mod.Coverage)
	}

	// Injected report failure → explicit risk item, partial coverage.
	boom := errors.New("aggregates unavailable")
	mod = buildOverviewRisks(admin.Actor{}, []*platform.Experiment{running}, true, now,
		func(string, time.Time) (*experiment.Report, error) { return nil, boom })
	if mod.Status != "partial" || mod.ReportCoverage != "partial" || mod.Coverage.Complete {
		t.Fatalf("failed-report module = %+v", mod)
	}
	if len(mod.Items) != 1 || mod.Items[0].Kind != "experiment_report_unavailable" ||
		mod.Items[0].Severity != "warn" || mod.Items[0].Text != boom.Error() {
		t.Fatalf("failed-report items = %+v", mod.Items)
	}

	// Unhealthy report → block item quoting the server verbatim.
	mod = buildOverviewRisks(admin.Actor{}, []*platform.Experiment{running}, true, now,
		func(string, time.Time) (*experiment.Report, error) {
			return &experiment.Report{Healthy: false,
				HealthIssues: []string{"no randomized exposures recorded yet"}}, nil
		})
	if mod.Status != "ok" || mod.ReportCoverage != "complete" || !mod.Coverage.Complete {
		t.Fatalf("unhealthy module = %+v", mod)
	}
	if len(mod.Items) != 1 || mod.Items[0].Kind != "experiment_unhealthy" ||
		mod.Items[0].Severity != "block" ||
		mod.Items[0].Text != "no randomized exposures recorded yet" {
		t.Fatalf("unhealthy items = %+v", mod.Items)
	}

	// Healthy report → no items, no false alarms.
	mod = buildOverviewRisks(admin.Actor{}, []*platform.Experiment{running}, true, now,
		func(string, time.Time) (*experiment.Report, error) {
			return &experiment.Report{Healthy: true}, nil
		})
	if len(mod.Items) != 0 || mod.Status != "ok" {
		t.Fatalf("healthy module = %+v", mod)
	}

	// Plan states are quoted from the record's PausedReason.
	planned := []*platform.Experiment{
		{ID: "exp-2", EnvironmentID: "env-1", State: model.ExperimentRunning, UpdatedAt: now,
			ReleasePlan: &platform.ReleasePlan{State: platform.ReleasePlanPaused, PausedReason: "srm alarm"}},
		{ID: "exp-3", EnvironmentID: "env-1", State: model.ExperimentStopped, UpdatedAt: now,
			ReleasePlan: &platform.ReleasePlan{State: platform.ReleasePlanRolledBack, PausedReason: "release plan rollback: guardrail breach"}},
	}
	mod = buildOverviewRisks(admin.Actor{}, planned, false, now, mustNotBuild)
	planPaused := findRisk(mod.Items, "exp-2", "plan_paused")
	rolledBack := findRisk(mod.Items, "exp-3", "plan_rolled_back")
	if planPaused == nil || planPaused.Severity != "warn" || planPaused.Text != "srm alarm" {
		t.Fatalf("plan paused item = %+v", planPaused)
	}
	if rolledBack == nil || rolledBack.Severity != "block" ||
		rolledBack.Text != "release plan rollback: guardrail breach" {
		t.Fatalf("plan rolled back item = %+v", rolledBack)
	}

	// Inspection is bounded to the 50 most recently updated experiments and
	// coverage discloses the truncation.
	many := make([]*platform.Experiment, overviewRiskInspectMax+10)
	for i := range many {
		many[i] = &platform.Experiment{
			ID: fmt.Sprintf("exp-%02d", i), EnvironmentID: "env-1",
			State: model.ExperimentPaused, UpdatedAt: now.Add(time.Duration(i) * time.Minute),
		}
	}
	mod = buildOverviewRisks(admin.Actor{}, many, false, now, mustNotBuild)
	if mod.Coverage.Inspected != overviewRiskInspectMax ||
		mod.Coverage.Total != overviewRiskInspectMax+10 || mod.Coverage.Complete {
		t.Fatalf("bounded coverage = %+v", mod.Coverage)
	}
	if len(mod.Items) != overviewRiskInspectMax {
		t.Fatalf("bounded items = %d", len(mod.Items))
	}
	seen := map[string]bool{}
	for _, item := range mod.Items {
		seen[item.ID] = true
	}
	if seen["exp-00"] || !seen["exp-10"] || !seen["exp-59"] {
		t.Fatalf("stale experiments must drop out first: %v", seen)
	}
}

// Matrix 9: pending rules — self-submitted grants stay reviewable, plus
// expiry, break-glass, role gates and the untruncated actionable total.
func TestBuildOverviewPendingRules(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	actor := admin.Actor{TokenID: "tok-reviewer", Name: "reviewer"}
	submitted := func(id, tokenID, name string) *platform.Approval {
		return &platform.Approval{
			ID: id, Action: "flag.rollback", Object: "flag-a", EnvironmentID: "env-1",
			State: platform.DraftSubmitted, SubmittedBy: name, SubmittedTokenID: tokenID,
			SubmittedAt: now, Risk: platform.RiskHigh,
		}
	}
	approved := func(id, action string, expiresAt *time.Time) *platform.Approval {
		return &platform.Approval{
			ID: id, Action: action, Object: "flag-a", EnvironmentID: "env-1",
			State: platform.DraftApproved, SubmittedBy: "submitter", SubmittedTokenID: "tok-submit",
			SubmittedAt: now, Risk: platform.RiskHigh, ExpiresAt: expiresAt,
		}
	}
	cases := []struct {
		name                          string
		approval                      *platform.Approval
		canApprove, canPublish        bool
		wantAction, wantBlockedReason string
	}{
		{"submitted other can approve", submitted("a1", "tok-submit", "submitter"), true, false, "review", ""},
		{"submitted self by token id is reviewable", submitted("a2", "tok-reviewer", "renamed"), true, false, "review", ""},
		{"submitted self by name is reviewable", submitted("a3", "tok-other", "reviewer"), true, false, "review", ""},
		{"submitted no approve right", submitted("a4", "tok-submit", "submitter"), false, false, "", "role"},
		{"approved executable", approved("a5", "flag.rollback", &future), false, true, "execute", ""},
		{"approved expired", approved("a6", "flag.rollback", &past), false, true, "", "expired"},
		{"approved without expiry is unusable", approved("a7", "flag.rollback", nil), false, true, "", "expired"},
		{"breakglass review not executable", approved("a8", "breakglass.review", &future), false, true, "", "breakglass_review"},
		{"approved no publish right", approved("a9", "flag.rollback", &future), false, false, "", "role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mod := buildOverviewPending(actor, []*platform.Approval{tc.approval}, tc.canApprove, tc.canPublish, now, 20)
			if len(mod.Items) != 1 {
				t.Fatalf("items = %+v", mod.Items)
			}
			item := mod.Items[0]
			if item.MyAction != tc.wantAction || item.BlockedReason != tc.wantBlockedReason {
				t.Fatalf("myAction=%q blockedReason=%q, want %q/%q",
					item.MyAction, item.BlockedReason, tc.wantAction, tc.wantBlockedReason)
			}
			wantTotal := 0
			if tc.wantAction != "" {
				wantTotal = 1
			}
			if mod.ActionableTotal != wantTotal {
				t.Fatalf("actionableTotal = %d", mod.ActionableTotal)
			}
		})
	}

	// Terminal grants are history, not pending work.
	history := []*platform.Approval{
		{ID: "h1", State: platform.ApprovalRejected, SubmittedBy: "s", SubmittedTokenID: "t"},
		{ID: "h2", State: platform.ApprovalUsed, SubmittedBy: "s", SubmittedTokenID: "t"},
	}
	if mod := buildOverviewPending(actor, history, true, true, now, 20); len(mod.Items) != 0 || mod.ActionableTotal != 0 {
		t.Fatalf("history leaked into pending: %+v", mod)
	}

	// actionableTotal counts the full scan; only Items is truncated.
	many := make([]*platform.Approval, 0, 30)
	for i := range 30 {
		many = append(many, submitted(fmt.Sprintf("a-%02d", i), "tok-submit", "submitter"))
	}
	mod := buildOverviewPending(actor, many, true, false, now, 5)
	if mod.ActionableTotal != 30 {
		t.Fatalf("actionableTotal = %d, want the untruncated 30", mod.ActionableTotal)
	}
	if len(mod.Items) != 5 {
		t.Fatalf("items must truncate to limit: %d", len(mod.Items))
	}
}

// Matrix 9: release whitelist, env narrowing, limit truncation and honest
// scan-window coverage at the pure-function level.
func TestBuildOverviewReleasesFilter(t *testing.T) {
	base := time.Now()
	entry := func(id int64, action, envID, object string) *platform.AuditEntry {
		return &platform.AuditEntry{
			ID: id, Time: base.Add(time.Duration(id) * time.Minute), Actor: "operator",
			Action: action, EnvironmentID: envID, Object: object,
		}
	}
	entries := []*platform.AuditEntry{ // newest first, as AuditPage returns
		entry(5, "flag.rollback", "env-1", "flag-a"),
		entry(4, "experiment.create", "env-1", "exp-1"),
		entry(3, "release_plan.apply_ratio", "env-2", "exp-2"),
		entry(2, "flag.publish", "env-1", "flag-b"),
	}
	mod := buildOverviewReleases(entries, "", 10)
	if mod.Status != "ok" || len(mod.Items) != 3 {
		t.Fatalf("whitelist filter = %+v", mod)
	}
	if mod.Items[0].Action != "flag.rollback" || mod.Items[1].Action != "release_plan.apply_ratio" ||
		mod.Items[2].Action != "flag.publish" {
		t.Fatalf("order = %+v", mod.Items)
	}
	if mod.Coverage.Inspected != 4 || mod.Coverage.Total != 4 || !mod.Coverage.Complete ||
		mod.Coverage.Note != "audit-scan-window" {
		t.Fatalf("coverage = %+v", mod.Coverage)
	}
	mod = buildOverviewReleases(entries, "env-1", 10)
	if len(mod.Items) != 2 || mod.Items[0].Object != "flag-a" || mod.Items[1].Object != "flag-b" {
		t.Fatalf("env filter = %+v", mod.Items)
	}
	mod = buildOverviewReleases(entries, "", 1)
	if len(mod.Items) != 1 || mod.Items[0].Action != "flag.rollback" {
		t.Fatalf("limit truncation = %+v", mod.Items)
	}
	if mod.Coverage.Inspected != 4 {
		t.Fatalf("coverage must reflect the scan, not the items: %+v", mod.Coverage)
	}
	full := make([]*platform.AuditEntry, overviewAuditScanLimit)
	for i := range full {
		full[i] = entry(int64(i), "flag.publish", "env-1", "flag-x")
	}
	if mod = buildOverviewReleases(full, "", 20); mod.Coverage.Complete {
		t.Fatalf("a full scan window must be incomplete: %+v", mod.Coverage)
	}
	if mod = buildOverviewReleases(full[:overviewAuditScanLimit-1], "", 20); !mod.Coverage.Complete {
		t.Fatalf("a partial scan window must be complete: %+v", mod.Coverage)
	}
}
