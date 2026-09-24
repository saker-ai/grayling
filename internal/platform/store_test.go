package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/godeps/golab/pkg/model"
)

func TestPersistenceRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")

	s1, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ws, err := s1.CreateWorkspace("acme", "Acme")
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	env, err := s1.CreateEnvironment(ws.ID, "prod", "")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	ct, err := s1.CreateToken(TokenAdmin, "root", RoleOwner, "", "", "boot")
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	flag := &model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "f1", Type: model.FlagBool,
		Variations:  map[string]any{"on": true, "off": false},
		DefaultRule: &model.Rule{Variation: "off"},
	}
	if _, err := s1.SaveFlagDraft(ws.ID, env.ID, flag, RiskNormal, "root", 0); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if err := s1.SubmitFlagDraft(ws.ID, env.ID, "f1", "root"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	exp, err := s1.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID,
		FlagKey: "f1", Unit: "user_id", Epoch: 1,
		Variants:    []string{"on", "off"},
		Proportions: map[string]float64{"on": 50, "off": 50},
		PrimaryMetric: MetricSpec{
			Key: "conv", Type: "proportion", Direction: "increase",
		},
		Alpha: 0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
	})
	if err != nil {
		t.Fatalf("experiment: %v", err)
	}
	unitHash := s1.HashUnit(ws.ID, "user-1")
	if _, err := s1.IngestEvents([]*Event{{
		EventID: "e1", WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Type: EventExposure, ExperimentID: exp.ID, Epoch: 1,
		Variant: "on", UnitHash: unitHash, Source: "split", ClientTime: time.Now(),
	}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := s1.RegisterAIResource(ws.ID, env.ID, AITypePrompt, "tone", "root"); err != nil {
		t.Fatalf("ai register: %v", err)
	}
	if err := s1.RevokeAIResource(ws.ID, env.ID, AITypePrompt, "tone", "drill", "root"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := s1.Audit("root", RoleOwner, "test.action", ws.ID, env.ID, "f1", "{}"); err != nil {
		t.Fatalf("audit: %v", err)
	}

	// Reopen: everything must survive, including the HMAC key (unit hashes
	// must be stable across restarts or attribution breaks).
	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	ws2, err := s2.GetWorkspace("acme")
	if err != nil || ws2.ID != ws.ID {
		t.Fatalf("ws roundtrip: %v", err)
	}
	envs := s2.ListEnvironments(ws.ID)
	if len(envs) != 1 || envs[0].NamespaceID != env.NamespaceID {
		t.Fatalf("env roundtrip: %+v", envs)
	}
	if _, err := s2.ResolveToken(ct.Secret); err != nil {
		t.Fatalf("token roundtrip: %v", err)
	}
	draft, err := s2.GetFlagDraft(ws.ID, env.ID, "f1")
	if err != nil || draft.State != DraftSubmitted {
		t.Fatalf("draft roundtrip: %+v %v", draft, err)
	}
	exp2, err := s2.GetExperiment(exp.ID)
	if err != nil || exp2.PrimaryMetric.Key != "conv" {
		t.Fatalf("experiment roundtrip: %v", err)
	}
	if s2.HashUnit(ws.ID, "user-1") != unitHash {
		t.Fatal("HMAC key must survive restart (unit attribution stability)")
	}
	split := s2.SplitExposedUnitCounts(ws.ID, env.ID, exp.ID, 1)
	if split["on"] != 1 {
		t.Fatalf("exposure aggregates lost: %v", split)
	}
	if _, revoked := s2.IsAIRevoked(ws.ID, env.ID, AITypePrompt, "tone"); !revoked {
		t.Fatal("revocation must survive restart")
	}
	if len(s2.AuditList(ws.ID, 0)) == 0 {
		t.Fatal("audit must survive restart")
	}

	// Store file must be permission-guarded (contains HMAC key material).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("store file permissions too open: %v", info.Mode())
	}
}

func TestMemoryOnlyStore(t *testing.T) {
	s, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := s.CreateWorkspace("mem", ""); err != nil {
		t.Fatalf("ws: %v", err)
	}
}

func TestSlugValidation(t *testing.T) {
	s, _ := NewStore("")
	for _, bad := range []string{"", "UPPER", "1start", "with space", "under_score", "dot.ted", "a"} {
		// single letters are fine; keep the truly bad ones bad
		if bad == "a" {
			continue
		}
		if _, err := s.CreateWorkspace(bad, ""); err == nil || !errors.Is(err, ErrValidation) {
			t.Errorf("slug %q accepted", bad)
		}
	}
	if _, err := s.CreateWorkspace("good-slug", ""); err != nil {
		t.Errorf("good-slug rejected: %v", err)
	}
}

func TestSelfApprovalAndStateMachines(t *testing.T) {
	s, _ := NewStore("")
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")
	flag := &model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "f", Type: model.FlagBool,
		Variations:  map[string]any{"on": true, "off": false},
		DefaultRule: &model.Rule{Variation: "off"},
	}
	if _, err := s.SaveFlagDraft(ws.ID, env.ID, flag, RiskHigh, "alice", 0); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SubmitFlagDraft(ws.ID, env.ID, "f", "alice"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := s.ApproveFlagDraft(ws.ID, env.ID, "f", "alice", ""); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("high-risk self-approval: %v", err)
	}
	if err := s.ApproveFlagDraft(ws.ID, env.ID, "f", "bob", ""); err != nil {
		t.Fatalf("cross approval: %v", err)
	}
	// Double submit from approved state is rejected.
	if err := s.SubmitFlagDraft(ws.ID, env.ID, "f", "alice"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("resubmit approved: %v", err)
	}

	// Experiment state machine.
	exp, err := s.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: 1,
		Unit: "user_id", Variants: []string{"on", "off"},
		Proportions:   map[string]float64{"on": 50, "off": 50},
		PrimaryMetric: MetricSpec{Key: "m", Type: "mean", Direction: "decrease"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatalf("create exp: %v", err)
	}
	if _, err := s.TransitionExperiment(exp.ID, model.ExperimentRunning, "x"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("draft→running must be rejected: %v", err)
	}
	for _, step := range []model.ExperimentState{
		model.ExperimentReviewed, model.ExperimentRunning, model.ExperimentPaused,
		model.ExperimentRunning, model.ExperimentStopped, model.ExperimentAnalyzed,
		model.ExperimentArchived,
	} {
		if _, err := s.TransitionExperiment(exp.ID, step, "x"); err != nil {
			t.Fatalf("transition → %s: %v", step, err)
		}
	}
	if _, err := s.TransitionExperiment(exp.ID, model.ExperimentRunning, "x"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("archived→running must be rejected: %v", err)
	}
}

func TestEpochsNeverReused(t *testing.T) {
	s, _ := NewStore("")
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")
	mk := func(epoch int64) *Experiment {
		return &Experiment{
			WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: epoch,
			Unit: "user_id", Variants: []string{"a", "b"},
			Proportions:   map[string]float64{"a": 50, "b": 50},
			PrimaryMetric: MetricSpec{Key: "m", Type: "proportion", Direction: "increase"},
			Alpha:         0.05, AnalysisWindow: time.Hour,
		}
	}
	if _, err := s.CreateExperiment(mk(1)); err != nil {
		t.Fatalf("epoch 1: %v", err)
	}
	if _, err := s.CreateExperiment(mk(1)); !errors.Is(err, ErrExists) {
		t.Fatalf("epoch reuse must be rejected: %v", err)
	}
	if next := s.NextEpoch(ws.ID, env.ID, "f"); next != 2 {
		t.Fatalf("NextEpoch = %d", next)
	}
}

func TestPruneKeepsCurrentAndWindow(t *testing.T) {
	s, _ := NewStore("")
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")
	flag := &model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "f", Type: model.FlagBool,
		Variations:  map[string]any{"on": true, "off": false},
		DefaultRule: &model.Rule{Variation: "off"},
	}
	for i := 1; i <= 6; i++ {
		if err := s.AppendFlagRevision(&FlagRevision{
			WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: "f",
			Revision: int64(i), Content: flag, Hash: string(rune('a' + i)),
			DataID: "golab.flag.f.r" + string(rune('0'+i)),
			// Backdate all but the last so the time window doesn't protect them.
			PublishedAt: time.Now().Add(-time.Duration(7-i) * 48 * time.Hour),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// Roll the current pointer back to r2 to prove pruning protects it.
	if err := s.SetCurrentFlagRevision(ws.ID, env.ID, "f", 2, "op"); err != nil {
		t.Fatalf("set current: %v", err)
	}
	removed, err := s.PruneFlagRevisions(ws.ID, env.ID, "f", 3, 24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	// Keep newest 3 (r4,r5,r6) + current (r2) → r1 and r3 removed.
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want r1+r3 dataIds", removed)
	}
	if _, err := s.GetFlagRevision(ws.ID, env.ID, "f", 2); err != nil {
		t.Fatalf("current revision must survive prune: %v", err)
	}
	if _, err := s.GetFlagRevision(ws.ID, env.ID, "f", 6); err != nil {
		t.Fatalf("newest revisions must survive prune: %v", err)
	}
	if _, err := s.GetFlagRevision(ws.ID, env.ID, "f", 1); err == nil {
		t.Fatal("r1 must be pruned")
	}
}

func TestTombstoneWindow(t *testing.T) {
	s, _ := NewStore("")
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")
	flag := &model.Flag{
		SchemaVersion: model.SchemaVersion, Key: "f", Type: model.FlagBool,
		Variations:  map[string]any{"on": true, "off": false},
		DefaultRule: &model.Rule{Variation: "off"},
	}
	if err := s.AppendFlagRevision(&FlagRevision{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: "f",
		Revision: 1, Content: flag, Hash: "h", DataID: "d1",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.DeleteFlagCurrent(ws.ID, env.ID, "f", 5); err != nil {
		t.Fatalf("delete: %v", err)
	}
	entries := s.FlagTombstones(ws.ID, env.ID, 6)
	if len(entries) != 1 || !entries[0].Deleted {
		t.Fatalf("tombstone missing: %+v", entries)
	}
	// Beyond the window the tombstone disappears.
	entries = s.FlagTombstones(ws.ID, env.ID, 5+TombstoneWindow+1)
	if len(entries) != 0 {
		t.Fatalf("tombstone must expire: %+v", entries)
	}
}
