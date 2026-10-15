package admin

import (
	"errors"
	"testing"

	gconfig "github.com/godeps/gonacos/pkg/config"
	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/bucketing"
	"github.com/saker-ai/grayling/pkg/model"
)

type fixture struct {
	svc        *Service
	store      *platform.Store
	ws         *platform.Workspace
	env        *platform.Environment
	owner      Actor
	publisher  Actor
	publisher2 Actor
	analyst    Actor
	viewer     Actor
	otherWS    Actor // owner of a DIFFERENT workspace
	notified   []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := configstore.NewGonacosStore(gconfig.NewService())
	f := &fixture{store: store}
	f.svc = NewService(store, cfg, func(wsID, envID string, rev int64) {
		f.notified = append(f.notified, wsID+"/"+envID)
	})

	ws, err := store.CreateWorkspace("acme", "Acme Inc")
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	f.ws, f.env = ws, env

	other, err := store.CreateWorkspace("globex", "Globex")
	if err != nil {
		t.Fatalf("CreateWorkspace globex: %v", err)
	}
	if _, err := store.CreateEnvironment(other.ID, "prod", "Production"); err != nil {
		t.Fatalf("CreateEnvironment globex: %v", err)
	}

	f.owner = Actor{TokenID: "tok-owner", Name: "root", Kind: platform.TokenAdmin, Role: platform.RoleOwner}
	f.publisher = Actor{TokenID: "tok-pub1", Name: "alice", Kind: platform.TokenAdmin,
		Role: platform.RolePublisher, WorkspaceID: ws.ID}
	f.publisher2 = Actor{TokenID: "tok-pub2", Name: "bob", Kind: platform.TokenAdmin,
		Role: platform.RolePublisher, WorkspaceID: ws.ID}
	f.analyst = Actor{TokenID: "tok-an", Name: "carol", Kind: platform.TokenAdmin,
		Role: platform.RoleAnalyst, WorkspaceID: ws.ID}
	f.viewer = Actor{TokenID: "tok-v", Name: "dave", Kind: platform.TokenAdmin,
		Role: platform.RoleViewer, WorkspaceID: ws.ID}
	f.otherWS = Actor{TokenID: "tok-o2", Name: "eve", Kind: platform.TokenAdmin,
		Role: platform.RoleOwner, WorkspaceID: other.ID}
	for _, a := range []*Actor{&f.owner, &f.publisher, &f.publisher2, &f.analyst, &f.viewer, &f.otherWS} {
		ct, err := store.CreateToken(platform.TokenAdmin, a.Name, a.Role, a.WorkspaceID, "", "fixture")
		if err != nil {
			t.Fatalf("CreateToken %s: %v", a.Name, err)
		}
		a.TokenID = ct.Token.ID
	}
	return f
}

func testFlag(key string) *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           key, Type: model.FlagBool,
		Fields:      []string{"user_id"},
		Bucketing:   &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: "s1"},
		Variations:  map[string]any{"on": true, "off": false},
		DefaultRule: &model.Rule{Variation: "off"},
		Targeting: []model.Rule{
			{Name: "ramp", Percentage: map[string]float64{"on": 50, "off": 50}},
		},
	}
}

// fullPublishFlow walks draft→submit→approve→publish and returns the result.
func fullPublishFlow(t *testing.T, f *fixture, actor, approver Actor, flag *model.Flag) *PublishResult {
	t.Helper()
	if _, err := f.svc.SaveFlagDraft(actor, f.ws.ID, f.env.ID, flag, platform.RiskNormal, 0); err != nil {
		t.Fatalf("SaveFlagDraft: %v", err)
	}
	if err := f.svc.SubmitFlagDraft(actor, f.ws.ID, f.env.ID, flag.Key); err != nil {
		t.Fatalf("SubmitFlagDraft: %v", err)
	}
	if err := f.svc.ApproveFlagDraft(approver, f.ws.ID, f.env.ID, flag.Key, "lgtm"); err != nil {
		t.Fatalf("ApproveFlagDraft: %v", err)
	}
	res, err := f.svc.PublishFlag(approver, f.ws.ID, f.env.ID, flag.Key, "release")
	if err != nil {
		t.Fatalf("PublishFlag: %v", err)
	}
	return res
}

func TestPublishPipelineEndToEnd(t *testing.T) {
	f := newFixture(t)
	res := fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))

	if res.FlagRevision != 1 || res.ManifestRevision != 1 {
		t.Fatalf("first publish: %+v", res)
	}
	if len(f.notified) == 0 {
		t.Fatal("listener must be notified after manifest read-back")
	}

	// The snapshot must load from the substrate with the published content.
	snap, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap.ManifestRevision != 1 {
		t.Fatalf("snapshot manifest revision = %d", snap.ManifestRevision)
	}
	got := snap.Flags["checkout"]
	if got == nil || got.Bucketing == nil || got.Bucketing.Salt != "s1" {
		t.Fatalf("snapshot flag content: %+v", got)
	}
	if snap.Workspace != f.ws.ID || snap.Environment != f.env.ID {
		t.Fatalf("hash coordinates must be surrogate IDs: %s/%s", snap.Workspace, snap.Environment)
	}
	if snap.WorkspaceSlug != "acme" || snap.EnvironmentSlug != "prod" {
		t.Fatalf("slugs: %s/%s", snap.WorkspaceSlug, snap.EnvironmentSlug)
	}

	// Second revision increments both counters.
	flag2 := testFlag("checkout")
	flag2.Bucketing.Salt = "s2"
	res2 := fullPublishFlow(t, f, f.publisher, f.owner, flag2)
	if res2.FlagRevision != 2 || res2.ManifestRevision != 2 {
		t.Fatalf("second publish: %+v", res2)
	}
	// The r1 document is still retained (immutable).
	if _, _, err := f.svc.Config.GetDoc(f.env.NamespaceID, configstore.FlagDataID("checkout", 1)); err != nil {
		t.Fatalf("r1 must remain readable: %v", err)
	}
}

func TestPublishRequiresApproval(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, testFlag("x"), platform.RiskNormal, 0); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Editing state cannot be published.
	if _, err := f.svc.PublishFlag(f.publisher, f.ws.ID, f.env.ID, "x", ""); !errors.Is(err, platform.ErrInvalidState) {
		t.Fatalf("publish without review: %v", err)
	}
	if err := f.svc.SubmitFlagDraft(f.publisher, f.ws.ID, f.env.ID, "x"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Submitted (not approved) cannot be published.
	if _, err := f.svc.PublishFlag(f.publisher, f.ws.ID, f.env.ID, "x", ""); !errors.Is(err, platform.ErrInvalidState) {
		t.Fatalf("publish unapproved: %v", err)
	}
}

func TestHighRiskSelfApprovalAllowed(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, testFlag("hr"), platform.RiskHigh, 0); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := f.svc.SubmitFlagDraft(f.publisher, f.ws.ID, f.env.ID, "hr"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Risk level does not change who may approve: the submitter can.
	if err := f.svc.ApproveFlagDraft(f.publisher, f.ws.ID, f.env.ID, "hr", "self"); err != nil {
		t.Fatalf("high-risk self-approval must be allowed: %v", err)
	}
	selfDraft, err := f.store.GetFlagDraft(f.ws.ID, f.env.ID, "hr")
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if selfDraft.State != platform.DraftApproved || selfDraft.ApprovedBy != f.publisher.Name {
		t.Fatalf("self-approved high-risk draft: %+v", selfDraft)
	}
	// Different publisher → allowed as well.
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, testFlag("hr2"), platform.RiskHigh, 0); err != nil {
		t.Fatalf("save hr2: %v", err)
	}
	if err := f.svc.SubmitFlagDraft(f.publisher, f.ws.ID, f.env.ID, "hr2"); err != nil {
		t.Fatalf("submit hr2: %v", err)
	}
	if err := f.svc.ApproveFlagDraft(f.publisher2, f.ws.ID, f.env.ID, "hr2", ""); err != nil {
		t.Fatalf("cross-approval: %v", err)
	}
	crossDraft, err := f.store.GetFlagDraft(f.ws.ID, f.env.ID, "hr2")
	if err != nil {
		t.Fatalf("get hr2 draft: %v", err)
	}
	if crossDraft.State != platform.DraftApproved || crossDraft.ApprovedBy != f.publisher2.Name {
		t.Fatalf("cross-approved high-risk draft: %+v", crossDraft)
	}
}

func TestRBACCrossWorkspaceDenied(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("secret"))

	// The globex owner must not read or write acme flags.
	if _, err := f.svc.GetFlagView(f.otherWS, f.ws.ID, f.env.ID, "secret", false); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("cross-workspace read: %v", err)
	}
	if _, err := f.svc.SaveFlagDraft(f.otherWS, f.ws.ID, f.env.ID, testFlag("evil"), platform.RiskNormal, 0); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("cross-workspace write: %v", err)
	}
	// Viewer cannot publish.
	if _, err := f.svc.PublishFlag(f.viewer, f.ws.ID, f.env.ID, "secret", ""); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("viewer publish: %v", err)
	}
	// Analyst cannot publish.
	if _, err := f.svc.PublishFlag(f.analyst, f.ws.ID, f.env.ID, "secret", ""); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("analyst publish: %v", err)
	}
	// SDK tokens are never admin actors.
	sdkActor := Actor{Kind: platform.TokenSDK, WorkspaceID: f.ws.ID}
	if _, err := f.svc.ListFlags(sdkActor, f.ws.ID, f.env.ID); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("sdk token on admin API: %v", err)
	}
}

func TestOptimisticLocking(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("lock"))

	// Editor A loads at revision 1, editor B publishes revision 2 first.
	updated := testFlag("lock")
	updated.Bucketing.Salt = "b"
	if _, err := f.svc.SaveFlagDraft(f.publisher2, f.ws.ID, f.env.ID, updated, platform.RiskNormal, 1); err != nil {
		t.Fatalf("B save at base 1: %v", err)
	}
	if err := f.svc.SubmitFlagDraft(f.publisher2, f.ws.ID, f.env.ID, "lock"); err != nil {
		t.Fatalf("B submit: %v", err)
	}
	if err := f.svc.ApproveFlagDraft(f.owner, f.ws.ID, f.env.ID, "lock", ""); err != nil {
		t.Fatalf("B approve: %v", err)
	}
	if _, err := f.svc.PublishFlag(f.publisher2, f.ws.ID, f.env.ID, "lock", ""); err != nil {
		t.Fatalf("B publish: %v", err)
	}
	// A now saves against stale base revision 1 → conflict.
	stale := testFlag("lock")
	stale.Bucketing.Salt = "a"
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, stale, platform.RiskNormal, 1); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("stale base must conflict: %v", err)
	}
	latest, err := f.svc.GetFlagView(f.publisher, f.ws.ID, f.env.ID, "lock", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, stale,
		platform.RiskNormal, 0, latest.DraftHash); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("checked save cannot bypass published revision lock with base 0: %v", err)
	}
	// Saving against the current revision succeeds.
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, stale, platform.RiskNormal, 2); err != nil {
		t.Fatalf("save at current base: %v", err)
	}
}

func TestFlagDraftHashPreventsSameRevisionOverwrite(t *testing.T) {
	f := newFixture(t)
	first := testFlag("shared")
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, first,
		platform.RiskNormal, 0, ""); err != nil {
		t.Fatalf("initial save: %v", err)
	}
	if _, err := f.svc.SaveFlagDraft(f.publisher2, f.ws.ID, f.env.ID, first,
		platform.RiskNormal, 0, ""); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("editor who saw no draft must not overwrite new draft: %v", err)
	}
	view, err := f.svc.GetFlagView(f.publisher2, f.ws.ID, f.env.ID, first.Key, false)
	if err != nil || view.DraftHash == "" {
		t.Fatalf("draft fingerprint missing: %+v, %v", view, err)
	}
	second := testFlag("shared")
	second.Bucketing.Salt = "bob"
	if _, err := f.svc.SaveFlagDraft(f.publisher2, f.ws.ID, f.env.ID, second,
		platform.RiskNormal, 0, view.DraftHash); err != nil {
		t.Fatalf("second editor save: %v", err)
	}
	stale := testFlag("shared")
	stale.Bucketing.Salt = "alice"
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, stale,
		platform.RiskNormal, 0, view.DraftHash); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("stale draft fingerprint must conflict: %v", err)
	}
	latest, err := f.svc.GetFlagView(f.publisher, f.ws.ID, f.env.ID, first.Key, false)
	if err != nil || latest.Draft.Content.Bucketing.Salt != "bob" || latest.DraftHash == view.DraftHash {
		t.Fatalf("stale save overwrote editor B: %+v, %v", latest, err)
	}
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, stale,
		platform.RiskNormal, 0, latest.DraftHash); err != nil {
		t.Fatalf("save after reading latest draft: %v", err)
	}
}

func TestRollback(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("rb"))
	v2 := testFlag("rb")
	v2.Bucketing.Salt = "changed"
	fullPublishFlow(t, f, f.publisher, f.owner, v2)

	res, err := f.svc.RollbackFlag(f.owner, f.ws.ID, f.env.ID, "rb", 1, "bad salt")
	if err != nil {
		t.Fatalf("RollbackFlag: %v", err)
	}
	// Rollback = NEW manifest revision pointing at the OLD flag revision.
	if res.ManifestRevision != 3 || res.FlagRevision != 1 {
		t.Fatalf("rollback result: %+v", res)
	}
	snap, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap.Flags["rb"].Bucketing.Salt != "s1" {
		t.Fatalf("snapshot must serve rolled-back content, got salt %q", snap.Flags["rb"].Bucketing.Salt)
	}
	if snap.ManifestRevision != 3 {
		t.Fatalf("manifest revision = %d", snap.ManifestRevision)
	}
	// Rollback without reason is rejected; unknown revision too.
	if _, err := f.svc.RollbackFlag(f.owner, f.ws.ID, f.env.ID, "rb", 1, ""); err == nil {
		t.Fatal("rollback without reason must fail")
	}
	if _, err := f.svc.RollbackFlag(f.owner, f.ws.ID, f.env.ID, "rb", 99, "nope"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("unknown revision: %v", err)
	}
}

func TestRollbackDetectsPrunedRevision(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("prune"))
	v2 := testFlag("prune")
	v2.Bucketing.Salt = "v2"
	fullPublishFlow(t, f, f.publisher, f.owner, v2)

	// Simulate substrate pruning: remove the r1 document behind Grayling's back.
	if err := f.svc.Config.DeleteDoc(f.env.NamespaceID, configstore.FlagDataID("prune", 1)); err != nil {
		t.Fatalf("DeleteDoc: %v", err)
	}
	if _, err := f.svc.RollbackFlag(f.owner, f.ws.ID, f.env.ID, "prune", 1, "try"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("rollback to pruned revision must fail closed: %v", err)
	}
}

func TestDeleteFlagTombstone(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("gone"))

	if err := f.svc.MarkFlagDeleted(f.publisher, f.ws.ID, f.env.ID, "gone"); err != nil {
		t.Fatalf("MarkFlagDeleted: %v", err)
	}
	if err := f.svc.SubmitFlagDraft(f.publisher, f.ws.ID, f.env.ID, "gone"); err != nil {
		t.Fatalf("submit deletion: %v", err)
	}
	if err := f.svc.ApproveFlagDraft(f.owner, f.ws.ID, f.env.ID, "gone", ""); err != nil {
		t.Fatalf("approve deletion: %v", err)
	}
	res, err := f.svc.PublishFlag(f.owner, f.ws.ID, f.env.ID, "gone", "")
	if err != nil {
		t.Fatalf("publish deletion: %v", err)
	}
	if res.ManifestRevision != 2 {
		t.Fatalf("deletion manifest: %+v", res)
	}
	snap, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if _, still := snap.Flags["gone"]; still {
		t.Fatal("deleted flag must be absent from the snapshot")
	}
	// Manifest still carries the tombstone for delta-capable clients.
	m, err := f.svc.Config.GetManifest(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	found := false
	for _, e := range m.Flags {
		if e.Key == "gone" && e.Deleted {
			found = true
		}
	}
	if !found {
		t.Fatalf("tombstone missing from manifest: %+v", m.Flags)
	}
}

func TestInvalidFlagRejectedAtDraft(t *testing.T) {
	f := newFixture(t)
	bad := testFlag("bad")
	bad.Targeting[0].Percentage = map[string]float64{"on": 60, "off": 60} // sums > 100
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, bad, platform.RiskNormal, 0); !errors.Is(err, platform.ErrValidation) {
		t.Fatalf("invalid split must be rejected: %v", err)
	}
	bad2 := testFlag("bad2")
	bad2.Targeting[0].Constraints = []model.Constraint{{Field: "ghost", Op: model.OpEq, Value: 1}}
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, bad2, platform.RiskNormal, 0); !errors.Is(err, platform.ErrValidation) {
		t.Fatalf("unregistered field must be rejected: %v", err)
	}
}

func TestQuotaEnforced(t *testing.T) {
	f := newFixture(t)
	f.store.SetQuotas(f.ws.ID, platform.Quotas{
		MaxFlags: 1, MaxFlagBytes: 64 * 1024, MaxSegments: 10, MaxExperiments: 10,
		MaxEventBytes: 1024, MaxEventsPerBatch: 10,
	})
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("first"))
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, testFlag("second"), platform.RiskNormal, 0); !errors.Is(err, platform.ErrQuotaExceeded) {
		t.Fatalf("flag quota must be enforced: %v", err)
	}
}

func TestSegmentsPublishAndValidate(t *testing.T) {
	f := newFixture(t)
	seg := &model.Segment{
		SchemaVersion: model.SchemaVersion, Key: "beta-users", Version: "",
		Constraints: []model.Constraint{{Field: "user_id", Op: model.OpRegex, Value: `^beta-`}},
	}
	if _, err := f.svc.SaveSegmentDraft(f.publisher, f.ws.ID, f.env.ID, seg, platform.RiskNormal); err != nil {
		t.Fatalf("SaveSegmentDraft: %v", err)
	}
	if err := f.svc.SubmitSegmentDraft(f.publisher, f.ws.ID, f.env.ID, "beta-users"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := f.svc.ApproveSegmentDraft(f.owner, f.ws.ID, f.env.ID, "beta-users"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := f.svc.PublishSegment(f.owner, f.ws.ID, f.env.ID, "beta-users", ""); err != nil {
		t.Fatalf("PublishSegment: %v", err)
	}

	// A flag referencing the segment validates; one referencing an unknown
	// segment does not.
	withSeg := testFlag("uses-seg")
	withSeg.Targeting = []model.Rule{{Segments: []string{"beta-users"}, Variation: "on"}}
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, withSeg, platform.RiskNormal, 0); err != nil {
		t.Fatalf("flag with published segment: %v", err)
	}
	badRef := testFlag("bad-ref")
	badRef.Targeting = []model.Rule{{Segments: []string{"ghost-seg"}, Variation: "on"}}
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, badRef, platform.RiskNormal, 0); !errors.Is(err, platform.ErrValidation) {
		t.Fatalf("unknown segment must be rejected: %v", err)
	}

	// Snapshot carries the segment.
	fullPublishFlow(t, f, f.publisher, f.owner, withSeg)
	snap, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap.Segments["beta-users"] == nil {
		t.Fatalf("segment missing from snapshot: %+v", snap.Segments)
	}
}

func TestSegmentDraftAllowsSelfApproval(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		key  string
		risk platform.Risk
	}{
		{"self-seg-normal", platform.RiskNormal},
		{"self-seg-high", platform.RiskHigh},
	} {
		t.Run(tc.key, func(t *testing.T) {
			seg := &model.Segment{
				SchemaVersion: model.SchemaVersion, Key: tc.key, Version: "",
				Constraints: []model.Constraint{{Field: "user_id", Op: model.OpRegex, Value: `^beta-`}},
			}
			if _, err := f.svc.SaveSegmentDraft(f.publisher, f.ws.ID, f.env.ID, seg, tc.risk); err != nil {
				t.Fatalf("SaveSegmentDraft: %v", err)
			}
			if err := f.svc.SubmitSegmentDraft(f.publisher, f.ws.ID, f.env.ID, tc.key); err != nil {
				t.Fatalf("SubmitSegmentDraft: %v", err)
			}
			// The submitter may approve their own segment draft.
			if err := f.svc.ApproveSegmentDraft(f.publisher, f.ws.ID, f.env.ID, tc.key); err != nil {
				t.Fatalf("segment self-approval must be allowed: %v", err)
			}
			draft, err := f.store.GetSegmentDraft(f.ws.ID, f.env.ID, tc.key)
			if err != nil {
				t.Fatalf("GetSegmentDraft: %v", err)
			}
			if draft.State != platform.DraftApproved || draft.ApprovedBy != f.publisher.Name {
				t.Fatalf("self-approved segment draft: %+v", draft)
			}
			if _, err := f.svc.PublishSegment(f.publisher, f.ws.ID, f.env.ID, tc.key, "self"); err != nil {
				t.Fatalf("PublishSegment: %v", err)
			}
		})
	}
}

func TestExperimentLifecycleRepublishesFlag(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("exp-flag"))

	exp := &platform.Experiment{
		FlagKey: "exp-flag", Unit: "user_id",
		Variants:    []string{"control", "candidate"},
		Proportions: map[string]float64{"control": 50, "candidate": 50},
		PrimaryMetric: platform.MetricSpec{
			Key: "conversion", Type: "proportion", Direction: "increase",
		},
		Alpha: 0.05, Power: 0.8, MDE: 0.02, MinSamplePerArm: 1000,
		AnalysisWindow: 14 * 24 * 3600e9, // 14d in ns
		Hypothesis:     "candidate lifts conversion",
	}
	// Variants must exist on the flag.
	if _, err := f.svc.CreateExperiment(f.analyst, f.ws.ID, f.env.ID, exp); err == nil {
		t.Fatal("variants control/candidate do not exist on on/off flag — must fail")
	}
	exp.Variants = []string{"on", "off"}
	exp.Proportions = map[string]float64{"on": 50, "off": 50}
	exp.CreatedBy = "spoofed"
	created, err := f.svc.CreateExperiment(f.analyst, f.ws.ID, f.env.ID, exp)
	if err != nil {
		t.Fatalf("CreateExperiment: %v", err)
	}
	if created.Epoch != 1 || created.State != model.ExperimentDraft {
		t.Fatalf("created: %+v", created)
	}
	if created.CreatedBy != f.analyst.Name {
		t.Fatalf("createdBy = %q, want authenticated actor %q", created.CreatedBy, f.analyst.Name)
	}
	if created.AnalysisMethod != "two-proportion-z" {
		t.Fatalf("analysis method = %q", created.AnalysisMethod)
	}
	// Analyst cannot publish; binding requires flag.write.
	if _, err := f.svc.ReviewExperiment(f.viewer, created.ID, ""); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("viewer review: %v", err)
	}
	if _, err := f.svc.ReviewExperiment(f.analyst, created.ID, "pre-registration complete"); err != nil {
		t.Fatalf("ReviewExperiment: %v", err)
	}
	readiness, err := f.svc.CheckExperimentBinding(f.viewer, created.ID)
	if err != nil || !readiness.Ready || readiness.CurrentRevision != created.FlagRevision {
		t.Fatalf("binding precheck: %+v, %v", readiness, err)
	}
	current := f.store.CurrentFlagContents(f.ws.ID, f.env.ID)[created.FlagKey]
	var changed model.Flag
	if err := copyFlagRules(&changed, current.Content); err != nil {
		t.Fatal(err)
	}
	if changed.DefaultRule.Variation == "on" {
		changed.DefaultRule.Variation = "off"
	} else {
		changed.DefaultRule.Variation = "on"
	}
	if _, err := f.store.SaveFlagDraft(f.ws.ID, f.env.ID, &changed, platform.RiskNormal,
		"editor", current.Revision); err != nil {
		t.Fatal(err)
	}
	readiness, err = f.svc.CheckExperimentBinding(f.viewer, created.ID)
	if err != nil || readiness.Ready {
		t.Fatalf("changed draft must block binding: %+v, %v", readiness, err)
	}
	if _, err := f.svc.BindExperimentFlag(f.publisher, created.ID); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("binding overwrote changed draft: %v", err)
	}
	if after, err := f.store.GetExperiment(created.ID); err != nil || after.State != model.ExperimentReviewed {
		t.Fatalf("failed bind changed experiment state: %+v, %v", after, err)
	}
	clean := *current.Content
	if _, err := f.store.SaveFlagDraft(f.ws.ID, f.env.ID, &clean, platform.RiskNormal,
		"editor", current.Revision); err != nil {
		t.Fatal(err)
	}
	cleanDraft, err := f.store.GetFlagDraft(f.ws.ID, f.env.ID, created.FlagKey)
	if err != nil {
		t.Fatal(err)
	}
	staleHash, err := platform.FlagDraftFingerprint(cleanDraft)
	if err != nil {
		t.Fatal(err)
	}
	concurrentEdit := clean
	concurrentEdit.Disable = !clean.Disable
	if _, err := f.store.SaveFlagDraft(f.ws.ID, f.env.ID, &concurrentEdit, platform.RiskNormal,
		"other-editor", current.Revision); err != nil {
		t.Fatal(err)
	}
	staleBinding := clean
	staleBinding.Experiment = created.ExperimentBinding()
	staleBinding.Experiment.State = model.ExperimentScheduled
	staleBinding.Version = ""
	if _, err := f.store.ScheduleExperimentBinding(created.ID, model.ExperimentReviewed,
		&staleBinding, "binder", current.Revision, staleHash); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("stale binding must conflict: %v", err)
	}
	if after, err := f.store.GetExperiment(created.ID); err != nil || after.State != model.ExperimentReviewed {
		t.Fatalf("stale binding changed experiment state: %+v, %v", after, err)
	}
	if after, err := f.store.GetFlagDraft(f.ws.ID, f.env.ID, created.FlagKey); err != nil ||
		after.UpdatedBy != "other-editor" {
		t.Fatalf("stale binding replaced concurrent editor's draft: %+v, %v", after, err)
	}
	if _, err := f.store.SaveFlagDraft(f.ws.ID, f.env.ID, &clean, platform.RiskNormal,
		"editor", current.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.BindExperimentFlag(f.publisher, created.ID); err != nil {
		t.Fatalf("BindExperimentFlag: %v", err)
	}
	readiness, err = f.svc.CheckExperimentBinding(f.viewer, created.ID)
	if err != nil || readiness.Ready {
		t.Fatalf("binding should not be repeated over a submitted draft: %+v, %v", readiness, err)
	}
	// Binding created a HIGH-risk draft in submitted state; the binder may
	// approve it, and only that one grant is recorded.
	if err := f.svc.ApproveFlagDraft(f.publisher, f.ws.ID, f.env.ID, "exp-flag", "self"); err != nil {
		t.Fatalf("binder self-approval must be allowed (high risk): %v", err)
	}
	bindingDraft, err := f.store.GetFlagDraft(f.ws.ID, f.env.ID, "exp-flag")
	if err != nil {
		t.Fatalf("get binding draft: %v", err)
	}
	if bindingDraft.State != platform.DraftApproved || bindingDraft.ApprovedBy != f.publisher.Name {
		t.Fatalf("self-approved binding draft: %+v", bindingDraft)
	}
	if err := f.svc.ApproveFlagDraft(f.owner, f.ws.ID, f.env.ID, "exp-flag", "ok"); !errors.Is(err, platform.ErrInvalidState) {
		t.Fatalf("approve binding twice: %v", err)
	}
	if _, err := f.svc.PublishFlag(f.publisher, f.ws.ID, f.env.ID, "exp-flag", ""); err != nil {
		t.Fatalf("publish binding: %v", err)
	}

	snap, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	binding := snap.Flags["exp-flag"].Experiment
	if binding == nil || binding.ID != created.ID || binding.Epoch != 1 {
		t.Fatalf("binding in snapshot: %+v", binding)
	}
	if binding.State != model.ExperimentScheduled {
		t.Fatalf("binding state = %s, want scheduled", binding.State)
	}

	// Start: transition to running republishes the flag with the new state.
	if _, err := f.svc.TransitionExperiment(f.publisher, created.ID, model.ExperimentRunning, "launch"); err != nil {
		t.Fatalf("TransitionExperiment: %v", err)
	}
	snap2, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("LoadSnapshot 2: %v", err)
	}
	if snap2.Flags["exp-flag"].Experiment.State != model.ExperimentRunning {
		t.Fatalf("running state not republished: %+v", snap2.Flags["exp-flag"].Experiment)
	}
	if snap2.ManifestRevision <= snap.ManifestRevision {
		t.Fatalf("manifest must advance: %d → %d", snap.ManifestRevision, snap2.ManifestRevision)
	}

	// Illegal transition is rejected.
	if _, err := f.svc.TransitionExperiment(f.publisher, created.ID, model.ExperimentReviewed, "back?"); !errors.Is(err, platform.ErrInvalidState) {
		t.Fatalf("running→reviewed must be rejected: %v", err)
	}
	// Pause republishes too.
	if _, err := f.svc.TransitionExperiment(f.owner, created.ID, model.ExperimentPaused, "guardrail scare"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	snap3, _ := f.svc.Config.LoadSnapshot(f.env.NamespaceID)
	if snap3.Flags["exp-flag"].Experiment.State != model.ExperimentPaused {
		t.Fatal("paused state not republished")
	}
}

func TestExperimentLayerSlotAssignment(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("layered"))

	mk := func() *platform.Experiment {
		return &platform.Experiment{
			FlagKey: "layered", Unit: "user_id",
			Variants:    []string{"on", "off"},
			Proportions: map[string]float64{"on": 50, "off": 50},
			PrimaryMetric: platform.MetricSpec{
				Key: "conv", Type: "proportion", Direction: "increase",
			},
			Alpha: 0.05, Eligibility: 50, Layer: "home-layer",
			MinSamplePerArm: 10, AnalysisWindow: 3600e9,
		}
	}
	e1, err := f.svc.CreateExperiment(f.analyst, f.ws.ID, f.env.ID, mk())
	if err != nil {
		t.Fatalf("create e1: %v", err)
	}
	e2, err := f.svc.CreateExperiment(f.analyst, f.ws.ID, f.env.ID, mk())
	if err != nil {
		t.Fatalf("create e2: %v", err)
	}
	if e1.LayerSlot == nil || e2.LayerSlot == nil {
		t.Fatal("slots must be assigned")
	}
	if e1.LayerSlot.Start == e2.LayerSlot.Start ||
		(e2.LayerSlot.Start < e1.LayerSlot.End && e2.LayerSlot.Start >= e1.LayerSlot.Start) {
		t.Fatalf("slots must be disjoint: %+v vs %+v", e1.LayerSlot, e2.LayerSlot)
	}
	if e1.Epoch == e2.Epoch {
		t.Fatalf("epochs must differ: %d", e1.Epoch)
	}
	// Layer full: a third 50% experiment cannot fit.
	e3, err := f.svc.CreateExperiment(f.analyst, f.ws.ID, f.env.ID, mk())
	if err == nil || !errors.Is(err, platform.ErrQuotaExceeded) {
		t.Fatalf("third 50%% experiment must not fit in layer: %+v %v", e3, err)
	}
}

func TestDebugEvaluateNoExposure(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("dbg"))

	res, snap, err := f.svc.DebugEvaluate(f.viewer, f.ws.ID, f.env.ID, "dbg",
		model.EvaluationContext{TargetingKey: "u1", Attributes: map[string]any{"user_id": "u1"}})
	if err != nil {
		t.Fatalf("DebugEvaluate: %v", err)
	}
	if res.TrackEvents || res.Experiment != nil {
		t.Fatalf("debugger must strip exposure: %+v", res)
	}
	if snap.ManifestRevision != 1 {
		t.Fatalf("snapshot revision = %d", snap.ManifestRevision)
	}
}

// TestConcurrentPublishesConverge proves the publish pipeline serialization:
// two flags published in parallel must BOTH appear in the final manifest/
// snapshot with distinct manifest revisions (no lost update).
func TestConcurrentPublishesConverge(t *testing.T) {
	f := newFixture(t)
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			key := fmtFlagKey(i)
			flag := testFlag(key)
			if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, flag, platform.RiskNormal, 0); err != nil {
				errs <- err
				return
			}
			if err := f.svc.SubmitFlagDraft(f.publisher, f.ws.ID, f.env.ID, key); err != nil {
				errs <- err
				return
			}
			if err := f.svc.ApproveFlagDraft(f.owner, f.ws.ID, f.env.ID, key, ""); err != nil {
				errs <- err
				return
			}
			if _, err := f.svc.PublishFlag(f.owner, f.ws.ID, f.env.ID, key, ""); err != nil {
				errs <- err
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent publish: %v", err)
		}
	}
	snap, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if len(snap.Flags) != n {
		t.Fatalf("snapshot has %d flags, want %d (lost update!): %v",
			len(snap.Flags), n, snapKeys(snap))
	}
	if snap.ManifestRevision != int64(n) {
		t.Fatalf("manifest revision = %d, want %d", snap.ManifestRevision, n)
	}
}

func fmtFlagKey(i int) string {
	return "flag-" + string(rune('a'+i))
}

func snapKeys(snap *model.Snapshot) []string {
	out := []string{}
	for k := range snap.Flags {
		out = append(out, k)
	}
	return out
}

func TestAuditTrailRecorded(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("audited"))
	entries := f.store.AuditList(f.ws.ID, 0)
	if len(entries) < 4 {
		t.Fatalf("expected save/submit/approve/publish audit entries, got %d", len(entries))
	}
	actions := map[string]bool{}
	for _, e := range entries {
		actions[e.Action] = true
	}
	for _, want := range []string{"flag.draft.save", "flag.draft.submit", "flag.draft.approve", "flag.publish"} {
		if !actions[want] {
			t.Errorf("audit action %q missing; have %v", want, actions)
		}
	}
}

func TestTokenScopingAndResolution(t *testing.T) {
	f := newFixture(t)
	ct, err := f.store.CreateToken(platform.TokenSDK, "prod-sdk", "", f.ws.ID, f.env.ID, "root")
	if err != nil {
		t.Fatalf("CreateToken sdk: %v", err)
	}
	if ct.Secret == "" || ct.Token.Prefix == "" {
		t.Fatal("secret must be returned once")
	}
	got, err := f.store.ResolveToken(ct.Secret)
	if err != nil || got.ID != ct.Token.ID {
		t.Fatalf("ResolveToken: %v", err)
	}
	// SDK tokens must be scoped; admin non-owner tokens too.
	if _, err := f.store.CreateToken(platform.TokenSDK, "bad", "", "", "", "root"); err == nil {
		t.Fatal("unscoped sdk token must be rejected")
	}
	if _, err := f.store.CreateToken(platform.TokenAdmin, "bad", platform.RolePublisher, "", "", "root"); err == nil {
		t.Fatal("global publisher token must be rejected")
	}
	// Revocation is immediate.
	if err := f.store.RevokeToken(ct.Token.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := f.store.ResolveToken(ct.Secret); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("revoked token must be rejected: %v", err)
	}
}

func TestNamespaceMappingGenerated(t *testing.T) {
	f := newFixture(t)
	if f.env.NamespaceID != "grayling-"+f.ws.ID+"-"+f.env.ID {
		t.Fatalf("namespace mapping = %q", f.env.NamespaceID)
	}
	// Two workspaces with same env slug get different namespaces.
	ws2, _ := f.store.CreateWorkspace("beta-co", "")
	env2, _ := f.store.CreateEnvironment(ws2.ID, "prod", "")
	if env2.NamespaceID == f.env.NamespaceID {
		t.Fatal("namespace collision across workspaces")
	}
}
