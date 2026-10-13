package configstore

import (
	"testing"
	"time"

	"github.com/saker-ai/grayling/pkg/bucketing"
	"github.com/saker-ai/grayling/pkg/model"
	gconfig "github.com/godeps/gonacos/pkg/config"
)

// newTestStore spins up the REAL gonacos config service (zero-dependency,
// in-memory) — contract tests must run against the actual substrate, not a
// fake (docs §12.1).
func newTestStore(t *testing.T) Store {
	t.Helper()
	return NewGonacosStore(gconfig.NewService())
}

func testFlag(key string) *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           key,
		Type:          model.FlagBool,
		Fields:        []string{"user_id"},
		Bucketing:     &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: "s"},
		Variations:    map[string]any{"on": true, "off": false},
		DefaultRule:   &model.Rule{Variation: "off"},
		Targeting: []model.Rule{
			{Percentage: map[string]float64{"on": 50, "off": 50}},
		},
	}
}

func TestPublishGetRoundtrip(t *testing.T) {
	s := newTestStore(t)
	f := testFlag("flag-a")
	content, err := model.CanonicalJSON(f)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dataID, err := s.PublishDoc("golab-ws-1-env-1", FlagDataID("flag-a", 1), content)
	if err != nil {
		t.Fatalf("PublishDoc: %v", err)
	}
	if dataID != "golab.flag.flag-a.r1" {
		t.Fatalf("dataID = %s", dataID)
	}
	got, hash, err := s.GetDoc("golab-ws-1-env-1", dataID)
	if err != nil {
		t.Fatalf("GetDoc: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("content drift")
	}
	if hash != model.HashBytes(content) {
		t.Fatalf("hash mismatch")
	}
	_, _, err = s.GetDoc("golab-ws-1-env-1", "golab.flag.missing.r1")
	if err == nil {
		t.Fatal("missing doc must error")
	}
}

func TestNamespaceIsolation(t *testing.T) {
	s := newTestStore(t)
	content := []byte(`{"schemaVersion":1}`)
	if _, err := s.PublishDoc("ns-a", "golab.flag.x.r1", content); err != nil {
		t.Fatalf("publish ns-a: %v", err)
	}
	if _, _, err := s.GetDoc("ns-b", "golab.flag.x.r1"); err == nil {
		t.Fatal("cross-namespace read must fail")
	}
}

func TestManifestAndSnapshot(t *testing.T) {
	s := newTestStore(t)
	ns := "golab-ws-2-env-1"

	f := testFlag("checkout")
	fContent, _ := model.CanonicalJSON(f)
	if _, err := s.PublishDoc(ns, FlagDataID("checkout", 7), fContent); err != nil {
		t.Fatalf("publish flag: %v", err)
	}
	seg := &model.Segment{
		SchemaVersion: model.SchemaVersion, Key: "beta", Version: "2",
		Constraints: []model.Constraint{{Field: "user_id", Op: model.OpRegex, Value: `^beta-`}},
	}
	sContent, _ := model.CanonicalJSON(seg)
	if _, err := s.PublishDoc(ns, SegmentDataID("beta", 2), sContent); err != nil {
		t.Fatalf("publish segment: %v", err)
	}

	m := &model.Manifest{
		SchemaVersion: model.SchemaVersion,
		Workspace:     "ws2", Environment: "prod",
		Revision:    3,
		GeneratedAt: time.Now().UTC().Truncate(time.Second),
		Flags: []model.ManifestEntry{
			{Key: "checkout", Revision: 7, DataID: FlagDataID("checkout", 7), Hash: model.HashBytes(fContent)},
		},
		Segments: []model.ManifestEntry{
			{Key: "beta", Revision: 2, DataID: SegmentDataID("beta", 2), Hash: model.HashBytes(sContent)},
		},
	}
	if err := s.PublishManifest(ns, m); err != nil {
		t.Fatalf("PublishManifest: %v", err)
	}
	got, err := s.GetManifest(ns)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if got.Revision != 3 || len(got.Flags) != 1 || len(got.Segments) != 1 {
		t.Fatalf("manifest roundtrip: %+v", got)
	}

	snap, err := s.LoadSnapshot(ns)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap.Workspace != "ws2" || snap.Environment != "prod" || snap.ManifestRevision != 3 {
		t.Fatalf("snapshot metadata: %+v", snap)
	}
	if snap.Flags["checkout"] == nil || snap.Segments["beta"] == nil {
		t.Fatalf("snapshot content: %+v", snap)
	}
}

func TestSnapshotRejectsTamperedHash(t *testing.T) {
	s := newTestStore(t)
	ns := "golab-ws-3-env-1"
	f := testFlag("t")
	fContent, _ := model.CanonicalJSON(f)
	if _, err := s.PublishDoc(ns, FlagDataID("t", 1), fContent); err != nil {
		t.Fatalf("publish: %v", err)
	}
	m := &model.Manifest{
		SchemaVersion: model.SchemaVersion, Workspace: "w", Environment: "e", Revision: 1,
		Flags: []model.ManifestEntry{
			{Key: "t", Revision: 1, DataID: FlagDataID("t", 1), Hash: "deadbeef"}, // wrong
		},
	}
	if err := s.PublishManifest(ns, m); err != nil {
		t.Fatalf("PublishManifest: %v", err)
	}
	if _, err := s.LoadSnapshot(ns); err == nil {
		t.Fatal("hash mismatch must abort snapshot load")
	}
}

func TestSnapshotRejectsMissingDocument(t *testing.T) {
	s := newTestStore(t)
	ns := "golab-ws-4-env-1"
	m := &model.Manifest{
		SchemaVersion: model.SchemaVersion, Workspace: "w", Environment: "e", Revision: 1,
		Flags: []model.ManifestEntry{
			{Key: "ghost", Revision: 1, DataID: FlagDataID("ghost", 1), Hash: "x"},
		},
	}
	if err := s.PublishManifest(ns, m); err != nil {
		t.Fatalf("PublishManifest: %v", err)
	}
	if _, err := s.LoadSnapshot(ns); err == nil {
		t.Fatal("missing document must abort snapshot load")
	}
}

func TestDeleteDocIdempotent(t *testing.T) {
	s := newTestStore(t)
	ns := "golab-ws-5-env-1"
	if err := s.DeleteDoc(ns, "never-existed"); err != nil {
		t.Fatalf("delete missing must be idempotent: %v", err)
	}
	if _, err := s.PublishDoc(ns, "golab.flag.d.r1", []byte(`{}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := s.DeleteDoc(ns, "golab.flag.d.r1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := s.GetDoc(ns, "golab.flag.d.r1"); err == nil {
		t.Fatal("deleted doc must be gone")
	}
}

func TestPublishEmptyRejected(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.PublishDoc("ns", "golab.flag.e.r1", nil); err == nil {
		t.Fatal("empty publish must be rejected")
	}
}
