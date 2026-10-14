package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/saker-ai/grayling/internal/admin"
	aisvc "github.com/saker-ai/grayling/internal/ai"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

// SetAI attaches the AI resource gate. When unset, all AI endpoints answer
// 501 — the platform never pretends to broker resources it cannot verify.
func (s *Server) SetAI(ai *aisvc.Service) {
	s.ai = ai
	if ai != nil && s.Admin != nil {
		// modelRef variations must pass the workspace allowlist before any
		// flag revision is published (§7).
		s.Admin.FlagPublishHook = func(workspaceID string, content *model.Flag) error {
			if content == nil {
				return nil
			}
			return ai.ValidateFlagModelRefs(workspaceID, content.Variations)
		}
	}
}

func (s *Server) requireAI(w http.ResponseWriter) bool {
	if s.ai == nil {
		writeError(w, http.StatusNotImplemented, "ai_unavailable",
			"AI resource services are not wired into this server")
		return false
	}
	return true
}

// resolveScope authorizes the actor for one permission and resolves the
// (workspace, environment) path values to store objects.
func (s *Server) resolveScope(actor admin.Actor, wsRef, envRef string,
	perm admin.Permission) (*platform.Workspace, *platform.Environment, error) {

	ws, err := s.Store.GetWorkspace(wsRef)
	if err != nil {
		return nil, nil, err
	}
	if err := admin.Authorize(actor, ws.ID, perm); err != nil {
		return nil, nil, err
	}
	env, err := s.Store.GetEnvironment(ws.ID, envRef)
	if err != nil {
		return nil, nil, err
	}
	return ws, env, nil
}

// handleRevokeAIResource is the emergency stop: takes effect immediately on
// the resolve/pre-execution path, independent of snapshot sync (§7).
func (s *Server) handleRevokeAIResource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAI(w) {
		return
	}
	actor := actorFrom(r)
	ws, env, err := s.resolveScope(actor, r.PathValue("ws"), r.PathValue("env"), admin.PermAIWrite)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if actor.Role != platform.RoleOwner {
		writeStoreError(w, platform.ErrAccessDenied)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "validation_failed", "revocation requires a reason")
		return
	}
	typ, logical := r.PathValue("type"), r.PathValue("logical")
	if err := s.ai.Revoke(ws.ID, env.ID, typ, logical, req.Reason, actor.Name); err != nil {
		writeStoreError(w, err)
		return
	}
	hash, err := approvalHash(struct {
		Reason string `json:"reason"`
	}{req.Reason})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	review, err := s.Store.RequestApproval(platform.Approval{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, Action: "breakglass.review",
		Object: typ + "/" + logical, PayloadHash: hash,
		BaseRevision: int64(len(s.Store.ListPinnedVersions(ws.ID, env.ID, typ, logical))),
		Reason:       req.Reason, Impact: "immediate denial of AI resource resolution and execution",
		RollbackTarget: "resource reinstatement after incident review",
		Guardrails:     "runtime revocation check; independent post-action review",
		SubmittedBy:    actor.Name, SubmittedTokenID: actor.TokenID,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	_ = s.Store.Audit(actor.Name+" ("+actor.TokenID+")", actor.Role, "breakglass.open", ws.ID, env.ID,
		review.ID, fmt.Sprintf(`{"action":"ai.revoke","object":%q,"reason":%q}`, typ+"/"+logical, req.Reason))
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "type": typ, "logicalId": logical, "reviewId": review.ID})
}

// handleReinstateAIResource lifts a revocation.
func (s *Server) handleReinstateAIResource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAI(w) {
		return
	}
	actor := actorFrom(r)
	ws, env, err := s.resolveScope(actor, r.PathValue("ws"), r.PathValue("env"), admin.PermAIWrite)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if actor.Role != platform.RoleOwner {
		writeStoreError(w, platform.ErrAccessDenied)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.Reason == "" {
		writeStoreError(w, fmt.Errorf("%w: reinstatement requires a reason", platform.ErrValidation))
		return
	}
	typ, logical := r.PathValue("type"), r.PathValue("logical")
	for _, review := range s.Store.ListApprovals(ws.ID) {
		if review.EnvironmentID == env.ID && review.Action == "breakglass.review" &&
			review.Object == typ+"/"+logical && review.State == platform.DraftSubmitted {
			writeStoreError(w, fmt.Errorf("%w: emergency revocation requires independent review", platform.ErrInvalidState))
			return
		}
	}
	if err := s.ai.Reinstate(ws.ID, env.ID, typ, logical); err != nil {
		writeStoreError(w, err)
		return
	}
	_ = s.Store.Audit(actor.Name+" ("+actor.TokenID+")", actor.Role, "ai.reinstate", ws.ID, env.ID,
		typ+"/"+logical, fmt.Sprintf(`{"reason":%q}`, req.Reason))
	writeJSON(w, http.StatusOK, map[string]any{"status": "reinstated"})
}

// ---------------------------------------------------------------------------
// Admin AI endpoints
// ---------------------------------------------------------------------------

func (s *Server) handleRegisterAIResource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAI(w) {
		return
	}
	actor := actorFrom(r)
	ws, env, err := s.resolveScope(actor, r.PathValue("ws"), r.PathValue("env"), admin.PermAIWrite)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Type      string `json:"type"`
		LogicalID string `json:"logicalId"`
		Name      string `json:"name"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	ref, err := s.ai.RegisterResource(ws.ID, env.ID, req.Type, req.LogicalID, req.Name, actor.Name)
	if err != nil {
		writeStoreError(w, mapAIError(err))
		return
	}
	_ = s.Store.Audit(actor.Name, actor.Role, "ai.register", ws.ID, env.ID,
		req.Type+"/"+req.LogicalID, ref.PhysicalID)
	writeJSON(w, http.StatusCreated, ref)
}

func (s *Server) handleListAIResources(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	ws, env, err := s.resolveScope(actor, r.PathValue("ws"), r.PathValue("env"), admin.PermFlagRead)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resources":   s.Store.ListAIResources(ws.ID, env.ID),
		"revocations": s.Store.ListAIRevocations(ws.ID, env.ID),
	})
}

type aiResourceDetail struct {
	Resource         *platform.AIResourceRef   `json:"resource"`
	Pins             []*platform.PinnedVersion `json:"pins"`
	Revoked          bool                      `json:"revoked"`
	RevocationReason string                    `json:"revocationReason,omitempty"`
	SourceMode       string                    `json:"sourceMode"`
}

// handleGetAIResource returns only metadata. Raw content has a separate grant.
func (s *Server) handleGetAIResource(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	ws, env, err := s.resolveScope(actor, r.PathValue("ws"), r.PathValue("env"), admin.PermFlagRead)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	typ, logical := r.PathValue("type"), r.PathValue("logical")
	ref, err := s.Store.GetAIResource(ws.ID, env.ID, typ, logical)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	pins := s.Store.ListPinnedVersions(ws.ID, env.ID, typ, logical)
	slices.Reverse(pins)
	reason, revoked := s.Store.IsAIRevoked(ws.ID, env.ID, typ, logical)
	mode := "unavailable"
	if s.ai != nil {
		if s.ai.External != nil {
			mode = "external-config"
		} else if s.ai.Gonacos != nil {
			mode = "embedded-gonacos"
		}
	}
	writeJSON(w, http.StatusOK, aiResourceDetail{
		Resource: ref, Pins: pins, Revoked: revoked, RevocationReason: reason, SourceMode: mode,
	})
}

func (s *Server) handlePinAIVersion(w http.ResponseWriter, r *http.Request) {
	if !s.requireAI(w) {
		return
	}
	actor := actorFrom(r)
	ws, env, err := s.resolveScope(actor, r.PathValue("ws"), r.PathValue("env"), admin.PermAIWrite)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Content    string `json:"content"`
		ApprovalID string `json:"approvalId"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	typ, logical := r.PathValue("type"), r.PathValue("logical")
	if err := admin.Authorize(actor, ws.ID, admin.PermFlagPublish); err != nil {
		writeStoreError(w, err)
		return
	}
	var pin *platform.PinnedVersion
	err = s.Admin.WithScopeLock(ws.ID, env.ID, func() error {
		base := int64(len(s.Store.ListPinnedVersions(ws.ID, env.ID, typ, logical)))
		if err := s.useApproval(actor, req.ApprovalID, ws.ID, env.ID, "ai.pin", typ+"/"+logical,
			struct {
				Content string `json:"content"`
			}{req.Content}, base); err != nil {
			return err
		}
		var err error
		pin, err = s.ai.PublishVersionContext(r.Context(), ws.ID, env.ID, typ, logical, req.Content, actor.Name)
		return err
	})
	if err != nil {
		writeStoreError(w, mapAIError(err))
		return
	}
	_ = s.Store.Audit(actor.Name, actor.Role, "ai.publish", ws.ID, env.ID,
		typ+"/"+logical, pin.Version+"@"+pin.Digest)
	writeJSON(w, http.StatusCreated, pin)
}

// handleResolveAIResource (admin): raw content read requires the separate
// ai.content.read grant — owner role only (§9: 读取原始 AI 内容单独授权).
func (s *Server) handleResolveAIResource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAI(w) {
		return
	}
	actor := actorFrom(r)
	ws, env, err := s.resolveScope(actor, r.PathValue("ws"), r.PathValue("env"), admin.PermAIContentRead)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	q := r.URL.Query()
	content, err := s.ai.ResolveContext(r.Context(), ws.ID, env.ID, q.Get("type"), q.Get("logicalId"), q.Get("version"))
	if err != nil {
		writeStoreError(w, mapAIError(err))
		return
	}
	writeJSON(w, http.StatusOK, content)
}

func (s *Server) handleSetModelAllowlist(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	ws, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actor, ws.ID, admin.PermAIContentRead); err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Entries []platform.ModelAllowEntry `json:"entries"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.Store.SetModelAllowlist(ws.ID, req.Entries); err != nil {
		writeStoreError(w, err)
		return
	}
	_ = s.Store.Audit(actor.Name, actor.Role, "ai.allowlist.set", ws.ID, "", "model-allowlist", "")
	writeJSON(w, http.StatusOK, req.Entries)
}

func (s *Server) handleGetModelAllowlist(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, s.Store.ListModelAllowlist(ws.ID))
}

// ---------------------------------------------------------------------------
// SDK AI endpoints (server-side SDK credentials; §7 forbids sending raw AI
// content to browsers — SDK tokens are backend credentials)
// ---------------------------------------------------------------------------

type resourceRequest struct {
	Type      string `json:"type"`
	LogicalID string `json:"logicalId"`
	Version   string `json:"version,omitempty"`
}

func (s *Server) handleSDKAIResolve(w http.ResponseWriter, r *http.Request) {
	if !s.requireAI(w) {
		return
	}
	tok := tokenFrom(r)
	env, err := s.Store.GetEnvironment(tok.WorkspaceID, tok.EnvironmentID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req resourceRequest
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	// The scope comes from the token; the request can only name resources
	// registered in the token's own environment.
	content, err := s.ai.ResolveContext(r.Context(), env.WorkspaceID, env.ID, req.Type, req.LogicalID, req.Version)
	if err != nil {
		writeStoreError(w, mapAIError(err))
		return
	}
	writeJSON(w, http.StatusOK, content)
}

// sessionFreezeRequest asks Grayling to evaluate flags once and freeze
// resource versions for the lifetime of a conversation/task (§7).
type sessionFreezeRequest struct {
	SessionID string                  `json:"sessionId"`
	TaskID    string                  `json:"taskId,omitempty"`
	Context   model.EvaluationContext `json:"context"`
	FlagKeys  []string                `json:"flagKeys"`
	// Resources explicitly requested in addition to auto-discovery from
	// served variation values (promptRef/resourceRef objects).
	Resources []resourceRequest `json:"resources,omitempty"`
}

// SessionSnapshot is the frozen per-task configuration. Long-running tasks
// carry it in their context; new configuration only affects subsequent
// sessions (§7).
type SessionSnapshot struct {
	SessionID        string                     `json:"sessionId"`
	TaskID           string                     `json:"taskId,omitempty"`
	WorkspaceID      string                     `json:"workspaceId"`
	EnvironmentID    string                     `json:"environmentId"`
	ManifestRevision int64                      `json:"manifestRevision"`
	FrozenAt         time.Time                  `json:"frozenAt"`
	Evaluations      map[string]model.Result    `json:"evaluations"`
	Resources        []aisvc.Content            `json:"resources"`
	ModelRefs        []platform.ModelAllowEntry `json:"modelRefs"`
}

func (s *Server) handleSDKAISession(w http.ResponseWriter, r *http.Request) {
	if !s.requireAI(w) {
		return
	}
	tok := tokenFrom(r)
	env, err := s.Store.GetEnvironment(tok.WorkspaceID, tok.EnvironmentID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req sessionFreezeRequest
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, "validation_failed", "sessionId required")
		return
	}
	ev, snapshot, revision, err := s.evaluatorFor(r.Context(), env)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	frozen := &SessionSnapshot{
		SessionID: req.SessionID, TaskID: req.TaskID,
		WorkspaceID: env.WorkspaceID, EnvironmentID: env.ID,
		ManifestRevision: revision, FrozenAt: time.Now().UTC(),
		Evaluations: map[string]model.Result{},
	}

	// 1. Evaluate every requested flag once, capturing exposures.
	seen := map[string]bool{}
	for _, key := range req.FlagKeys {
		res := ev.Evaluate(key, req.Context)
		frozen.Evaluations[key] = res
		if res.TrackEvents && res.Experiment != nil {
			s.recordRemoteExposure(tok, env, snapshot,
				evalRequest{FlagKey: key, Context: req.Context}, res)
		}
		// 2. Auto-discover resource refs inside the served variation value.
		if !res.Failed {
			for _, rr := range discoverResourceRefs(res.Value) {
				if seen[rr.Type+"/"+rr.LogicalID+"@"+rr.Version] {
					continue
				}
				seen[rr.Type+"/"+rr.LogicalID+"@"+rr.Version] = true
				req.Resources = append(req.Resources, rr)
			}
			for _, mr := range discoverModelRefs(res.Value) {
				entry, err := s.ai.ResolveModelRef(env.WorkspaceID, mr.provider, mr.model)
				if err != nil {
					writeStoreError(w, mapAIError(err))
					return
				}
				frozen.ModelRefs = append(frozen.ModelRefs, entry)
			}
		}
	}

	// 3. Resolve + digest-verify every resource; ANY failure fails the
	// freeze — a task must never start on partially resolved config (§7:
	// 资源缺失安全降级 = do not run with stale/partial bindings).
	for _, rr := range req.Resources {
		content, err := s.ai.ResolveContext(r.Context(), env.WorkspaceID, env.ID, rr.Type, rr.LogicalID, rr.Version)
		if err != nil {
			writeStoreError(w, mapAIError(err))
			return
		}
		frozen.Resources = append(frozen.Resources, *content)
	}
	writeJSON(w, http.StatusOK, frozen)
}

// handleSDKAICheck is the immediate pre-execution revocation gate (§7:
// 紧急安全撤销需另设可即时阻断的执行前校验，不依赖普通快照). Long tasks
// call it right before executing each frozen resource.
func (s *Server) handleSDKAICheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireAI(w) {
		return
	}
	tok := tokenFrom(r)
	env, err := s.Store.GetEnvironment(tok.WorkspaceID, tok.EnvironmentID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Resources []resourceRequest `json:"resources"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	type status struct {
		Type      string `json:"type"`
		LogicalID string `json:"logicalId"`
		Blocked   bool   `json:"blocked"`
		Reason    string `json:"reason,omitempty"`
	}
	out := make([]status, 0, len(req.Resources))
	anyBlocked := false
	for _, rr := range req.Resources {
		reason, blocked := s.ai.CheckRevocation(env.WorkspaceID, env.ID, rr.Type, rr.LogicalID)
		if blocked {
			anyBlocked = true
		}
		out = append(out, status{Type: rr.Type, LogicalID: rr.LogicalID, Blocked: blocked, Reason: reason})
	}
	code := http.StatusOK
	if anyBlocked {
		code = http.StatusLocked // 423: resource revoked, execution must stop
	}
	writeJSON(w, code, map[string]any{"resources": out, "anyBlocked": anyBlocked})
}

// ---------------------------------------------------------------------------
// Variation-value ref discovery
// ---------------------------------------------------------------------------

type modelRef struct{ provider, model string }

// discoverResourceRefs finds {"resourceRef"|"promptRef": {type, logicalId,
// version?}} objects anywhere inside a variation value.
func discoverResourceRefs(value any) []resourceRequest {
	var out []resourceRequest
	walkJSON(value, func(obj map[string]any) {
		for _, key := range []string{"resourceRef", "promptRef"} {
			if ref, ok := obj[key].(map[string]any); ok {
				typ, _ := ref["type"].(string)
				logical, _ := ref["logicalId"].(string)
				if typ == "" && key == "promptRef" {
					typ = platform.AITypePrompt
				}
				if typ != "" && logical != "" {
					ver, _ := ref["version"].(string)
					out = append(out, resourceRequest{Type: typ, LogicalID: logical, Version: ver})
				}
			}
		}
	})
	return out
}

// discoverModelRefs finds {"modelRef": {provider, model}} objects.
func discoverModelRefs(value any) []modelRef {
	var out []modelRef
	walkJSON(value, func(obj map[string]any) {
		if ref, ok := obj["modelRef"].(map[string]any); ok {
			provider, _ := ref["provider"].(string)
			m, _ := ref["model"].(string)
			if provider != "" && m != "" {
				out = append(out, modelRef{provider: provider, model: m})
			}
		}
	})
	return out
}

func walkJSON(v any, visit func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		visit(t)
		for _, child := range t {
			walkJSON(child, visit)
		}
	case []any:
		for _, child := range t {
			walkJSON(child, visit)
		}
	}
}

// mapAIError translates AI gate errors onto HTTP semantics. Cross-scope
// lookups answer 404 (never 403 — existence must not leak, §5.1).
func mapAIError(err error) error {
	switch {
	case errors.Is(err, aisvc.ErrUnauthorized), errors.Is(err, aisvc.ErrVersionNotPinned):
		return platform.ErrNotFound
	case errors.Is(err, aisvc.ErrRevoked):
		return errRevoked
	case errors.Is(err, aisvc.ErrDigestDrift):
		return errDigestDrift
	case errors.Is(err, aisvc.ErrModelNotAllowed), errors.Is(err, aisvc.ErrTypeUnsupported):
		return errAIValidation
	}
	return err
}

var (
	errRevoked      = revokedError{}
	errDigestDrift  = digestDriftError{}
	errAIValidation = aiValidationError{}
)

type revokedError struct{}

func (revokedError) Error() string { return "resource is revoked" }
func (revokedError) Is(target error) bool {
	return target == platform.ErrInvalidState
}

type digestDriftError struct{}

func (digestDriftError) Error() string { return "published content digest changed" }
func (digestDriftError) Is(target error) bool {
	return target == platform.ErrConflict
}

type aiValidationError struct{}

func (aiValidationError) Error() string { return "ai validation failed" }
func (aiValidationError) Is(target error) bool {
	return target == platform.ErrValidation
}
