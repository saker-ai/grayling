package api

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/godeps/golab/internal/admin"
	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/pkg/model"
)

type approvalProposal struct {
	Action  string         `json:"action"`
	Object  string         `json:"object"`
	Payload jsontext.Value `json:"payload"`
	Reason  string         `json:"reason"`
}

func approvalHash(v any) (string, error) {
	raw, err := model.CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return model.HashBytes(raw), nil
}

// approvalBasis computes the server-owned diff context. The same function is
// used at request and execution, so a changed object invalidates review.
func (s *Server) approvalBasis(wsID, envID, action, object string, payload jsontext.Value) (hash string, base int64, impact, rollback, guardrails string, err error) {
	switch action {
	case "release_plan.create":
		var body struct {
			Steps []platform.ReleaseStep `json:"steps"`
		}
		// ReleaseStep uses nanosecond time.Duration fields. Match the
		// existing release-plan API's v1 JSON number representation.
		if err = jsonv1.Unmarshal(payload, &body); err != nil {
			return
		}
		if err = admin.ValidateReleaseSteps(body.Steps); err != nil {
			return
		}
		var exp *platform.Experiment
		exp, err = s.Store.GetExperiment(object)
		if err != nil {
			return
		}
		if exp.WorkspaceID != wsID || exp.EnvironmentID != envID {
			err = platform.ErrAccessDenied
			return
		}
		if exp.ReleasePlan != nil {
			err = platform.ErrConflict
			return
		}
		base = s.Store.CurrentFlagRevision(wsID, envID, exp.FlagKey)
		impact = fmt.Sprintf("maximum eligibility %.2f%% of experiment units", body.Steps[len(body.Steps)-1].Ratio)
		rollback = fmt.Sprintf("flag %s revision %d", exp.FlagKey, base)
		guardrails = "pre-registered experiment decision gates and SRM checks"
		hash, err = approvalHash(body)
	case "ai.pin":
		typ, logical, ok := strings.Cut(object, "/")
		if !ok {
			err = platform.ErrValidation
			return
		}
		if _, err = s.Store.GetAIResource(wsID, envID, typ, logical); err != nil {
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		if err = json.Unmarshal(payload, &body); err != nil {
			return
		}
		if body.Content == "" {
			err = platform.ErrValidation
			return
		}
		pins := s.Store.ListPinnedVersions(wsID, envID, typ, logical)
		base = int64(len(pins))
		impact = "new immutable AI resource version may be selected by future publishes"
		if len(pins) > 0 {
			rollback = pins[len(pins)-1].Version
		} else {
			rollback = "no prior pin"
		}
		guardrails = "digest pin and model allowlist validation"
		hash, err = approvalHash(body)
	case "flag.rollback":
		var body struct {
			Revision int64 `json:"revision"`
		}
		if err = json.Unmarshal(payload, &body); err != nil {
			return
		}
		var target *platform.FlagRevision
		target, err = s.Store.GetFlagRevision(wsID, envID, object, body.Revision)
		if err != nil {
			return
		}
		base = s.Store.CurrentFlagRevision(wsID, envID, object)
		impact = "all users evaluated by this flag"
		rollback = fmt.Sprintf("flag %s revision %d (%s)", object, body.Revision, target.Hash)
		guardrails = "retained target document and SHA-256 verified before switch"
		hash, err = approvalHash(body)
	default:
		err = fmt.Errorf("%w: unsupported approval action %q", platform.ErrValidation, action)
	}
	return
}

func (s *Server) handleRequestApproval(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	ws, env, err := s.resolveScope(actor, r.PathValue("ws"), r.PathValue("env"), admin.PermFlagPublish)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req approvalProposal
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.Action == "ai.pin" {
		if err := admin.Authorize(actor, ws.ID, admin.PermAIWrite); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	hash, base, impact, rollback, guardrails, err := s.approvalBasis(ws.ID, env.ID, req.Action, req.Object, req.Payload)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	storedPayload := ""
	if req.Action != "ai.pin" {
		storedPayload = string(req.Payload)
	}
	a, err := s.Store.RequestApproval(platform.Approval{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, Action: req.Action, Object: req.Object,
		PayloadHash: hash, BaseRevision: base, Reason: req.Reason, Impact: impact,
		RollbackTarget: rollback, Guardrails: guardrails, Payload: storedPayload, SubmittedBy: actor.Name,
		SubmittedTokenID: actor.TokenID,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	_ = s.Store.Audit(actor.Name+" ("+actor.TokenID+")", actor.Role, "approval.request", ws.ID, env.ID, a.ID,
		fmt.Sprintf(`{"action":%q,"object":%q,"hash":%q,"baseRevision":%d,"reason":%q}`, a.Action, a.Object, a.PayloadHash, a.BaseRevision, a.Reason))
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	ws, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actor, ws.ID, admin.PermFlagRead); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Store.ListApprovals(ws.ID))
}

func (s *Server) handleApproveChange(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	ws, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	a, err := s.Store.GetApproval(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if a.WorkspaceID != ws.ID {
		writeStoreError(w, platform.ErrAccessDenied)
		return
	}
	if err := admin.Authorize(actor, ws.ID, admin.PermFlagApprove); err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	a, err = s.Store.ApproveChange(a.ID, actor.Name, actor.TokenID, req.Note)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	action := "approval.approve"
	if a.Action == "breakglass.review" {
		action = "breakglass.review"
	}
	_ = s.Store.Audit(actor.Name+" ("+actor.TokenID+")", actor.Role, action, a.WorkspaceID, a.EnvironmentID, a.ID,
		fmt.Sprintf(`{"hash":%q,"baseRevision":%d,"expiresAt":%q,"note":%q}`, a.PayloadHash, a.BaseRevision, a.ExpiresAt.Format(time.RFC3339), req.Note))
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) useApproval(actor admin.Actor, id, wsID, envID, action, object string, payload any, base int64) error {
	a, err := s.Store.GetApproval(id)
	if err != nil {
		return err
	}
	if err := s.Admin.CheckApprover(a.ApprovedTokenID, wsID); err != nil {
		return err
	}
	hash, err := approvalHash(payload)
	if err != nil {
		return err
	}
	_, err = s.Store.UseApproval(id, wsID, envID, action, object, hash, base)
	if err != nil {
		return err
	}
	return s.Store.Audit(actor.Name+" ("+actor.TokenID+")", actor.Role, "approval.use", wsID, envID, id,
		fmt.Sprintf(`{"action":%q,"object":%q,"hash":%q,"baseRevision":%d}`, action, object, hash, base))
}
