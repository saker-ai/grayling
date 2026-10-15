package admin

import (
	"errors"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
)

func TestDraftApprovalSelfApprovalChangeExpiryAndRevocation(t *testing.T) {
	f := newFixture(t)
	key := "reviewed"
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, testFlag(key), platform.RiskNormal, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SubmitFlagDraft(f.publisher, f.ws.ID, f.env.ID, key); err != nil {
		t.Fatal(err)
	}
	// The submitter may approve their own draft, whatever the risk level.
	if err := f.svc.ApproveFlagDraft(f.publisher, f.ws.ID, f.env.ID, key, "self"); err != nil {
		t.Fatalf("self approval must be allowed: %v", err)
	}
	d, _ := f.store.GetFlagDraft(f.ws.ID, f.env.ID, key)
	if d.State != platform.DraftApproved || d.ApprovedBy != f.publisher.Name ||
		d.ApprovedTokenID != f.publisher.TokenID {
		t.Fatalf("self-approved draft: %+v", d)
	}
	// A fresh save starts the state machine again and binds a new hash.
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, testFlag(key), platform.RiskNormal, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SubmitFlagDraft(f.publisher, f.ws.ID, f.env.ID, key); err != nil {
		t.Fatal(err)
	}
	d, _ = f.store.GetFlagDraft(f.ws.ID, f.env.ID, key)
	d.Content.Variations["on"] = false
	if err := f.svc.ApproveFlagDraft(f.publisher, f.ws.ID, f.env.ID, key, "changed"); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("edited submitted bytes: %v", err)
	}
	// A fresh save again starts the state machine and binds a new hash.
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, testFlag(key), platform.RiskNormal, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SubmitFlagDraft(f.publisher, f.ws.ID, f.env.ID, key); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ApproveFlagDraft(f.owner, f.ws.ID, f.env.ID, key, "ok"); err != nil {
		t.Fatal(err)
	}
	d, _ = f.store.GetFlagDraft(f.ws.ID, f.env.ID, key)
	d.Content.Variations["on"] = false
	if _, err := f.svc.PublishFlag(f.publisher, f.ws.ID, f.env.ID, key, "changed"); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("edited approved bytes: %v", err)
	}
	d.Content.Variations["on"] = true
	*d.ApprovalExpiresAt = time.Now().Add(-time.Minute)
	if _, err := f.svc.PublishFlag(f.publisher, f.ws.ID, f.env.ID, key, "expired"); !errors.Is(err, platform.ErrInvalidState) {
		t.Fatalf("expired approval: %v", err)
	}
	*d.ApprovalExpiresAt = time.Now().Add(time.Hour)
	if err := f.store.RevokeToken(f.owner.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PublishFlag(f.publisher, f.ws.ID, f.env.ID, key, "revoked"); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("revoked approver: %v", err)
	}
}

func TestEmergencyRollbackRequiresIndependentPostReview(t *testing.T) {
	f := newFixture(t)
	first := fullPublishFlow(t, f, f.publisher, f.owner, testFlag("emergency"))
	second := testFlag("emergency")
	second.Variations["on"] = false
	fullPublishFlow(t, f, f.publisher, f.owner, second)
	if _, err := f.svc.RollbackFlag(f.publisher, f.ws.ID, f.env.ID, "emergency", first.FlagRevision, "incident"); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("publisher break-glass: %v", err)
	}
	res, err := f.svc.RollbackFlag(f.owner, f.ws.ID, f.env.ID, "emergency", first.FlagRevision, "incident")
	if err != nil {
		t.Fatal(err)
	}
	if res.ReviewID == "" {
		t.Fatal("emergency action did not create post-review")
	}
	a, err := f.store.GetApproval(res.ReviewID)
	if err != nil || a.State != platform.DraftSubmitted || a.BaseRevision != res.ManifestRevision {
		t.Fatalf("post-review record: %+v, %v", a, err)
	}
	// Independence is the whole point of an emergency post-review, so this
	// is the one action that still refuses a self-review.
	if _, err := f.store.ApproveChange(a.ID, f.owner.Name, f.owner.TokenID, "self"); !errors.Is(err, platform.ErrSelfApproval) {
		t.Fatalf("self post-review: %v", err)
	}
	// Matching either identity is enough to be a self-review: the same
	// credential under another display name is still refused.
	if _, err := f.store.ApproveChange(a.ID, "someone-else", f.owner.TokenID, "self"); !errors.Is(err, platform.ErrSelfApproval) {
		t.Fatalf("self post-review by token id: %v", err)
	}
	if _, err := f.store.ApproveChange(a.ID, f.owner.Name, "other-token", "self"); !errors.Is(err, platform.ErrSelfApproval) {
		t.Fatalf("self post-review by name: %v", err)
	}
	// A reviewer who did not open the emergency action can still verify it.
	verified, err := f.store.ApproveChange(a.ID, f.publisher2.Name, f.publisher2.TokenID, "verified")
	if err != nil {
		t.Fatal(err)
	}
	if verified.State != platform.DraftApproved || verified.ApprovedBy != f.publisher2.Name ||
		verified.ApprovedTokenID != f.publisher2.TokenID {
		t.Fatalf("independent post-review: %+v", verified)
	}
	actions := map[string]bool{}
	for _, entry := range f.store.AuditList(f.ws.ID, 0) {
		actions[entry.Action] = true
	}
	for _, action := range []string{"flag.draft.submit", "flag.draft.approve", "flag.publish", "flag.rollback", "breakglass.open"} {
		if !actions[action] {
			t.Errorf("audit chain lacks %s", action)
		}
	}
}

func TestCrossWorkspaceApprovalIsDenied(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, testFlag("scoped"), platform.RiskNormal, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SubmitFlagDraft(f.publisher, f.ws.ID, f.env.ID, "scoped"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ApproveFlagDraft(f.otherWS, f.ws.ID, f.env.ID, "scoped", "other tenant"); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("cross-workspace approval: %v", err)
	}
}
