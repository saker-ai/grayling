package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/internal/admin"
	service "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
)

func decodeAgentAdmin(w http.ResponseWriter, r *http.Request, target any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
	if err != nil {
		return platform.ErrValidation
	}
	if err := json.Unmarshal(body, target, json.RejectUnknownMembers(true)); err != nil {
		return platform.ErrValidation
	}
	return nil
}
func writeAgentAdminError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, platform.ErrNotFound), errors.Is(err, platform.ErrAccessDenied), errors.Is(err, platform.ErrValidation), errors.Is(err, platform.ErrConflict):
		writeStoreError(w, err)
	default:
		if detail, ok := errors.AsType[*agentv1.ProtocolError](err); ok && detail.Code == agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			writeStoreError(w, platform.ErrValidation)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "agent_management_unavailable", "Agent management could not commit or read authoritative state")
	}
}
func (s *Server) agentAdminScope(w http.ResponseWriter, r *http.Request, permission admin.Permission) (client.Scope, bool) {
	w.Header().Set("Cache-Control", "no-store")
	if r.TLS == nil {
		writeError(w, http.StatusForbidden, "tls_required", "Agent management requires TLS on this listener")
		return client.Scope{}, false
	}
	workspace, environment, err := s.resolveScope(actorFrom(r), r.PathValue("ws"), r.PathValue("env"), permission)
	if err != nil {
		writeStoreError(w, err)
		return client.Scope{}, false
	}
	key := r.PathValue("service")
	if !model.KeyPattern.MatchString(key) {
		writeStoreError(w, platform.ErrValidation)
		return client.Scope{}, false
	}
	return client.Scope{WorkspaceID: workspace.ID, EnvironmentID: environment.ID, ServiceKey: key}, true
}
func agentActor(r *http.Request, operation string) service.AdminMutation {
	actor := actorFrom(r)
	return service.AdminMutation{ActorID: actor.TokenID, ActorRole: string(actor.Role), OperationID: operation}
}
func (s *Server) handleIssueAgentCredential(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.agentAdminScope(w, r, admin.PermAgentCredentialManage)
	if !ok {
		return
	}
	var request struct {
		OperationID        string    `json:"operationId"`
		PrincipalID        string    `json:"principalId"`
		AgentID            string    `json:"agentId"`
		InstanceID         string    `json:"instanceId"`
		BootID             string    `json:"bootId"`
		TakeoverFromBootID string    `json:"takeoverFromBootId"`
		Permissions        []string  `json:"permissions"`
		ExpiresAt          time.Time `json:"expiresAt"`
	}
	if err := decodeAgentAdmin(w, r, &request); err != nil {
		writeAgentAdminError(w, err)
		return
	}
	permissions, err := service.ParsePermissions(request.Permissions)
	if err != nil {
		writeAgentAdminError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.agentManagement.IssueManagedCredential(ctx, agentActor(r, request.OperationID), service.Grant{Scope: scope, PrincipalID: request.PrincipalID, AgentID: request.AgentID, InstanceID: request.InstanceID, BootID: request.BootID, TakeoverFromBootID: request.TakeoverFromBootID, Permissions: permissions, ExpiresAt: request.ExpiresAt})
	if err != nil {
		writeAgentAdminError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}
func (s *Server) handleRevokeAgentCredential(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.agentAdminScope(w, r, admin.PermAgentCredentialManage)
	if !ok {
		return
	}
	var request struct {
		OperationID string `json:"operationId"`
	}
	if err := decodeAgentAdmin(w, r, &request); err != nil {
		writeAgentAdminError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.agentManagement.RevokeManagedCredential(ctx, scope, agentActor(r, request.OperationID), r.PathValue("id")); err != nil {
		writeAgentAdminError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleListAgentCredentials(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.agentAdminScope(w, r, admin.PermAgentCredentialManage)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	items, err := s.agentManagement.ListCredentials(ctx, scope, r.URL.Query().Get("after"))
	if err != nil {
		writeAgentAdminError(w, err)
		return
	}
	cursor := ""
	if len(items) == 100 {
		cursor = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, struct {
		Items      []service.CredentialInfo `json:"items"`
		NextCursor string                   `json:"nextCursor,omitempty"`
	}{items, cursor})
}
func (s *Server) handleGetAgentLimit(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.agentAdminScope(w, r, admin.PermAgentQuotaManage)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.agentManagement.ScopeLimit(ctx, scope)
	if err != nil {
		writeAgentAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) handleSetAgentLimit(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.agentAdminScope(w, r, admin.PermAgentQuotaManage)
	if !ok {
		return
	}
	var request struct {
		OperationID            string `json:"operationId"`
		ExpectedMaxConnections *int   `json:"expectedMaxConnections"`
		MaxConnections         int    `json:"maxConnections"`
	}
	if err := decodeAgentAdmin(w, r, &request); err != nil {
		writeAgentAdminError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if request.ExpectedMaxConnections == nil {
		writeAgentAdminError(w, platform.ErrValidation)
		return
	}
	result, err := s.agentManagement.SetManagedScopeLimit(ctx, scope, agentActor(r, request.OperationID), *request.ExpectedMaxConnections, request.MaxConnections)
	if err != nil {
		writeAgentAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) handleAgentAudit(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.agentAdminScope(w, r, admin.PermAgentAuditRead)
	if !ok {
		return
	}
	var before uint64
	if value := r.URL.Query().Get("before"); value != "" {
		var err error
		before, err = strconv.ParseUint(value, 10, 64)
		if err != nil {
			writeAgentAdminError(w, platform.ErrValidation)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	items, err := s.agentManagement.ListAdminAudit(ctx, scope, before)
	if err != nil {
		writeAgentAdminError(w, err)
		return
	}
	cursor := ""
	if len(items) == 100 {
		cursor = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, struct {
		Items      []service.AdminAudit `json:"items"`
		NextCursor string               `json:"nextCursor,omitempty"`
	}{items, cursor})
}
