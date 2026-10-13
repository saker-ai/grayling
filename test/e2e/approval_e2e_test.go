package e2e

import (
	"net/http"
	"testing"

	"github.com/godeps/grayling/internal/platform"
)

func TestApprovalAPIRejectsScopeSelfEditAndRevocation(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()
	h.publishFlag(t, testFlag("approval-api"), platform.RiskNormal, h.reviewer)
	base := "/api/v1/admin/workspaces/acme/environments/prod"
	var request platform.Approval
	if code := h.do("POST", base+"/approvals", h.owner,
		map[string]any{"action": "flag.rollback", "object": "approval-api", "payload": map[string]any{"revision": 1}, "reason": "planned"},
		&request); code != http.StatusCreated {
		t.Fatalf("request: %d", code)
	}
	if request.PayloadHash == "" || request.RollbackTarget == "" || request.Guardrails == "" {
		t.Fatalf("incomplete preview: %+v", request)
	}
	approvePath := "/api/v1/admin/workspaces/acme/approvals/" + request.ID + "/approve"
	if code := h.do("POST", approvePath, h.owner, map[string]any{"note": "self"}, nil); code != http.StatusForbidden {
		t.Fatalf("self approval status: %d", code)
	}
	other, err := h.store.CreateWorkspace("other-approval", "Other")
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := h.store.CreateToken(platform.TokenAdmin, "other-owner", platform.RoleOwner, other.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if code := h.do("POST", approvePath, otherToken.Secret, map[string]any{"note": "cross"}, nil); code != http.StatusForbidden {
		t.Fatalf("cross-workspace approval status: %d", code)
	}
	if code := h.do("POST", approvePath, h.reviewer, map[string]any{"note": "checked"}, nil); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	rollback := base + "/flags/approval-api/rollback"
	if code := h.do("POST", rollback, h.owner,
		map[string]any{"revision": 2, "reason": "changed", "approvalId": request.ID}, nil); code != http.StatusConflict {
		t.Fatalf("changed target status: %d", code)
	}
	reviewerToken, err := h.store.ResolveToken(h.reviewer)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.RevokeToken(reviewerToken.ID); err != nil {
		t.Fatal(err)
	}
	if code := h.do("POST", rollback, h.owner,
		map[string]any{"revision": 1, "reason": "planned", "approvalId": request.ID}, nil); code != http.StatusForbidden {
		t.Fatalf("revoked approver status: %d", code)
	}
}

func TestEmergencyAPILeavesReviewAndAuditChain(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()
	h.publishFlag(t, testFlag("emergency-api"), platform.RiskNormal, h.reviewer)
	base := "/api/v1/admin/workspaces/acme/environments/prod/flags/emergency-api/rollback"
	var result struct {
		ReviewID         string `json:"reviewId"`
		ManifestRevision int64  `json:"manifestRevision"`
	}
	if code := h.do("POST", base, h.owner,
		map[string]any{"revision": 1, "reason": "incident", "emergency": true}, &result); code != http.StatusOK {
		t.Fatalf("emergency rollback: %d", code)
	}
	if result.ReviewID == "" || result.ManifestRevision == 0 {
		t.Fatalf("result: %+v", result)
	}
	var inbox []*platform.Approval
	if code := h.do("GET", "/api/v1/admin/workspaces/acme/approvals", h.reviewer, nil, &inbox); code != http.StatusOK {
		t.Fatalf("inbox: %d", code)
	}
	if len(inbox) != 1 || inbox[0].ID != result.ReviewID || inbox[0].State != platform.DraftSubmitted {
		t.Fatalf("pending review: %+v", inbox)
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/approvals/"+result.ReviewID+"/approve",
		h.reviewer, map[string]any{"note": "incident verified"}, nil); code != http.StatusOK {
		t.Fatalf("post-review: %d", code)
	}
	actions := map[string]bool{}
	for _, entry := range h.store.AuditList(h.ws.ID, 0) {
		actions[entry.Action] = true
	}
	for _, action := range []string{"flag.draft.submit", "flag.draft.approve", "flag.publish", "flag.rollback", "breakglass.open", "breakglass.review"} {
		if !actions[action] {
			t.Errorf("missing audit action %s", action)
		}
	}
}

func TestApprovedRollbackConsumesExactGrant(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()
	h.publishFlag(t, testFlag("planned-rollback"), platform.RiskNormal, h.reviewer)
	changed := testFlag("planned-rollback")
	changed.Variations["control"] = "changed-value"
	h.publishFlag(t, changed, platform.RiskNormal, h.reviewer)
	base := "/api/v1/admin/workspaces/acme/environments/prod"
	var request platform.Approval
	if code := h.do("POST", base+"/approvals", h.owner,
		map[string]any{"action": "flag.rollback", "object": "planned-rollback",
			"payload": map[string]any{"revision": 1}, "reason": "restore known version"}, &request); code != http.StatusCreated {
		t.Fatalf("request: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/approvals/"+request.ID+"/approve",
		h.reviewer, map[string]any{"note": "target verified"}, nil); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	var result struct {
		FlagRevision int64  `json:"flagRevision"`
		ReviewID     string `json:"reviewId"`
	}
	path := base + "/flags/planned-rollback/rollback"
	if code := h.do("POST", path, h.owner,
		map[string]any{"revision": 1, "reason": "restore known version", "approvalId": request.ID}, &result); code != http.StatusOK {
		t.Fatalf("rollback: %d", code)
	}
	if result.FlagRevision != 1 || result.ReviewID != "" {
		t.Fatalf("planned rollback result: %+v", result)
	}
	if code := h.do("POST", path, h.owner,
		map[string]any{"revision": 1, "reason": "replay", "approvalId": request.ID}, nil); code != http.StatusConflict {
		t.Fatalf("replayed grant: %d", code)
	}
}

func TestEmergencyDisablePublishesDisabledRevision(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()
	h.publishFlag(t, testFlag("disable-api"), platform.RiskNormal, h.reviewer)
	path := "/api/v1/admin/workspaces/acme/environments/prod/flags/disable-api/emergency-disable"
	if code := h.do("POST", path, h.reviewer, map[string]any{"reason": "incident"}, nil); code != http.StatusForbidden {
		t.Fatalf("non-owner emergency disable: %d", code)
	}
	var result struct {
		FlagRevision int64  `json:"flagRevision"`
		ReviewID     string `json:"reviewId"`
	}
	if code := h.do("POST", path, h.owner, map[string]any{"reason": "incident"}, &result); code != http.StatusOK {
		t.Fatalf("owner emergency disable: %d", code)
	}
	if result.FlagRevision != 2 || result.ReviewID == "" {
		t.Fatalf("result: %+v", result)
	}
	current := h.store.CurrentFlagContents(h.ws.ID, h.env.ID)["disable-api"]
	if current == nil || !current.Content.Disable {
		t.Fatalf("disabled revision: %+v", current)
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/approvals/"+result.ReviewID+"/approve",
		h.reviewer, map[string]any{"note": "verified"}, nil); code != http.StatusOK {
		t.Fatalf("post-review: %d", code)
	}
}
