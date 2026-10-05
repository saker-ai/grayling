package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godeps/golab/pkg/model"
)

func snapshotJSON(t *testing.T, rev int64, flags map[string]*model.Flag) []byte {
	t.Helper()
	snap := model.Snapshot{
		SchemaVersion: model.SchemaVersion,
		Workspace:     "ws-1", Environment: "env-1",
		ManifestRevision: rev,
		GeneratedAt:      time.Now().UTC(),
		Flags:            flags,
		Segments:         map[string]*model.Segment{},
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func boolFlag(key string, def string) *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           key, Type: model.FlagString,
		Variations:  map[string]any{"a": "rev-" + def, "b": "other"},
		DefaultRule: &model.Rule{Variation: "a"},
	}
}

// TestSyncIgnoresStaleRevision covers the out-of-order notification case
// (P1 acceptance: 乱序通知用例): a late response carrying an older manifest
// revision must never replace a newer snapshot (§5.2).
func TestSyncIgnoresStaleRevision(t *testing.T) {
	var call atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := call.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1: // first sync: revision 2
			w.Header().Set("ETag", `"golab-r2"`)
			_, _ = w.Write(snapshotJSON(t, 2, map[string]*model.Flag{"f": boolFlag("f", "2")}))
		default: // racing late response: revision 1 (stale)
			w.Header().Set("ETag", `"golab-r1"`)
			_, _ = w.Write(snapshotJSON(t, 1, map[string]*model.Flag{"f": boolFlag("f", "1")}))
		}
	}))
	defer srv.Close()

	c, err := New(context.Background(), Options{
		BaseURL: srv.URL, Token: "glab_sdk_test", PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if got := c.Status().ManifestRevision; got != 2 {
		t.Fatalf("initial revision = %d", got)
	}
	// Second sync delivers an older revision — must be ignored.
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := c.Status().ManifestRevision; got != 2 {
		t.Fatalf("stale response downgraded snapshot to %d", got)
	}
	v, _ := c.StringVariation("f", model.EvaluationContext{}, "?")
	if v != "rev-2" {
		t.Fatalf("serving stale content: %q", v)
	}
}

// TestSyncRejectsInvalidSnapshot: a corrupt candidate is rejected wholesale
// and the last good snapshot keeps serving (§5.2 atomic replacement).
func TestSyncRejectsInvalidSnapshot(t *testing.T) {
	var call atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := call.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write(snapshotJSON(t, 1, map[string]*model.Flag{"f": boolFlag("f", "1")}))
			return
		}
		// Revision 2 but the flag violates the DSL (percentage sums > 100,
		// missing bucketing) — the evaluator build must fail.
		bad := boolFlag("f", "2")
		bad.Targeting = []model.Rule{{Percentage: map[string]float64{"a": 80, "b": 80}}}
		_, _ = w.Write(snapshotJSON(t, 2, map[string]*model.Flag{"f": bad}))
	}))
	defer srv.Close()

	c, err := New(context.Background(), Options{
		BaseURL: srv.URL, Token: "glab_sdk_test", PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Sync(context.Background()); err == nil {
		t.Fatal("invalid snapshot must be rejected")
	}
	st := c.Status()
	if st.ManifestRevision != 1 || !st.Ready {
		t.Fatalf("last good snapshot must remain: %+v", st)
	}
	v, _ := c.StringVariation("f", model.EvaluationContext{}, "?")
	if v != "rev-1" {
		t.Fatalf("must keep serving last good content: %q", v)
	}
}

// TestColdStartServesDefaults: no successful sync ever → caller defaults
// with an explicit reason (§3 cold-start contract).
func TestColdStartServesDefaults(t *testing.T) {
	c, _ := New(context.Background(), Options{
		BaseURL: "http://127.0.0.1:1", Token: "glab_sdk_test", PollInterval: time.Hour,
	})
	if c == nil {
		t.Fatal("client must exist even when the first sync fails")
	}
	defer c.Close()
	v, r := c.StringVariation("any", model.EvaluationContext{}, "safe")
	if v != "safe" || !r.Failed || r.Reason != model.ReasonError {
		t.Fatalf("cold start: %q %+v", v, r)
	}
	b, r2 := c.BoolVariation("any", model.EvaluationContext{}, true)
	if b != true || !r2.Failed {
		t.Fatalf("cold start bool: %v %+v", b, r2)
	}
}

// TestExposureQueueBounded: overflow drops newest events and counts them —
// evaluation is never blocked by a full queue.
func TestExposureQueueBounded(t *testing.T) {
	opts := Options{}
	c := &Client{
		opts:      opts.withDefaults(),
		exposures: make(chan *collectEvent, 2),
		closed:    make(chan struct{}),
	}
	c.opts.CollectToken = "glab_collect_test"
	snap := &model.Snapshot{
		SchemaVersion: model.SchemaVersion, Workspace: "w", Environment: "e",
		Flags: map[string]*model.Flag{"f": {
			SchemaVersion: model.SchemaVersion, Key: "f", Type: model.FlagString,
			Fields:      []string{"user_id"},
			Bucketing:   &model.Bucketing{Unit: "user_id"},
			Variations:  map[string]any{"a": "x"},
			DefaultRule: &model.Rule{Variation: "a"},
		}},
	}
	res := model.Result{
		FlagKey: "f", TrackEvents: true,
		Experiment: &model.Exposure{ExperimentID: "exp-1", Epoch: 1, Variant: "a", Source: "split"},
	}
	ctx := model.EvaluationContext{Attributes: map[string]any{"user_id": "u1"}}
	for i := 0; i < 10; i++ {
		c.queueExposure(snap, ctx, res)
	}
	if c.droppedExposures.Load() != 8 {
		t.Fatalf("dropped = %d, want 8", c.droppedExposures.Load())
	}
	if len(c.exposures) != 2 {
		t.Fatalf("queue len = %d", len(c.exposures))
	}
}

// TestEmptyEnvironmentConverges: 404 (no manifest yet) installs an empty
// snapshot; a later publish becomes visible via the next sync.
func TestEmptyEnvironmentConverges(t *testing.T) {
	var published atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !published.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"golab-r1"`)
		_, _ = w.Write(snapshotJSON(t, 1, map[string]*model.Flag{"f": boolFlag("f", "1")}))
	}))
	defer srv.Close()

	c, err := New(context.Background(), Options{
		BaseURL: srv.URL, Token: "glab_sdk_test", PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("New on empty env: %v", err)
	}
	defer c.Close()
	if st := c.Status(); !st.Ready || st.FlagCount != 0 {
		t.Fatalf("empty env status: %+v", st)
	}
	_, r := c.StringVariation("f", model.EvaluationContext{}, "safe")
	if r.ErrorCode != model.ErrFlagNotFound {
		t.Fatalf("empty env eval: %+v", r)
	}
	published.Store(true)
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("Sync after publish: %v", err)
	}
	v, r := c.StringVariation("f", model.EvaluationContext{}, "safe")
	if v != "rev-1" || r.Failed {
		t.Fatalf("converged eval: %q %+v", v, r)
	}
}

func TestLocalDecisionDiagnostic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(snapshotJSON(t, 7, map[string]*model.Flag{"f": boolFlag("f", "7")}))
	}))
	defer srv.Close()
	c, err := New(t.Context(), Options{BaseURL: srv.URL, Token: "test", EvaluationSampleRate: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	v, res := c.StringVariation("f", model.EvaluationContext{
		Attributes: map[string]any{"user_id": "private-user"},
	}, "?")
	if v != "rev-7" || res.DecisionID == "" || res.ManifestRevision != 7 || res.Rule != "default" {
		t.Fatalf("result: %+v", res)
	}
	d, ok := c.Diagnose(res.DecisionID)
	if !ok || d.Result.DecisionID != res.DecisionID || d.ExposureStatus != "none" {
		t.Fatalf("diagnostic: %+v, %v", d, ok)
	}
	encoded, _ := json.Marshal(d)
	if bytes.Contains(encoded, []byte("private-user")) {
		t.Fatalf("raw unit leaked: %s", encoded)
	}
	if st := c.Status(); st.State != "READY" || st.EvaluationCount != 1 {
		t.Fatalf("status: %+v", st)
	}
	select {
	case event := <-c.SampledEvents():
		if event.Name != "feature_flag.evaluation" || event.DecisionID != res.DecisionID ||
			event.Attributes["feature_flag.result.variant"] != res.VariationType {
			t.Fatalf("sampled event: %+v", event)
		}
		encoded, _ := json.Marshal(event)
		if bytes.Contains(encoded, []byte("private-user")) || bytes.Contains(encoded, []byte("rev-7")) {
			t.Fatalf("sampled event leaked context or value: %s", encoded)
		}
	default:
		t.Fatal("missing sampled event")
	}
}
