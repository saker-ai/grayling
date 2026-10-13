package platform

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testMetricDefinition(workspaceID string) *MetricDefinition {
	return &MetricDefinition{
		WorkspaceID:       workspaceID,
		Key:               "activation.complete",
		DisplayName:       "Activation completion",
		Owner:             "growth-team",
		Source:            "collect",
		EventKeys:         []string{"activation_complete"},
		DedupUnit:         "user",
		AttributionWindow: 7 * 24 * time.Hour,
		Spec:              MetricSpec{Type: "proportion", Direction: "increase"},
		CreatedBy:         "analyst",
	}
}

func TestMetricDefinitionVersionAndImmutability(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("metrics", "Metrics")
	if err != nil {
		t.Fatal(err)
	}
	input := testMetricDefinition(ws.ID)
	first, err := store.CreateMetricDefinition(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || first.Status != MetricDraft || first.Hash == "" || first.Spec.Key != first.EventKeys[0] {
		t.Fatalf("unexpected first version: %+v", first)
	}
	input.EventKeys[0] = "changed_after_create"
	first.EventKeys[0] = "changed_response"
	stored, err := store.GetMetricDefinition(ws.ID, MetricRef{Key: first.Key, Version: 1})
	if err != nil || stored.EventKeys[0] != "activation_complete" {
		t.Fatalf("store contract mutated: %+v, %v", stored, err)
	}
	published, err := store.PublishMetricDefinition(ws.ID, MetricRef{Key: first.Key, Version: 1}, "reviewer")
	if err != nil || published.Status != MetricPublished || published.Hash != stored.Hash {
		t.Fatalf("publish changed contract: %+v, %v", published, err)
	}
	if _, err := store.PublishMetricDefinition(ws.ID, MetricRef{Key: first.Key, Version: 1}, "reviewer"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second publish error = %v", err)
	}
	deprecated, err := store.DeprecateMetricDefinition(ws.ID, MetricRef{Key: first.Key, Version: 1})
	if err != nil || deprecated.Status != MetricDeprecated || deprecated.Hash != first.Hash {
		t.Fatalf("deprecate changed contract: %+v, %v", deprecated, err)
	}
	second, err := store.CreateMetricDefinition(testMetricDefinition(ws.ID))
	if err != nil || second.Version != 2 {
		t.Fatalf("next version: %+v, %v", second, err)
	}
	if _, err := store.CreateMetricDefinition(&MetricDefinition{WorkspaceID: ws.ID, Key: "bad key"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid definition error = %v", err)
	}
	if defs := store.ListMetricDefinitions(ws.ID); len(defs) != 2 || defs[0].Version != 2 {
		t.Fatalf("list definitions: %+v", defs)
	}
}

func TestMetricDefinitionSurvivesReopen(t *testing.T) {
	tests := []struct {
		name string
		open func(string) (*Store, error)
	}{
		{"json", NewStore},
		{"sqlite", func(path string) (*Store, error) { return NewSQLiteStore(t.Context(), path) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metrics.db")
			store, err := tt.open(path)
			if err != nil {
				t.Fatal(err)
			}
			ws, err := store.CreateWorkspace("metrics", "Metrics")
			if err != nil {
				t.Fatal(err)
			}
			created, err := store.CreateMetricDefinition(testMetricDefinition(ws.ID))
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.PublishMetricDefinition(ws.ID, MetricRef{Key: created.Key, Version: created.Version}, "reviewer")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := tt.open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got, err := reopened.GetMetricDefinition(ws.ID, MetricRef{Key: created.Key, Version: created.Version})
			if err != nil || got.Status != MetricPublished || got.Hash != created.Hash {
				t.Fatalf("reopened definition: %+v, %v", got, err)
			}
		})
	}
}

func TestExperimentFreezesPublishedMetricReferences(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("metrics", "Metrics")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	create := func(key, event string) MetricRef {
		t.Helper()
		def := testMetricDefinition(ws.ID)
		def.Key, def.EventKeys = key, []string{event}
		created, err := store.CreateMetricDefinition(def)
		if err != nil {
			t.Fatal(err)
		}
		ref := MetricRef{Key: key, Version: created.Version}
		if _, err := store.PublishMetricDefinition(ws.ID, ref, "reviewer"); err != nil {
			t.Fatal(err)
		}
		return ref
	}
	primary := create("activation.complete", "activation_complete")
	guard := create("guardrail.crash", "crash_event")
	short := baseP5Experiment(ws.ID, env.ID, MetricSpec{})
	short.PrimaryMetricRef = &primary
	if _, err := store.CreateExperiment(short); !errors.Is(err, ErrValidation) {
		t.Fatalf("short analysis window accepted for catalog metric: %v", err)
	}
	exp := baseP5Experiment(ws.ID, env.ID, MetricSpec{})
	exp.AnalysisWindow = 7 * 24 * time.Hour
	exp.PrimaryMetricRef = &primary
	exp.Guardrails = []GuardrailSpec{{MetricRef: &guard, MaxRelativeRegression: 0.05, MinSample: 10}}
	created, err := store.CreateExperiment(exp)
	if err != nil {
		t.Fatal(err)
	}
	if created.PrimaryMetric.Key != "activation_complete" || created.PrimaryMetricRef.DefinitionHash == "" ||
		created.Guardrails[0].MetricKey != "crash_event" || created.Guardrails[0].MetricRef.DefinitionHash == "" {
		t.Fatalf("metric reference not frozen: %+v", created)
	}
	version := created.RegistrationVersion
	if err := store.ValidateFrozenMetricRef(ws.ID, *created.PrimaryMetricRef); err != nil {
		t.Fatalf("frozen definition invalid: %v", err)
	}
	wrong := *created.PrimaryMetricRef
	wrong.DefinitionHash = "sha256:wrong"
	if err := store.ValidateFrozenMetricRef(ws.ID, wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched definition hash accepted: %v", err)
	}
	created.PrimaryMetricRef.Key = "tampered"
	created.Guardrails[0].MetricRef.Key = "tampered"
	reread, err := store.GetExperiment(created.ID)
	if err != nil || reread.PrimaryMetricRef.Key != primary.Key || reread.Guardrails[0].MetricRef.Key != guard.Key || reread.RegistrationVersion != version {
		t.Fatalf("registration contract mutated: %+v, %v", reread, err)
	}
	if _, err := store.DeprecateMetricDefinition(ws.ID, primary); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateFrozenMetricRef(ws.ID, *reread.PrimaryMetricRef); err != nil {
		t.Fatalf("deprecated definition must remain reportable: %v", err)
	}
	newExperiment := baseP5Experiment(ws.ID, env.ID, MetricSpec{})
	newExperiment.AnalysisWindow = 7 * 24 * time.Hour
	newExperiment.Epoch = 2
	newExperiment.PrimaryMetricRef = &primary
	if _, err := store.CreateExperiment(newExperiment); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("deprecated version accepted: %v", err)
	}
}
