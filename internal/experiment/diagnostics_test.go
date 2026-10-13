package experiment

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

func TestDiagnosticsSeparatesAssignmentAndExposureSRM(t *testing.T) {
	now := time.Now().UTC()
	exp := &platform.Experiment{
		ID: "experiment", Epoch: 1, ExposureMode: "onRender",
		Variants:    []string{"control", "treatment"},
		Proportions: map[string]float64{"control": 50, "treatment": 50},
	}
	window := platform.AnalysisWindowStatus{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	var events []*platform.Event
	for _, variant := range exp.Variants {
		for i := range 100 {
			decision := fmt.Sprintf("%s-%d", variant, i)
			unit := fmt.Sprintf("private-%s-%d", variant, i)
			assignment := &platform.Event{
				EventID: "asg-" + decision, Type: platform.EventAssignment,
				Variant: variant, UnitHash: unit, Source: "split", FlagKey: "flag",
				FlagRevision: 1, ClientTime: now, ServerTime: now,
			}
			events = append(events, assignment)
			limit := 100
			if variant == "treatment" {
				limit = 20
			}
			if i < limit {
				exposure := *assignment
				exposure.EventID = "exp-" + decision
				exposure.Type = platform.EventExposure
				events = append(events, &exposure)
			}
		}
	}
	d := buildDiagnostics(exp, events, window, now)
	if d.AssignmentSRM == nil || !d.AssignmentSRM.Healthy {
		t.Fatalf("balanced assignments must be healthy: %+v", d.AssignmentSRM)
	}
	if d.ExposureSRM == nil || d.ExposureSRM.Healthy {
		t.Fatalf("skewed delivery must alarm: %+v", d.ExposureSRM)
	}
	if d.PerVariant[0].Unexposed != 0 || d.PerVariant[1].Unexposed != 80 {
		t.Fatalf("unexpected attrition: %+v", d.PerVariant)
	}
	if d.AssignmentMismatch != 0 || d.CrossVariantUnits != 0 || len(d.Issues) == 0 {
		t.Fatalf("unexpected quality diagnosis: %+v", d)
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "asg-") {
		t.Fatal("aggregate diagnostics leaked unit or decision identifiers")
	}
}

func TestDiagnosticsDetectsCrossVariantAndAssignmentMismatch(t *testing.T) {
	now := time.Now().UTC()
	exp := &platform.Experiment{
		ID: "experiment", Epoch: 1, ExposureMode: "onRender",
		Variants:    []string{"control", "treatment"},
		Proportions: map[string]float64{"control": 50, "treatment": 50},
	}
	window := platform.AnalysisWindowStatus{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	events := []*platform.Event{
		{EventID: "asg-one", Type: platform.EventAssignment, Variant: "control",
			UnitHash: "private-unit", FlagKey: "flag", FlagRevision: 1, ClientTime: now},
		{EventID: "exp-one", Type: platform.EventExposure, Variant: "control",
			UnitHash: "private-unit", FlagKey: "flag", FlagRevision: 1, ClientTime: now},
		{EventID: "exp-two", Type: platform.EventExposure, Variant: "treatment",
			UnitHash: "private-unit", FlagKey: "flag", FlagRevision: 1, ClientTime: now},
	}
	d := buildDiagnostics(exp, events, window, now)
	if d.CrossVariantUnits != 1 || d.AssignmentMismatch != 1 || len(d.Issues) != 2 {
		t.Fatalf("pollution and mismatch must be separate blockers: %+v", d)
	}
}

func TestOnRenderReportHoldsForUnexposedAssignments(t *testing.T) {
	store, ws, env := setup(t)
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "flag", Epoch: 1,
		Unit: "user_id", ExposureMode: "onRender",
		Variants:    []string{"control", "treatment"},
		Proportions: map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: platform.MetricSpec{
			Key: "conversion", Type: "proportion", Direction: "increase",
		},
		Alpha: 0.05, AnalysisWindow: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionExperiment(exp.ID, model.ExperimentReviewed, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionExperiment(exp.ID, model.ExperimentRunning, "test"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	assignment := &platform.Event{
		EventID: "asg-unseen", WorkspaceID: ws.ID, EnvironmentID: env.ID,
		ExperimentID: exp.ID, Epoch: exp.Epoch, Type: platform.EventAssignment,
		FlagKey: exp.FlagKey, FlagRevision: 1, Variant: "control",
		UnitHash: store.HashUnit(ws.ID, "private-user"), Source: "split",
		ClientTime: now, ServerTime: now,
	}
	if result, err := store.IngestEvents([]*platform.Event{assignment}); err != nil || result.Accepted != 1 {
		t.Fatalf("assignment ingest: %+v, %v", result, err)
	}
	report, err := BuildReport(store, exp.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if report.Diagnostics == nil || report.Diagnostics.PerVariant[0].Unexposed != 1 {
		t.Fatalf("report lost delivery gap: %+v", report.Diagnostics)
	}
	if report.Healthy || report.Decision.Verdict != "hold" {
		t.Fatalf("unreviewed delivery gap must hold: healthy=%t verdict=%s", report.Healthy, report.Decision.Verdict)
	}
}

func TestDiagnosticsReplaysSQLiteAssignmentsAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.sqlite")
	store, err := platform.NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("diagnostics", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "flag", Epoch: 1,
		Unit: "user_id", ExposureMode: "onRender",
		Variants:    []string{"control", "treatment"},
		Proportions: map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: platform.MetricSpec{
			Key: "conversion", Type: "proportion", Direction: "increase",
		},
		Alpha: 0.05, AnalysisWindow: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionExperiment(exp.ID, model.ExperimentReviewed, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionExperiment(exp.ID, model.ExperimentRunning, "test"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	unit := store.HashUnit(ws.ID, "private-user")
	assignment := &platform.Event{
		EventID: "asg-sqlite", WorkspaceID: ws.ID, EnvironmentID: env.ID,
		ExperimentID: exp.ID, Epoch: exp.Epoch, Type: platform.EventAssignment,
		FlagKey: exp.FlagKey, FlagRevision: 1, Variant: "control",
		UnitHash: unit, Source: "split", ClientTime: now, ServerTime: now,
	}
	exposure := *assignment
	exposure.EventID = "exp-sqlite"
	exposure.Type = platform.EventExposure
	if result, err := store.IngestEvents([]*platform.Event{assignment, &exposure}); err != nil || result.Accepted != 2 {
		t.Fatalf("ingest: %+v, %v", result, err)
	}
	if err := store.RecordCollectionOutcomes(ws.ID, env.ID, []platform.CollectionOutcome{
		{ExperimentID: exp.ID, Status: "rejected", Code: "stale_epoch"},
		{ExperimentID: exp.ID, Status: "duplicate", Code: "duplicate_event"},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := BuildDiagnostics(store, exp.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := platform.NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := BuildDiagnostics(reopened, exp.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if before.PerVariant[0] != after.PerVariant[0] ||
		after.PerVariant[0].Assignments != 1 || after.PerVariant[0].Exposures != 1 ||
		after.Window.Backend != "sqlite-raw" || after.RejectedByReason["stale_epoch"] != 1 ||
		after.DuplicateEvents != 1 {
		t.Fatalf("raw replay changed diagnostics: before=%+v after=%+v", before, after)
	}
}
