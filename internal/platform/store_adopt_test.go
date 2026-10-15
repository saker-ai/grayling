package platform

import (
	"encoding/json"
	"testing"
)

// A store only ever lazy-initialises its maps once, in newStore. Every path that
// later adopts a freshly loaded *Data — cluster sync, full reload, and the
// post-commit recovery paths — replaced s.data wholesale, so any map that the
// persistence layer handed back as nil stayed nil. metricDefinitions is the only
// field tagged omitempty, so it is the one that serialises to an absent key and
// comes back nil; writing to it then panicked with "assignment to entry in nil
// map" and took the whole request down.
//
// These tests pin the invariant at the level that actually broke: whatever a
// Persistence returns from Load, the Store must be able to accept it and keep
// serving writes.

// nilMapData is a persisted document that omits every map the way an empty
// store serialises: absent keys, not empty objects.
func nilMapData(t *testing.T) *Data {
	t.Helper()
	raw := []byte(`{"schemaVersion":1,"nextCounter":1}`)
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("decode minimal data: %v", err)
	}
	return &d
}

func TestAdoptNormalizesMapsFromPersistence(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, tc := range []struct {
		name string
		m    any
	}{
		{"metricDefinitions", store.data.MetricDefinitions},
		{"approvals", store.data.Approvals},
		{"experiments", store.data.Experiments},
		{"quotas", store.data.Quotas},
	} {
		if tc.m == nil {
			t.Fatalf("newStore left %s nil", tc.name)
		}
	}

	// Replacing the working set with a reloaded document must not be able to
	// reintroduce a nil map.
	store.mu.Lock()
	store.adoptLocked(nilMapData(t))
	got := store.data
	store.mu.Unlock()

	if got.MetricDefinitions == nil {
		t.Fatal("adoptLocked left MetricDefinitions nil")
	}
	if got.Approvals == nil {
		t.Fatal("adoptLocked left Approvals nil")
	}

	// And the end-to-end consequence: a write that indexes that map must not panic.
	ws, err := store.CreateWorkspace("acme", "Acme")
	if err != nil {
		t.Fatalf("CreateWorkspace after adopt: %v", err)
	}
	def, err := store.CreateMetricDefinition(testMetricDefinition(ws.ID))
	if err != nil {
		t.Fatalf("CreateMetricDefinition after adopt: %v", err)
	}
	if def == nil || def.Key != "activation.complete" {
		t.Fatalf("unexpected definition: %+v", def)
	}
	if n := len(store.ListMetricDefinitions(ws.ID)); n != 1 {
		t.Fatalf("ListMetricDefinitions = %d, want 1", n)
	}
}

func TestFullReloadNormalizesMaps(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir + "/store.json")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ws, err := store.CreateWorkspace("acme", "Acme")
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	// The reload must bring the workspace back intact, not just leave the maps
	// writable.
	if _, err := store.GetWorkspace(ws.ID); err != nil {
		t.Fatalf("GetWorkspace after full reload: %v", err)
	}

	store.mu.Lock()
	err = store.fullReloadLocked()
	data := store.data
	store.mu.Unlock()
	if err != nil {
		t.Fatalf("fullReloadLocked: %v", err)
	}
	if data.MetricDefinitions == nil {
		t.Fatal("fullReloadLocked left MetricDefinitions nil")
	}
	if _, err := store.CreateMetricDefinition(testMetricDefinition(ws.ID)); err != nil {
		t.Fatalf("CreateMetricDefinition after full reload: %v", err)
	}
}