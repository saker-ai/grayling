package growthservice

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/api"
	"github.com/saker-ai/grayling/internal/growth"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

// The baseline report is explicitly synthetic; creation, RBAC, registration,
// review, binding and the linked empty-run report are the actual platform API.
func TestIntegratedPlatformLostResponseRestoreAndLearning(t *testing.T) {
	core, err := platform.NewStore(filepath.Join(privateDir(t), "platform.json"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := core.CreateWorkspace("synthetic-growth", "Synthetic growth")
	if err != nil {
		t.Fatal(err)
	}
	env, err := core.CreateEnvironment(ws.ID, "test", "Test")
	if err != nil {
		t.Fatal(err)
	}
	refs := []growth.ProposalMetric{}
	for _, key := range []string{"activation", "retention"} {
		m, e := core.CreateMetricDefinition(&platform.MetricDefinition{WorkspaceID: ws.ID, Key: key, DisplayName: key, Owner: "synthetic", Source: "collect", EventKeys: []string{key}, DedupUnit: "user", AttributionWindow: 24 * time.Hour, Spec: platform.MetricSpec{Type: "proportion", Direction: "increase"}, CreatedBy: "synthetic"})
		if e != nil {
			t.Fatal(e)
		}
		m, e = core.PublishMetricDefinition(ws.ID, platform.MetricRef{Key: key, Version: m.Version}, "synthetic")
		if e != nil {
			t.Fatal(e)
		}
		refs = append(refs, growth.ProposalMetric{Ref: growth.MetricRef{Key: key, Version: m.Version, DefinitionHash: m.Hash}, SourcePath: "/api/v1/admin/workspaces/" + ws.ID + "/metrics/" + key + "/versions/1"})
	}
	flag := &model.Flag{SchemaVersion: model.SchemaVersion, Key: "onboarding", Type: model.FlagBool, Fields: []string{"user_id"}, Bucketing: &model.Bucketing{Algorithm: "grayling-fnv1a-10000-v1", Unit: "user_id", Salt: "synthetic"}, Variations: map[string]any{"control": false, "treatment": true}, DefaultRule: &model.Rule{Variation: "control"}, Targeting: []model.Rule{{Percentage: map[string]float64{"control": 50, "treatment": 50}}}}
	if err = core.AppendFlagRevision(&platform.FlagRevision{WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: flag.Key, Revision: 7, Content: flag}); err != nil {
		t.Fatal(err)
	}
	owner, e := core.CreateToken(platform.TokenAdmin, "synthetic owner", platform.RoleOwner, ws.ID, "", "synthetic")
	if e != nil {
		t.Fatal(e)
	}
	viewer, e := core.CreateToken(platform.TokenAdmin, "synthetic reader", platform.RoleViewer, ws.ID, "", "synthetic")
	if e != nil {
		t.Fatal(e)
	}
	f := newFixture(t, func(f *fixture) {
		f.goal.WorkspaceID = ws.ID
		f.goal.EnvironmentID = env.ID
		f.goal.PrimaryMetric = refs[0]
		f.goal.Guardrails = refs[1:]
		f.proposal.WorkspaceID = ws.ID
		f.proposal.EnvironmentID = env.ID
		f.proposal.PrimaryMetric = refs[0]
		f.proposal.Guardrails = refs[1:]
		f.manifest.WorkspaceID = ws.ID
		f.manifest.EnvironmentID = env.ID
		f.manifest.PrimaryMetric = refs[0].Ref
		f.manifest.Guardrails = []growth.MetricRef{refs[1].Ref}
		f.report.PrimaryMetricRef = &refs[0].Ref
		f.report.GuardrailMetricRefs = []growth.MetricRef{refs[1].Ref}
	})
	actual := api.NewServer(core, nil, api.Options{}).Handler()
	var creates atomic.Int32
	var lose atomic.Bool
	lose.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/admin/experiments/exp1/report/export" && r.Method == "GET" {
			write(w, 200, map[string]any{"workspaceId": ws.ID, "environmentId": env.ID, "report": f.report})
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/experiments") {
			rec := httptest.NewRecorder()
			actual.ServeHTTP(rec, r)
			if rec.Code == 201 {
				creates.Add(1)
			}
			if rec.Code == 201 && lose.Swap(false) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
				return
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
			return
		}
		actual.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	cfg := f.service.cfg
	cfg.Evidence.BaseURL = upstream.URL
	cfg.Evidence.BearerToken = viewer.Secret
	f.service.Close()
	f.service, e = New(t.Context(), f.store, cfg)
	if e != nil {
		t.Fatal(e)
	}
	task := f.create(t)
	f.service.process(t.Context())
	task = f.load(t, task.ID)
	if task.State != "DIAGNOSED" {
		t.Fatalf("analysis: %+v", task)
	}
	candidate := f.proposal
	candidate.ID = task.ID
	candidate.Version = 2
	candidate.GoalPin = task.Proposal.GoalPin
	task = f.act(t, task, "edit", Action{Proposal: &candidate}, 200)
	task = f.act(t, task, "review", Action{Decision: growth.ReviewApprove, Rationale: "Synthetic reviewer tests readiness"}, 200)
	threshold := 0.05
	settings := growth.PreparationInputs{AllocationMode: "fixed-split", IntegrationOwner: "synthetic", Alpha: 0.05, Power: 0.8, AnalysisWindowHours: 336, Guardrails: []growth.GuardrailPreparation{{Ref: refs[1].Ref, MaxRelativeRegression: &threshold, MinSample: 100}}}
	task = f.act(t, task, "prepare", Action{Settings: &settings}, 200)
	send := func(action, token string, expected int) {
		t.Helper()
		b, _ := json.Marshal(Action{Revision: task.Revision})
		r := httptest.NewRequest("POST", "/v1/tasks/"+task.ID+"/"+action, bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+reviewSecret)
		r.Header.Set("X-Grayling-Token", token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(w, r)
		if w.Code != expected {
			t.Fatalf("%s %d expected %d: %s", action, w.Code, expected, w.Body.String())
		}
		task = f.load(t, task.ID)
	}
	// A changed flag invalidates local preparation and requires a fresh review.
	if err = core.AppendFlagRevision(&platform.FlagRevision{WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: flag.Key, Revision: 8, Content: flag}); err != nil {
		t.Fatal(err)
	}
	send("submit", owner.Secret, 409)
	if task.State != "HUMAN_REVIEW" || task.Review != nil || task.Prepared != nil || len(core.ListExperiments(ws.ID, env.ID)) != 0 {
		t.Fatal("stale flag preparation survived")
	}
	if _, e = core.CreateExperimentAtRevision(&platform.Experiment{WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: flag.Key}, 7); !errors.Is(e, platform.ErrConflict) {
		t.Fatal("atomic store revision precondition missing")
	}
	task = f.act(t, task, "review", Action{Decision: growth.ReviewApprove, Rationale: "Synthetic fresh review after flag change"}, 200)
	task = f.act(t, task, "prepare", Action{Settings: &settings}, 200)
	staleBody := struct {
		growth.CreateRequest
		FlagRevision int64 `json:"flagRevision"`
	}{task.Prepared.Request, 7}
	raw, _ := json.Marshal(staleBody)
	req := httptest.NewRequest("POST", "/api/v1/admin/workspaces/"+ws.ID+"/environments/"+env.ID+"/experiments", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+owner.Secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	actual.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatal("admin revision precondition missing")
	}
	// Downstream RBAC denies an otherwise locally authorized reviewer.
	send("submit", viewer.Secret, 422)
	if len(core.ListExperiments(ws.ID, env.ID)) != 0 {
		t.Fatal("viewer created experiment")
	}
	// Lost response leaves durable SUBMITTING, not a new allocation on retry.
	send("submit", owner.Secret, 422)
	if task.State != "SUBMITTING" || task.ReservedEpoch != 1 || len(core.ListExperiments(ws.ID, env.ID)) != 1 {
		t.Fatalf("lost response checkpoint: %+v", task)
	}
	backup := filepath.Join(privateDir(t), "restore.sqlite")
	if e = f.store.Backup(t.Context(), backup); e != nil {
		t.Fatal(e)
	}
	restored, e := Open(t.Context(), backup)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	service, e := New(t.Context(), restored, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer service.Close()
	f.store = restored
	f.service = service
	send("submit", owner.Secret, 200)
	if task.State != "SUBMITTED" || task.Manifest == nil || creates.Load() != 1 {
		t.Fatalf("duplicate creation or failed reconciliation: state=%s manifest=%+v successful creates=%d", task.State, task.Manifest, creates.Load())
	}
	send("submit", owner.Secret, 200)
	if creates.Load() != 1 {
		t.Fatal("retry issued create")
	}
	send("platform-review", owner.Secret, 200)
	send("platform-review", owner.Secret, 200)
	send("bind", owner.Secret, 200)
	send("bind", owner.Secret, 200)
	if task.State != "BOUND" {
		t.Fatal(task.State)
	}
	stored, e := core.GetExperiment(task.Manifest.ExperimentID)
	if e != nil || stored.State != model.ExperimentScheduled || core.CurrentFlagRevision(ws.ID, env.ID, "onboarding") != 8 {
		t.Fatal("binding published or advanced traffic")
	}
	task = f.act(t, task, "monitor", Action{}, 200)
	if task.State != "HOLD" || task.Observation == nil || len(task.Observation.Blockers) == 0 {
		t.Fatalf("empty real report should HOLD: %+v", task)
	}
	task = f.act(t, task, "learning", Action{Followups: []string{"Synthetic empty-run failure: wait for authorized mature data"}}, 200)
	if task.Learning == nil || task.Learning.BusinessWinEstablished || task.Learning.PublicationAuthorized || task.Learning.Observation.Status != growth.Hold {
		t.Fatal("false success")
	}
	status, b := f.call(t, "GET", "/v1/learning?text=empty-run", viewSecret, nil, "")
	if status != 200 {
		t.Fatalf("search %d %s", status, b)
	}
	var search growth.LearningSearchResult
	if json.Unmarshal(b, &search) != nil || len(search.Matches) != 1 {
		t.Fatal("learning not searchable")
	}
	if err = restored.Verify(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(task)
	for _, secret := range []string{owner.Secret, viewer.Secret, reviewSecret} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("credential persisted")
		}
	}
	t.Log("actual platform RBAC/create/review/bind/report: passed; lost-response restore: one create; no flag publication; empty evidence HOLD persisted and searchable")
}
