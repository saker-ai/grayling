package growth

import (
	"bytes"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/api"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

// Only this local fixture invokes the existing write handler. The compiler has
// no write transport. Synthetic READY evidence proves compatibility, not pilot
// acceptance or the health of a real Saker experiment.
func TestPreparedRequestWithCreateHandler(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("prepare", "Prepare")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "test", "Test")
	if err != nil {
		t.Fatal(err)
	}
	p, e := proposalFixture()
	p.WorkspaceID = ws.ID
	p.EnvironmentID = env.ID
	e.WorkspaceID = ws.ID
	e.EnvironmentID = env.ID
	for i, key := range []string{"activation", "retention"} {
		def, err := store.CreateMetricDefinition(&platform.MetricDefinition{WorkspaceID: ws.ID, Key: key, DisplayName: key, Owner: "fixture", Source: "collect", EventKeys: []string{key}, DedupUnit: "user", AttributionWindow: 24 * time.Hour, Spec: platform.MetricSpec{Type: "proportion", Direction: "increase"}, CreatedBy: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		def, err = store.PublishMetricDefinition(ws.ID, platform.MetricRef{Key: key, Version: def.Version}, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		ref := MetricRef{Key: key, Version: def.Version, DefinitionHash: def.Hash}
		path := "/api/v1/admin/workspaces/" + ws.ID + "/metrics/" + key + "/versions/" + strconv.FormatInt(def.Version, 10)
		e.Metrics[i] = Metric{WorkspaceID: ws.ID, Key: key, Version: def.Version, Hash: def.Hash, SourcePath: path}
		if i == 0 {
			p.PrimaryMetric = ProposalMetric{Ref: ref, SourcePath: path}
			e.Evidence[0].Report.PrimaryMetricRef = &ref
		} else {
			p.Guardrails[0] = ProposalMetric{Ref: ref, SourcePath: path}
			e.Evidence[0].Report.GuardrailMetricRefs = []MetricRef{ref}
		}
	}
	// Frozen v1 must survive a newer published primary definition.
	newer, err := store.CreateMetricDefinition(&platform.MetricDefinition{WorkspaceID: ws.ID, Key: "activation", DisplayName: "new definition", Owner: "fixture", Source: "collect", EventKeys: []string{"different-event"}, DedupUnit: "user", AttributionWindow: 24 * time.Hour, Spec: platform.MetricSpec{Type: "proportion", Direction: "decrease"}, CreatedBy: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.PublishMetricDefinition(ws.ID, platform.MetricRef{Key: newer.Key, Version: newer.Version}, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err = store.AppendFlagRevision(&platform.FlagRevision{WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: p.FlagKey, Revision: 7, Content: &model.Flag{Key: p.FlagKey, Variations: map[string]any{"control": false, "treatment": true}}}); err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateToken(platform.TokenAdmin, "fixture-owner", platform.RoleOwner, ws.ID, "", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := store.CreateToken(platform.TokenAdmin, "fixture-viewer", platform.RoleViewer, ws.ID, "", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := CompileCreateRequest(p, e, preparationFixture(p))
	if err != nil {
		t.Fatal(err)
	}
	handler := api.NewServer(store, nil, api.Options{}).Handler()
	post := func(request CreateRequest, secret string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(request, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/workspaces/"+ws.ID+"/environments/"+env.ID+"/experiments", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	denied := post(prepared.Request, viewer.Secret)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("viewer: %d %s", denied.Code, denied.Body)
	}
	rec := post(prepared.Request, owner.Secret)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created platform.Experiment
	if err = jsonv1.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	primary := platform.MetricRef{Key: p.PrimaryMetric.Ref.Key, Version: p.PrimaryMetric.Ref.Version, DefinitionHash: p.PrimaryMetric.Ref.DefinitionHash}
	guard := platform.MetricRef{Key: p.Guardrails[0].Ref.Key, Version: p.Guardrails[0].Ref.Version, DefinitionHash: p.Guardrails[0].Ref.DefinitionHash}
	if created.State != model.ExperimentDraft || created.ID == "" || created.FlagRevision != 7 || created.Epoch != 1 || created.RegistrationVersion == "" || created.PrimaryMetricRef == nil || *created.PrimaryMetricRef != primary || created.Guardrails[0].MetricRef == nil || *created.Guardrails[0].MetricRef != guard || created.PrimaryMetric.Direction != "increase" {
		t.Fatalf("registration: %+v", created)
	}
	frozen, err := store.GetExperiment(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(frozen.PrimaryMetricRef, created.PrimaryMetricRef) || !reflect.DeepEqual(frozen.Guardrails, created.Guardrails) {
		t.Fatal("references not frozen in store")
	}
	// Server validation remains authoritative even for syntactically valid pins.
	for _, tt := range []struct {
		name   string
		change func(*CreateRequest)
		status int
	}{
		{"hash", func(r *CreateRequest) { r.PrimaryMetricRef.DefinitionHash = hashA }, http.StatusConflict},
		{"version", func(r *CreateRequest) { r.PrimaryMetricRef.Version = 99 }, http.StatusNotFound},
		{"guard hash", func(r *CreateRequest) { r.Guardrails[0].MetricRef.DefinitionHash = hashA }, http.StatusConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fresh, err := CompileCreateRequest(p, e, preparationFixture(p))
			if err != nil {
				t.Fatal(err)
			}
			tt.change(&fresh.Request)
			rec := post(fresh.Request, owner.Secret)
			if rec.Code != tt.status {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	if _, err = store.DeprecateMetricDefinition(ws.ID, primary); err != nil {
		t.Fatal(err)
	}
	rec = post(prepared.Request, owner.Secret)
	if rec.Code != http.StatusConflict {
		t.Fatalf("deprecated: %d %s", rec.Code, rec.Body)
	}
}
