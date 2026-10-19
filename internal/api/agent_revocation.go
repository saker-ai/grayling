package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	client "github.com/saker-ai/grayling/pkg/agent"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/internal/admin"
	service "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/internal/platform"
	"google.golang.org/protobuf/proto"
)

func (s *Server) handleRevokeAgentBundles(w http.ResponseWriter, r *http.Request) {
	s.handleAgentRevocation(w, r, false)
}
func (s *Server) handleRefreshAgentRevocation(w http.ResponseWriter, r *http.Request) {
	s.handleAgentRevocation(w, r, true)
}
func (s *Server) handleAgentRevocation(w http.ResponseWriter, r *http.Request, refresh bool) {
	scope, ok := s.agentAdminScope(w, r, admin.PermAgentRevocationManage)
	if !ok {
		return
	}
	var request struct {
		OperationID              string   `json:"operationId"`
		ExpectedArtifactRevision uint64   `json:"expectedArtifactRevision,string"`
		ExpectedEpoch            *uint64  `json:"expectedEpoch,string"`
		BundleHashes             []string `json:"bundleHashes"`
		CancelActiveStreams      bool     `json:"cancelActiveStreams"`
		Reason                   string   `json:"reason"`
	}
	if err := decodeAgentAdmin(w, r, &request); err != nil {
		writeAgentAdminError(w, err)
		return
	}
	if request.ExpectedEpoch == nil || request.OperationID == "" || refresh && (len(request.BundleHashes) != 0 || request.CancelActiveStreams || request.Reason != "") {
		writeAgentAdminError(w, platform.ErrValidation)
		return
	}
	actor := actorFrom(r)
	q := service.RevokeBundlesRequest{ExpectedArtifactRevision: request.ExpectedArtifactRevision, ExpectedEpoch: *request.ExpectedEpoch, BundleHashes: request.BundleHashes, CancelActiveStreams: request.CancelActiveStreams, Reason: request.Reason, RefreshOnly: refresh}
	if refresh {
		q.Reason = "refresh revocation delivery proof"
	}
	q.TransactionGuard = func(ctx context.Context, tx pgx.Tx) error {
		if actor.TokenID == "configured-admin-password" {
			return nil
		}
		token, err := platform.LockPostgresTokenMetadata(ctx, tx, actor.TokenID)
		if err != nil {
			return err
		}
		return admin.Authorize(admin.Actor{TokenID: token.ID, Name: token.Name, Kind: token.Kind, Role: token.Role, WorkspaceID: token.WorkspaceID}, scope.WorkspaceID, admin.PermAgentRevocationManage)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resource, err := s.agentPublisher.RevokeBundles(ctx, scope, agentActor(r, request.OperationID), q, s.agentReleaseVerifier)
	if err != nil {
		if detail, ok := errors.AsType[*agentv1.ProtocolError](err); ok && detail.Code == agentv1.ErrorCode_ERROR_CODE_STALE_REVISION {
			writeError(w, http.StatusConflict, "revocation_revision_changed", "Revocation content or proof revision changed; reload current state before retrying")
			return
		}
		writeAgentAdminError(w, err)
		return
	}
	var signed agentv1.SignedResource
	if proto.Unmarshal(resource.Body, &signed) != nil {
		writeAgentAdminError(w, platform.ErrValidation)
		return
	}
	var document client.RevocationDocument
	if json.Unmarshal(signed.Payload, &document) != nil {
		writeAgentAdminError(w, platform.ErrValidation)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ArtifactRevision uint64    `json:"artifactRevision,string"`
		Epoch            uint64    `json:"epoch,string"`
		ExpiresAt        time.Time `json:"expiresAt"`
		OperationID      string    `json:"operationId"`
	}{ArtifactRevision: resource.Revision, Epoch: document.Epoch, ExpiresAt: resource.ExpiresAt, OperationID: request.OperationID})
}
