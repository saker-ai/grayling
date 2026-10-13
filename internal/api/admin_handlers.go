package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/saker-ai/grayling/internal/admin"
	"github.com/saker-ai/grayling/internal/experiment"
	"github.com/saker-ai/grayling/internal/gffimport"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

// maxAdminBodyBytes bounds admin request payloads (§11: 单 flag 大小限额 is
// enforced inside the admin service against workspace quotas; this is the
// transport-level guard).
const maxAdminBodyBytes = 2 * 1024 * 1024

// ---------------------------------------------------------------------------
// Workspaces / environments / tokens
// ---------------------------------------------------------------------------

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	if err := admin.Authorize(actor, "", admin.PermWorkspaceAdmin); err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	ws, err := s.Store.CreateWorkspace(req.Slug, req.Name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	out := []*platform.Workspace{}
	for _, ws := range s.Store.ListWorkspaces() {
		// Scoped admins only see their own workspace.
		if actor.IsGlobalOwner() || actor.WorkspaceID == ws.ID {
			out = append(out, ws)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGlobalCapabilities supports workspace creation before any workspace exists.
func (s *Server) handleGlobalCapabilities(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	writeJSON(w, http.StatusOK, map[string]bool{
		string(admin.PermWorkspaceAdmin): admin.Authorize(actor, "", admin.PermWorkspaceAdmin) == nil,
	})
}

// handleWorkspaceCapabilities derives the console read model from the authenticated
// actor and resolved workspace. It never accepts a client-provided role.
func (s *Server) handleWorkspaceCapabilities(w http.ResponseWriter, r *http.Request) {
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
	permissions := make(map[admin.Permission]bool)
	for _, perm := range []admin.Permission{
		admin.PermWorkspaceAdmin, admin.PermTokenManage, admin.PermFlagRead,
		admin.PermFlagWrite, admin.PermFlagApprove, admin.PermFlagPublish,
		admin.PermExperimentWrite, admin.PermReportRead, admin.PermAIWrite,
		admin.PermAIContentRead, admin.PermQuotaManage,
	} {
		permissions[perm] = admin.Authorize(actor, ws.ID, perm) == nil
	}
	writeJSON(w, http.StatusOK, struct {
		WorkspaceID string                    `json:"workspaceId"`
		Permissions map[admin.Permission]bool `json:"permissions"`
	}{ws.ID, permissions})
}

func (s *Server) handleCreateEnvironment(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	ws := r.PathValue("ws")
	if err := admin.Authorize(actor, wsID(s, ws), admin.PermWorkspaceAdmin); err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	resolved, err := s.Store.GetWorkspace(ws)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	env, err := s.Store.CreateEnvironment(resolved.ID, req.Slug, req.Name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, env)
}

func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	resolved, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actor, resolved.ID, admin.PermFlagRead); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Store.ListEnvironments(resolved.ID))
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	resolved, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actor, resolved.ID, admin.PermTokenManage); err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Name        string             `json:"name"`
		Kind        platform.TokenKind `json:"kind"`
		Role        platform.Role      `json:"role"`
		Environment string             `json:"environment"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	envID := ""
	if req.Environment != "" {
		env, err := s.Store.GetEnvironment(resolved.ID, req.Environment)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		envID = env.ID
	}
	// Tokens are always scoped to the workspace in the path. Global owner
	// tokens are bootstrap-only (minted by cmd/golab), never via this
	// endpoint — a workspace owner cannot escalate to global.
	ct, err := s.Store.CreateToken(req.Kind, req.Name, req.Role, resolved.ID, envID, actor.Name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	_ = s.Store.Audit(actor.Name, actor.Role, "token.create", resolved.ID, envID, ct.Token.ID, "")
	writeJSON(w, http.StatusCreated, ct) // secret shown exactly once
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	resolved, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actor, resolved.ID, admin.PermTokenManage); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Store.ListTokens(resolved.ID))
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	var request struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &request, maxAdminBodyBytes); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	// Object-level check: a workspace-scoped owner may only revoke tokens
	// of their own workspace; global owners may revoke any.
	tokens := s.Store.ListTokens("")
	var target *platform.Token
	for _, tk := range tokens {
		if tk.ID == r.PathValue("id") {
			target = tk
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "not_found", "token not found")
		return
	}
	if target.WorkspaceID == "" && !actor.IsGlobalOwner() {
		// Global credentials can only be revoked by a global owner —
		// Authorize's scope check skips empty workspace IDs.
		writeStoreError(w, platform.ErrAccessDenied)
		return
	}
	if err := admin.Authorize(actor, target.WorkspaceID, admin.PermTokenManage); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.Store.RevokeToken(target.ID); err != nil {
		writeStoreError(w, err)
		return
	}
	_ = s.Store.Audit(actor.Name, actor.Role, "token.revoke", target.WorkspaceID, "", target.ID, request.Reason)
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	resolved, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actor, resolved.ID, admin.PermFlagRead); err != nil {
		writeStoreError(w, err)
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	var before int64
	if v := r.URL.Query().Get("before"); v != "" {
		before, err = strconv.ParseInt(v, 10, 64)
		if err != nil || before <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "before must be a positive audit ID")
			return
		}
	}
	query := r.URL.Query().Get("q")
	if len(query) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_query", "audit search exceeds 128 bytes")
		return
	}
	writeJSON(w, http.StatusOK, s.Store.AuditPage(resolved.ID, before, limit, query))
}

func (s *Server) handleSetQuotas(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	resolved, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actor, resolved.ID, admin.PermQuotaManage); err != nil {
		writeStoreError(w, err)
		return
	}
	var q platform.Quotas
	if err := decodeJSON(r, &q, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if q.MaxFlags <= 0 || q.MaxFlagBytes <= 0 || q.MaxSegments <= 0 ||
		q.MaxExperiments <= 0 || q.MaxEventBytes <= 0 || q.MaxEventsPerBatch <= 0 || q.EventRetention <= 0 {
		writeStoreError(w, fmt.Errorf("%w: all quota limits must be positive", platform.ErrValidation))
		return
	}
	if err := s.Store.SetQuotas(resolved.ID, q); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) handleGetQuotas(w http.ResponseWriter, r *http.Request) {
	resolved, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actorFrom(r), resolved.ID, admin.PermQuotaManage); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Store.GetQuotas(resolved.ID))
}

// wsID resolves a workspace path value to its surrogate ID for
// authorization before the object exists.
func wsID(s *Server, idOrSlug string) string {
	if ws, err := s.Store.GetWorkspace(idOrSlug); err == nil {
		return ws.ID
	}
	return idOrSlug
}

// ---------------------------------------------------------------------------
// Flags & segments
// ---------------------------------------------------------------------------

func (s *Server) handleListFlags(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	views, err := s.Admin.ListFlags(actor, r.PathValue("ws"), r.PathValue("env"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleGetFlag(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	view, err := s.Admin.GetFlagView(actor, r.PathValue("ws"), r.PathValue("env"),
		r.PathValue("key"), true)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleSaveDraft(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	var req struct {
		Content      *model.Flag   `json:"content"`
		Risk         platform.Risk `json:"risk"`
		BaseRevision int64         `json:"baseRevision"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.Content == nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "content required")
		return
	}
	// Path key is authoritative: a body key mismatch is a client bug, not
	// a rename (renames are delete+create in v1).
	key := r.PathValue("key")
	if req.Content.Key != key {
		writeError(w, http.StatusBadRequest, "validation_failed",
			"content.key "+req.Content.Key+" does not match path key "+key)
		return
	}
	draft, err := s.Admin.SaveFlagDraft(actor, r.PathValue("ws"), r.PathValue("env"),
		req.Content, req.Risk, req.BaseRevision)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func (s *Server) handleSubmitDraft(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.Reason == "" {
		writeStoreError(w, fmt.Errorf("%w: submission requires a reason", platform.ErrValidation))
		return
	}
	if err := s.Admin.SubmitFlagDraft(actorFrom(r), r.PathValue("ws"), r.PathValue("env"),
		r.PathValue("key"), req.Reason); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": string(platform.DraftSubmitted)})
}

func (s *Server) handleApproveDraft(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note string `json:"note"`
	}
	_ = decodeJSON(r, &req, maxAdminBodyBytes) // note optional
	if err := s.Admin.ApproveFlagDraft(actorFrom(r), r.PathValue("ws"), r.PathValue("env"),
		r.PathValue("key"), req.Note); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": string(platform.DraftApproved)})
}

func (s *Server) handleRejectDraft(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note string `json:"note"`
	}
	_ = decodeJSON(r, &req, maxAdminBodyBytes)
	if err := s.Admin.RejectFlagDraft(actorFrom(r), r.PathValue("ws"), r.PathValue("env"),
		r.PathValue("key"), req.Note); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": string(platform.DraftEditing)})
}

func (s *Server) handlePublishFlag(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note string `json:"note"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.Note == "" {
		writeStoreError(w, fmt.Errorf("%w: publish requires a note", platform.ErrValidation))
		return
	}
	res, err := s.Admin.PublishFlagContext(r.Context(), actorFrom(r), r.PathValue("ws"), r.PathValue("env"),
		r.PathValue("key"), req.Note)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision   int64  `json:"revision"`
		Reason     string `json:"reason"`
		ApprovalID string `json:"approvalId"`
		Emergency  bool   `json:"emergency"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.ApprovalID == "" && !req.Emergency {
		writeError(w, http.StatusBadRequest, "validation_failed", "approvalId or emergency=true required")
		return
	}
	res, err := s.Admin.RollbackFlagContext(r.Context(), actorFrom(r), r.PathValue("ws"), r.PathValue("env"),
		r.PathValue("key"), req.Revision, req.Reason, req.ApprovalID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleEmergencyDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	result, err := s.Admin.EmergencyDisableFlagContext(r.Context(), actorFrom(r), r.PathValue("ws"), r.PathValue("env"),
		r.PathValue("key"), req.Reason)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleMarkDeleted(w http.ResponseWriter, r *http.Request) {
	if err := s.Admin.MarkFlagDeleted(actorFrom(r), r.PathValue("ws"), r.PathValue("env"),
		r.PathValue("key")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "marked-deleted",
		"next":   "submit → approve → publish to tombstone",
	})
}

// handleImportGff converts a pasted gff document into a GoLab draft. The
// import is strict: unsupported fields are errors, never silent drops
// (§5.2). Warnings are returned alongside the draft.
func (s *Server) handleImportGff(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	var req struct {
		Document string `json:"document"` // YAML or JSON gff flag
		Key      string `json:"key"`
		Salt     string `json:"salt"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	ws := r.PathValue("ws")
	env := r.PathValue("env")
	flag, warnings, err := gffimport.ImportFlag([]byte(req.Document), gffimport.Options{
		FlagKey: req.Key, Salt: req.Salt,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "import_failed", err.Error())
		return
	}
	// Imports are HIGH risk by policy: the assignment algorithm changed, so
	// publishing one re-draws user assignments (§2, §5.2).
	draft, err := s.Admin.SaveFlagDraft(actor, ws, env, flag, platform.RiskHigh, 0)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"flag": flag, "warnings": warnings, "draft": draft,
		"note": "imported drafts are HIGH risk: GoLab's fixed-bucket algorithm does not reproduce gff assignments (§2)",
	})
}

func (s *Server) handleListSegments(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	resolved, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actor, resolved.ID, admin.PermFlagRead); err != nil {
		writeStoreError(w, err)
		return
	}
	env, err := s.Store.GetEnvironment(resolved.ID, r.PathValue("env"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Store.ListSegmentDrafts(env.WorkspaceID, env.ID))
}

func (s *Server) handleSaveSegmentDraft(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	var req struct {
		Content *model.Segment `json:"content"`
		Risk    platform.Risk  `json:"risk"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.Content == nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "content required")
		return
	}
	key := r.PathValue("key")
	if req.Content.Key != key {
		writeError(w, http.StatusBadRequest, "validation_failed", "content.key does not match path")
		return
	}
	draft, err := s.Admin.SaveSegmentDraft(actor, r.PathValue("ws"), r.PathValue("env"), req.Content, req.Risk)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func (s *Server) handleSubmitSegment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.Reason == "" {
		writeStoreError(w, fmt.Errorf("%w: submission requires a reason", platform.ErrValidation))
		return
	}
	if err := s.Admin.SubmitSegmentDraft(actorFrom(r), r.PathValue("ws"), r.PathValue("env"),
		r.PathValue("key"), req.Reason); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": string(platform.DraftSubmitted)})
}

func (s *Server) handleSegmentPreview(w http.ResponseWriter, r *http.Request) {
	p, err := s.Admin.GetSegmentPreview(actorFrom(r), r.PathValue("ws"), r.PathValue("env"), r.PathValue("key"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleApproveSegment(w http.ResponseWriter, r *http.Request) {
	if err := s.Admin.ApproveSegmentDraft(actorFrom(r), r.PathValue("ws"), r.PathValue("env"),
		r.PathValue("key")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": string(platform.DraftApproved)})
}

func (s *Server) handlePublishSegment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note string `json:"note"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if req.Note == "" {
		writeStoreError(w, fmt.Errorf("%w: publish requires a note", platform.ErrValidation))
		return
	}
	res, err := s.Admin.PublishSegmentContext(r.Context(), actorFrom(r), r.PathValue("ws"), r.PathValue("env"),
		r.PathValue("key"), req.Note)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------------------
// Experiments
// ---------------------------------------------------------------------------

func (s *Server) handleCreateExperiment(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	var exp platform.Experiment
	if err := decodeJSON(r, &exp, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	created, err := s.Admin.CreateExperiment(actor, r.PathValue("ws"), r.PathValue("env"), &exp)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleListExperiments(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	list, err := s.Admin.ListExperiments(actor, r.PathValue("ws"), r.PathValue("env"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetExperiment(w http.ResponseWriter, r *http.Request) {
	exp, err := s.Admin.GetExperiment(actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

func (s *Server) handleReviewExperiment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note string `json:"note"`
	}
	_ = decodeJSON(r, &req, maxAdminBodyBytes)
	exp, err := s.Admin.ReviewExperiment(actorFrom(r), r.PathValue("id"), req.Note)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

func (s *Server) handleBindExperiment(w http.ResponseWriter, r *http.Request) {
	draft, err := s.Admin.BindExperimentFlag(actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func (s *Server) handleTransitionExperiment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To     model.ExperimentState `json:"to"`
		Reason string                `json:"reason"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	exp, err := s.Admin.TransitionExperimentContext(r.Context(), actorFrom(r), r.PathValue("id"), req.To, req.Reason)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

func (s *Server) handleExperimentReport(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	exp, err := s.Admin.GetExperiment(actor, r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := admin.Authorize(actor, exp.WorkspaceID, admin.PermReportRead); err != nil {
		writeStoreError(w, err)
		return
	}
	rep, err := experiment.BuildReport(s.Store, exp.ID, time.Now())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// ---------------------------------------------------------------------------
// Debug
// ---------------------------------------------------------------------------

func (s *Server) handleDebugEval(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	var req struct {
		FlagKey string                  `json:"flagKey"`
		Context model.EvaluationContext `json:"context"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	res, snapshot, err := s.Admin.DebugEvaluateContext(r.Context(), actor, r.PathValue("ws"), r.PathValue("env"),
		req.FlagKey, req.Context)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"result":           res,
		"manifestRevision": snapshot.ManifestRevision,
		"note":             "debug evaluations never record exposures (§11)",
	})
}
