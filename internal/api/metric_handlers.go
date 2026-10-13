package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/saker-ai/grayling/internal/admin"
	"github.com/saker-ai/grayling/internal/platform"
)

func (s *Server) metricWorkspace(r *http.Request, permission admin.Permission) (*platform.Workspace, error) {
	ws, err := s.Store.GetWorkspace(r.PathValue("ws"))
	if err != nil {
		return nil, err
	}
	if err := admin.Authorize(actorFrom(r), ws.ID, permission); err != nil {
		return nil, err
	}
	return ws, nil
}

func metricPathRef(r *http.Request) (platform.MetricRef, error) {
	version, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || version <= 0 {
		return platform.MetricRef{}, fmt.Errorf("%w: invalid metric version", platform.ErrValidation)
	}
	return platform.MetricRef{Key: r.PathValue("key"), Version: version}, nil
}

func (s *Server) handleCreateMetricDefinition(w http.ResponseWriter, r *http.Request) {
	ws, err := s.metricWorkspace(r, admin.PermExperimentWrite)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var def platform.MetricDefinition
	if err := decodeJSON(r, &def, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	def.WorkspaceID = ws.ID
	def.CreatedBy = actorFrom(r).Name
	created, err := s.Store.CreateMetricDefinition(&def)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	_ = s.Store.Audit(actorFrom(r).Name, actorFrom(r).Role, "metric.create", ws.ID, "",
		fmt.Sprintf("%s/v%d", created.Key, created.Version), created.Hash)
	writeJSON(w, http.StatusCreated, created)
}

func metricCursor(def *platform.MetricDefinition) string {
	return base64.RawURLEncoding.EncodeToString([]byte(def.Key + "\x00" + strconv.FormatInt(def.Version, 10)))
}

func (s *Server) handleListMetricDefinitions(w http.ResponseWriter, r *http.Request) {
	ws, err := s.metricWorkspace(r, admin.PermReportRead)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			writeStoreError(w, fmt.Errorf("%w: limit must be 1..200", platform.ErrValidation))
			return
		}
		limit = parsed
	}
	definitions := s.Store.ListMetricDefinitions(ws.ID)
	status := r.URL.Query().Get("status")
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if status != "" && status != string(platform.MetricDraft) &&
		status != string(platform.MetricPublished) && status != string(platform.MetricDeprecated) {
		writeStoreError(w, fmt.Errorf("%w: invalid metric status", platform.ErrValidation))
		return
	}
	filtered := make([]*platform.MetricDefinition, 0, len(definitions))
	for _, def := range definitions {
		if (status == "" || string(def.Status) == status) &&
			(query == "" || strings.Contains(strings.ToLower(def.Key), query) ||
				strings.Contains(strings.ToLower(def.DisplayName), query) ||
				strings.Contains(strings.ToLower(def.Description), query)) {
			filtered = append(filtered, def)
		}
	}
	start := 0
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || !strings.ContainsRune(string(decoded), '\x00') {
			writeStoreError(w, fmt.Errorf("%w: invalid metric cursor", platform.ErrValidation))
			return
		}
		found := false
		for i, def := range filtered {
			if metricCursor(def) == cursor {
				start, found = i+1, true
				break
			}
		}
		if !found {
			writeStoreError(w, fmt.Errorf("%w: metric cursor no longer exists", platform.ErrConflict))
			return
		}
	}
	end := min(start+limit, len(filtered))
	next := ""
	if end < len(filtered) {
		next = metricCursor(filtered[end-1])
	}
	writeJSON(w, http.StatusOK, struct {
		Items      []*platform.MetricDefinition `json:"items"`
		NextCursor string                       `json:"nextCursor,omitempty"`
	}{filtered[start:end], next})
}

func (s *Server) handleGetMetricDefinition(w http.ResponseWriter, r *http.Request) {
	ws, err := s.metricWorkspace(r, admin.PermReportRead)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	ref, err := metricPathRef(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	def, err := s.Store.GetMetricDefinition(ws.ID, ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, def)
}

func (s *Server) handleSubmitMetricDefinition(w http.ResponseWriter, r *http.Request) {
	ws, err := s.metricWorkspace(r, admin.PermExperimentWrite)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	ref, err := metricPathRef(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	def, err := s.Store.GetMetricDefinition(ws.ID, ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if def.Status != platform.MetricDraft {
		writeStoreError(w, fmt.Errorf("%w: only draft metrics can be submitted", platform.ErrInvalidState))
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeStoreError(w, fmt.Errorf("%w: review reason required", platform.ErrValidation))
		return
	}
	actor := actorFrom(r)
	approval, err := s.Store.RequestApproval(platform.Approval{
		WorkspaceID: ws.ID, Action: "metric.publish", Object: fmt.Sprintf("%s/v%d", ref.Key, ref.Version),
		PayloadHash: def.Hash, BaseRevision: ref.Version, Reason: req.Reason,
		Impact:         "new experiments may reference this metric definition",
		RollbackTarget: "deprecate this version; existing experiments keep their frozen definition",
		Guardrails:     "independent reviewer must approve the exact definition hash",
		SubmittedBy:    actor.Name, SubmittedTokenID: actor.TokenID,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	_ = s.Store.Audit(actor.Name, actor.Role, "metric.submit", ws.ID, "", approval.Object, approval.ID)
	writeJSON(w, http.StatusCreated, approval)
}

func (s *Server) handlePublishMetricDefinition(w http.ResponseWriter, r *http.Request) {
	ws, err := s.metricWorkspace(r, admin.PermFlagPublish)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	ref, err := metricPathRef(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		ApprovalID string `json:"approvalId"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	def, err := s.Store.GetMetricDefinition(ws.ID, ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if def.Status != platform.MetricDraft {
		writeStoreError(w, fmt.Errorf("%w: only draft metrics can be published", platform.ErrInvalidState))
		return
	}
	approval, err := s.Store.GetApproval(req.ApprovalID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.Admin.CheckApprover(approval.ApprovedTokenID, ws.ID); err != nil {
		writeStoreError(w, err)
		return
	}
	actor := actorFrom(r)
	published, err := s.Store.PublishMetricDefinitionWithApproval(ws.ID, ref, actor.Name, req.ApprovalID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	object := fmt.Sprintf("%s/v%d", ref.Key, ref.Version)
	_ = s.Store.Audit(actor.Name, actor.Role, "metric.publish", ws.ID, "", object, def.Hash)
	writeJSON(w, http.StatusOK, published)
}

func (s *Server) handleDeprecateMetricDefinition(w http.ResponseWriter, r *http.Request) {
	ws, err := s.metricWorkspace(r, admin.PermFlagPublish)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	ref, err := metricPathRef(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req, maxAdminBodyBytes); err != nil {
		writeStoreError(w, err)
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeStoreError(w, fmt.Errorf("%w: deprecation reason required", platform.ErrValidation))
		return
	}
	def, err := s.Store.DeprecateMetricDefinition(ws.ID, ref)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	actor := actorFrom(r)
	_ = s.Store.Audit(actor.Name, actor.Role, "metric.deprecate", ws.ID, "",
		fmt.Sprintf("%s/v%d", ref.Key, ref.Version), req.Reason)
	writeJSON(w, http.StatusOK, def)
}
