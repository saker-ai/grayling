package api

import (
	"context"
	"net/http"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/internal/admin"
	service "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/internal/platform"
)

func (s *Server) handleGetAgentRegistrationPolicy(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.agentAdminScope(w, r, admin.PermAgentCredentialManage)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.agentManagement.GetRegistrationPolicy(ctx, scope)
	if err != nil {
		writeAgentAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) handleSetAgentRegistrationPolicy(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.agentAdminScope(w, r, admin.PermAgentCredentialManage)
	if !ok {
		return
	}
	var request struct {
		OperationID      string                     `json:"operationId"`
		ExpectedRevision *uint64                    `json:"expectedRevision,string"`
		Policy           service.RegistrationPolicy `json:"policy"`
	}
	if err := decodeAgentAdmin(w, r, &request); err != nil {
		writeAgentAdminError(w, err)
		return
	}
	if request.ExpectedRevision == nil {
		writeAgentAdminError(w, platform.ErrValidation)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.agentManagement.SetRegistrationPolicy(ctx, scope, agentActor(r, request.OperationID), *request.ExpectedRevision, request.Policy)
	if err != nil {
		writeAgentAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) handleAuthorizeAgentDeployment(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.agentAdminScope(w, r, admin.PermAgentCredentialManage)
	if !ok {
		return
	}
	// Scope, lease and readiness are deliberately absent: management supplies
	// immutable deployment identity; the resolved path supplies the scope.
	var request struct {
		OperationID    string    `json:"operationId"`
		PrincipalID    string    `json:"principalId"`
		AgentID        string    `json:"agentId"`
		PolicyRevision *uint64   `json:"policyRevision,string"`
		ExpiresAt      time.Time `json:"expiresAt"`
		Instance       struct {
			InstanceID      string `json:"instanceId"`
			BootID          string `json:"bootId"`
			BuildID         string `json:"buildId"`
			ImageDigest     string `json:"imageDigest"`
			Address         string `json:"address"`
			Port            uint32 `json:"port"`
			TLSServerName   string `json:"tlsServerName"`
			Zone            string `json:"zone"`
			ProtocolVersion uint32 `json:"protocolVersion"`
		} `json:"instance"`
	}
	if err := decodeAgentAdmin(w, r, &request); err != nil {
		writeAgentAdminError(w, err)
		return
	}
	if request.PolicyRevision == nil {
		writeAgentAdminError(w, platform.ErrValidation)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	i := request.Instance
	instance := &agentv1.Instance{Scope: &agentv1.Scope{WorkspaceId: scope.WorkspaceID, EnvironmentId: scope.EnvironmentID, ServiceKey: scope.ServiceKey}, InstanceId: i.InstanceID, BootId: i.BootID, BuildId: i.BuildID, ImageDigest: i.ImageDigest, Address: i.Address, Port: i.Port, TlsServerName: i.TLSServerName, Zone: i.Zone, ProtocolVersion: i.ProtocolVersion}
	result, err := s.agentManagement.AuthorizeDeployment(ctx, agentActor(r, request.OperationID), scope, request.PrincipalID, request.AgentID, *request.PolicyRevision, instance, request.ExpiresAt)
	if err != nil {
		writeAgentAdminError(w, err)
		return
	}
	// The opaque authorization reference is replayable metadata, not a secret.
	writeJSON(w, http.StatusOK, result)
}
