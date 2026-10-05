package experiment

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/internal/stats"
	"github.com/godeps/golab/pkg/model"
	"github.com/godeps/golab/test/pgtest"
)

func TestJourneyReplayOrderDuplicatesSessionsAndWatermark(t *testing.T) {
	start := time.Date(2026, 3, 7, 5, 0, 0, 0, time.UTC)
	window := platform.AnalysisWindowStatus{Start: start, End: start.Add(72 * time.Hour), Watermark: start.Add(10 * 24 * time.Hour)}
	exp := &platform.Experiment{Variants: []string{"a", "b"}}
	ev := func(id, variant, unit, session, metric string, typ platform.EventType, at time.Time) *platform.Event {
		return &platform.Event{EventID: id, Variant: variant, UnitHash: unit, SessionID: session, MetricKey: metric, Type: typ, ClientTime: at, Source: "split"}
	}
	events := []*platform.Event{
		ev("late-step", "a", "u1", "s1", "pay", platform.EventConversion, start.Add(7*time.Hour)),
		ev("view", "a", "u1", "s1", "view", platform.EventConversion, start.Add(time.Hour)),
		ev("exposure", "a", "u1", "", "", platform.EventExposure, start),
		ev("duplicate-view", "a", "u1", "s1", "view", platform.EventConversion, start.Add(time.Hour)),
		ev("pay", "a", "u1", "s1", "pay", platform.EventConversion, start.Add(2*time.Hour)),
		ev("second-session-view", "a", "u1", "s2", "view", platform.EventConversion, start.Add(3*time.Hour)),
		ev("second-session-pay-too-late", "a", "u1", "s2", "pay", platform.EventConversion, start.Add(5*time.Hour)),
		ev("missing-session", "a", "u1", "", "view", platform.EventConversion, start.Add(time.Hour)),
		ev("before-exposure", "b", "u2", "s3", "view", platform.EventConversion, start.Add(30*time.Minute)),
		ev("b-exposure", "b", "u2", "", "", platform.EventExposure, start.Add(time.Hour)),
	}
	spec := &platform.FunnelSpec{Steps: []string{"view", "pay"}, DedupUnit: "session", StepTimeout: time.Hour + time.Second}
	got := buildJourney(exp, "funnel", "checkout", spec, nil, events, window)
	if a := got.Arms[0]; a.Denominator != 2 || a.Numerator != 1 || a.Excluded["missing_or_late_step"] != 1 || a.Excluded["missing_session"] != 1 {
		t.Fatalf("funnel a: %+v", a)
	}
	if b := got.Arms[1]; b.Denominator != 0 || b.Excluded["not_randomized_or_before_exposure"] != 1 {
		t.Fatalf("funnel b: %+v", b)
	}
	reversed := slices.Clone(events)
	slices.Reverse(reversed)
	again := buildJourney(exp, "funnel", "checkout", spec, nil, reversed, window)
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("replay changed with arrival order: %+v != %+v", got, again)
	}
}

func TestRetentionCalendarDayAndImmatureCohorts(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	local := time.Date(2026, 3, 7, 23, 30, 0, 0, loc)
	start := local.UTC().Add(-time.Hour)
	window := platform.AnalysisWindowStatus{Start: start, End: start.Add(48 * time.Hour), Watermark: local.AddDate(0, 0, 2).UTC()}
	exp := &platform.Experiment{Variants: []string{"a", "b"}}
	ev := func(id, unit, metric string, typ platform.EventType, at time.Time) *platform.Event {
		return &platform.Event{EventID: id, Variant: "a", UnitHash: unit, MetricKey: metric, Type: typ, ClientTime: at, Source: "split"}
	}
	events := []*platform.Event{
		ev("e1", "u1", "", platform.EventExposure, local.UTC()),
		ev("e2", "u2", "", platform.EventExposure, local.UTC()),
		ev("r1", "u1", "return", platform.EventConversion, local.AddDate(0, 0, 1).Add(-30*time.Minute).UTC()),
		ev("early", "u2", "return", platform.EventConversion, local.Add(-time.Hour).UTC()),
		ev("e3", "u3", "", platform.EventExposure, local.AddDate(0, 0, 1).UTC()),
	}
	spec := &platform.RetentionSpec{ReturnKey: "return", Day: 1, Timezone: "America/New_York"}
	got := buildJourney(exp, "retention", "d1", nil, spec, events, window)
	if a := got.Arms[0]; a.Denominator != 2 || a.Numerator != 1 || a.Immature != 1 || a.Excluded["no_return_in_window"] != 1 {
		t.Fatalf("retention a: %+v", a)
	}
	window.Watermark = local.AddDate(0, 0, 3).UTC()
	mature := buildJourney(exp, "retention", "d1", nil, spec, events, window)
	if a := mature.Arms[0]; a.Denominator != 3 || a.Numerator != 1 || a.Immature != 0 {
		t.Fatalf("mature retention: %+v", a)
	}
	reversed := slices.Clone(events)
	slices.Reverse(reversed)
	if replay := buildJourney(exp, "retention", "d1", nil, spec, reversed, window); !reflect.DeepEqual(mature, replay) {
		t.Fatalf("replay changed: %+v != %+v", mature, replay)
	}
}

func TestPostgresJourneyReportReplaysAcceptedEvents(t *testing.T) {
	dsn := pgtest.DSN(t)
	store, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ws, err := store.CreateWorkspace("journey", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: 1, Unit: "user_id",
		Variants: []string{"a", "b"}, Proportions: map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: platform.MetricSpec{Key: "checkout", Type: "funnel", Direction: "increase", Funnel: &platform.FunnelSpec{Steps: []string{"view", "pay"}, DedupUnit: "session", StepTimeout: time.Minute}},
		Guardrails:    []platform.GuardrailSpec{{MetricKey: "d1", Type: "retention", Direction: "increase", MinSample: 1, MaxRelativeRegression: .1, Retention: &platform.RetentionSpec{ReturnKey: "visit", Day: 1, Timezone: "UTC"}}},
		Alpha:         .05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.TransitionExperiment(exp.ID, model.ExperimentReviewed, "test"); err != nil {
		t.Fatal(err)
	}
	if exp, err = store.TransitionExperiment(exp.ID, model.ExperimentRunning, "test"); err != nil {
		t.Fatal(err)
	}
	start := *exp.StartedAt
	event := func(id, variant, unit, metric string, typ platform.EventType, offset time.Duration) *platform.Event {
		ev := &platform.Event{EventID: id, WorkspaceID: ws.ID, EnvironmentID: env.ID, ExperimentID: exp.ID, Epoch: 1, Variant: variant, UnitHash: unit, Type: typ, MetricKey: metric, ClientTime: start.Add(offset)}
		if typ == platform.EventExposure {
			ev.Source = "split"
		} else {
			ev.SessionID = "opaque-" + unit
		}
		return ev
	}
	events := []*platform.Event{
		event("a-pay", "a", "u1", "pay", platform.EventConversion, 3*time.Millisecond),
		event("a-view", "a", "u1", "view", platform.EventConversion, 2*time.Millisecond),
		event("a-exposure", "a", "u1", "", platform.EventExposure, time.Millisecond),
		event("b-exposure", "b", "u2", "", platform.EventExposure, time.Millisecond),
		event("b-view", "b", "u2", "view", platform.EventConversion, 2*time.Millisecond),
	}
	initial := []*platform.Event{events[0], events[2], events[3], events[4]}
	result, err := store.IngestEvents(initial)
	if err != nil || result.Accepted != len(initial) {
		t.Fatalf("ingest: %+v %v", result, err)
	}
	preliminary, err := BuildReport(store, exp.ID, start.Add(5*time.Minute))
	if err != nil || preliminary.Journeys[0].Arms[0].Denominator != 0 || preliminary.Healthy {
		t.Fatalf("pre-watermark report: %+v %v", preliminary, err)
	}
	result, err = store.IngestEvents([]*platform.Event{events[1]}) // arrives after pay, client time is earlier
	if err != nil || result.Accepted != 1 {
		t.Fatalf("late view: %+v %v", result, err)
	}
	stored, err := store.AnalysisEventSnapshot(exp, start.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range stored {
		if ev.SessionID != "" && (ev.SessionID == "opaque-"+ev.UnitHash || len(ev.SessionID) != 64) {
			t.Fatalf("raw session identifier persisted: %q", ev.SessionID)
		}
	}
	result, err = store.IngestEvents([]*platform.Event{events[0]})
	if err != nil || result.Duplicates != 1 {
		t.Fatalf("dedup: %+v %v", result, err)
	}
	at := start.Add(73 * time.Hour)
	first, err := BuildReport(store, exp.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Window.Complete || len(first.Journeys) != 2 || first.Journeys[0].Arms[0].Denominator != 1 || first.Journeys[0].Arms[0].Numerator != 1 || first.Journeys[0].Arms[1].Numerator != 0 {
		t.Fatalf("report: %+v", first)
	}
	if first.Journeys[1].Arms[0].Immature != 1 || first.Healthy || first.Decision.Verdict != stats.VerdictHold {
		t.Fatalf("immature guardrail passed decision gate: %+v", first)
	}
	if err := store.RotateHMACKey("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	reopened, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := BuildReport(reopened, exp.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Journeys, again.Journeys) {
		t.Fatalf("replay changed: %+v != %+v", first.Journeys, again.Journeys)
	}
}
