package platform

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godeps/grayling/pkg/model"
	"github.com/godeps/grayling/test/pgtest"
)

func TestPGBanditEventProvenanceAndReplay(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openPGStore(t, dsn)
	ws, err := a.CreateWorkspace("bandit", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.CreateEnvironment(ws.ID, "prod", "")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := a.CreateExperiment(&Experiment{WorkspaceID: ws.ID, EnvironmentID: env.ID,
		FlagKey: "f", Epoch: 1, Unit: "user_id", Variants: []string{"a", "b"},
		Proportions: map[string]float64{"a": 50, "b": 50}, AllocationMode: "bandit",
		Bandit: &BanditSpec{PolicyVersion: "p1", Strategy: "epsilon-greedy", PriorAlpha: 1, PriorBeta: 1,
			Epsilon: 0.2, FeedbackDelay: 73 * time.Hour},
		PrimaryMetric:  MetricSpec{Key: "reward", Type: "proportion", Direction: "increase"},
		AnalysisMethod: "bandit-beta-bernoulli", AnalysisWindow: time.Hour, Alpha: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.TransitionExperiment(exp.ID, model.ExperimentReviewed, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.TransitionExperiment(exp.ID, model.ExperimentRunning, "test"); err != nil {
		t.Fatal(err)
	}
	ev := &Event{EventID: "bandit-provenance", WorkspaceID: ws.ID, EnvironmentID: env.ID,
		ExperimentID: exp.ID, Epoch: exp.Epoch, Type: EventExposure, Variant: "b", UnitHash: a.HashUnit(ws.ID, "u"),
		Source: "bandit", PolicyVersion: "p1", StateVersion: 41, AllocationProbability: 0.9,
		ClientTime: time.Now().UTC()}
	res, err := a.IngestEvents([]*Event{ev})
	if err != nil || res.Accepted != 1 {
		t.Fatalf("ingest: %+v %v", res, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := openPGStore(t, dsn)
	replayed, err := b.BanditEvents(exp, time.Now().Add(time.Minute))
	if err != nil || len(replayed) != 1 {
		t.Fatalf("replay: %+v %v", replayed, err)
	}
	got := replayed[0]
	if got.PolicyVersion != "p1" || got.StateVersion != 41 || got.AllocationProbability != 0.9 || got.Source != "bandit" {
		t.Fatalf("lost assignment provenance: %+v", got)
	}
	pin, err := b.PinBanditAssignment(exp, &Event{ExperimentID: exp.ID, Epoch: exp.Epoch,
		UnitHash: got.UnitHash, Variant: "a", Source: "bandit", PolicyVersion: "p1",
		StateVersion: 99, AllocationProbability: 0.1})
	if err != nil || pin.Variant != "b" || pin.StateVersion != 41 || pin.AllocationProbability != 0.9 {
		t.Fatalf("event ingestion did not pin first assignment: %+v %v", pin, err)
	}
	if err := b.RotateHMACKey(strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	rotated, err := b.PinBanditAssignment(exp, &Event{ExperimentID: exp.ID, Epoch: exp.Epoch,
		UnitHash: b.HashUnit(ws.ID, "u"), Variant: "a", Source: "bandit", PolicyVersion: "p1",
		StateVersion: 99, AllocationProbability: 0.1})
	if err != nil || rotated.Variant != "b" || rotated.StateVersion != 41 {
		t.Fatalf("key rotation lost pin: %+v %v", rotated, err)
	}
}

func TestPGBanditConcurrentPinAcrossReplicas(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openPGStore(t, dsn)
	b := openPGStore(t, dsn)
	exp := &Experiment{ID: "e", Epoch: 1}
	left := &Event{ExperimentID: exp.ID, Epoch: exp.Epoch, UnitHash: "same-unit", Variant: "a",
		Source: "bandit", PolicyVersion: "p1", StateVersion: 1, AllocationProbability: 0.2}
	right := &Event{ExperimentID: exp.ID, Epoch: exp.Epoch, UnitHash: "same-unit", Variant: "b",
		Source: "bandit", PolicyVersion: "p1", StateVersion: 2, AllocationProbability: 0.8}
	var got [2]*Event
	var errs [2]error
	var wg sync.WaitGroup
	wg.Go(func() { got[0], errs[0] = a.PinBanditAssignment(exp, left) })
	wg.Go(func() { got[1], errs[1] = b.PinBanditAssignment(exp, right) })
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || got[0].Variant != got[1].Variant ||
		got[0].StateVersion != got[1].StateVersion || got[0].AllocationProbability != got[1].AllocationProbability {
		t.Fatalf("replicas disagree: %+v/%v vs %+v/%v", got[0], errs[0], got[1], errs[1])
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	c := openPGStore(t, dsn)
	after, err := c.PinBanditAssignment(exp, right)
	if err != nil || after.Variant != got[0].Variant || after.StateVersion != got[0].StateVersion {
		t.Fatalf("restart lost pin: %+v %v", after, err)
	}
}
