package api

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/internal/admin"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
)

// Scope, epoch, policy revision and source digest are assembled by the server.
// The request contains only the reviewed mapping and expected source/CAS basis.
type bundleApprovalRequest struct {
	SourceRevision         int64                `json:"sourceRevision"`
	ExpectedPolicyRevision *uint64              `json:"expectedPolicyRevision,string"`
	PolicyKey              string               `json:"policyKey"`
	RouteFlag              string               `json:"routeFlag"`
	ValueBuilds            map[string]string    `json:"valueBuilds"`
	MinimumProtocolVersion uint32               `json:"minimumProtocolVersion"`
	DefaultBehavior        string               `json:"defaultBehavior"`
	DefaultBuild           string               `json:"defaultBuild"`
	ValidUntilUnixMs       int64                `json:"validUntilUnixMs,string"`
	Builds                 []client.BundleBuild `json:"builds"`
	Reason                 string               `json:"reason"`
}

func (s *Server) handleRequestBundleApproval(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.agentAdminScope(w, r, admin.PermFlagPublish)
	if !ok {
		return
	}
	var request bundleApprovalRequest
	if err := decodeAgentAdmin(w, r, &request); err != nil {
		writeBundlePublicationError(w, err)
		return
	}
	if request.ExpectedPolicyRevision == nil || *request.ExpectedPolicyRevision == math.MaxUint64 || request.SourceRevision <= 0 {
		writeBundlePublicationError(w, platform.ErrValidation)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	epoch, now, err := s.agentManagement.ServerState(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "bundle_approval_unavailable", "Could not read publication state; approval proposal was not created")
		return
	}
	if request.ValidUntilUnixMs <= now.UnixMilli() {
		writeBundlePublicationError(w, platform.ErrValidation)
		return
	}
	document := client.BundleDocument{SchemaVersion: 1, Purpose: "release-bundle", Scope: scope, Policy: client.BundlePolicy{Key: request.PolicyKey, ControlPlaneEpoch: epoch, Revision: *request.ExpectedPolicyRevision + 1}, RouteFlag: request.RouteFlag, ValueBuilds: request.ValueBuilds, MinimumProtocolVersion: request.MinimumProtocolVersion, DefaultBehavior: request.DefaultBehavior, DefaultBuild: request.DefaultBuild, ValidUntilUnixMs: request.ValidUntilUnixMs, Builds: request.Builds}
	approval, err := s.Admin.RequestBundlePublication(ctx, actorFrom(r), scope, request.SourceRevision, *request.ExpectedPolicyRevision, document, request.Reason)
	if err != nil {
		if approval != nil && errors.Is(err, admin.ErrBundleApprovalAuditUnconfirmed) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"approvalId": approval.ID, "error": map[string]string{"code": "bundle_approval_audit_unconfirmed", "message": "Approval exists; inspect this approval before submitting another proposal. Request audit is unconfirmed."}})
			return
		}
		// A storage failure may have committed an approval without confirming its ID.
		// Reconcile the existing approval list rather than blindly duplicating a request.
		if errors.Is(err, platform.ErrNotFound) || errors.Is(err, platform.ErrAccessDenied) || errors.Is(err, platform.ErrConflict) || errors.Is(err, platform.ErrValidation) || errors.Is(err, client.ErrResource) {
			writeBundlePublicationError(w, err)
		} else {
			writeError(w, http.StatusServiceUnavailable, "bundle_approval_unconfirmed", "Could not confirm approval creation; inspect existing approvals before resubmitting")
		}
		return
	}
	writeJSON(w, http.StatusCreated, approval)
}
func (s *Server) handlePublishApprovedBundle(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.agentAdminScope(w, r, admin.PermFlagPublish)
	if !ok {
		return
	}
	var request struct {
		ApprovalID string `json:"approvalId"`
	}
	if err := decodeAgentAdmin(w, r, &request); err != nil {
		writeBundlePublicationError(w, err)
		return
	}
	if request.ApprovalID == "" {
		writeBundlePublicationError(w, platform.ErrValidation)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.Admin.PublishApprovedBundle(ctx, actorFrom(r), scope, request.ApprovalID, s.agentPublisher)
	if err != nil {
		writeBundlePublicationError(w, err)
		return
	}
	// Stable metadata covers both the first confirmed result and exact replay.
	writeJSON(w, http.StatusOK, struct {
		BundleHash       string    `json:"bundleHash"`
		BundleKey        string    `json:"bundleKey"`
		ArtifactRevision uint64    `json:"artifactRevision,string"`
		PolicyRevision   uint64    `json:"policyRevision,string"`
		ExpiresAt        time.Time `json:"expiresAt"`
	}{BundleHash: hex.EncodeToString(result.BundleHash[:]), BundleKey: client.BundleKey(result.BundleHash), ArtifactRevision: result.Bundle.Revision, PolicyRevision: result.Policy.Revision, ExpiresAt: result.Bundle.ExpiresAt})
}
func writeBundlePublicationError(w http.ResponseWriter, err error) {
	if errors.Is(err, admin.ErrBundleOutcomeUnknown) {
		writeError(w, http.StatusServiceUnavailable, "bundle_publication_unavailable", "Bundle publication or audit is unconfirmed; retry the same approval")
		return
	}

	if detail, ok := errors.AsType[*agentv1.ProtocolError](err); ok {
		switch detail.Code {
		case agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, agentv1.ErrorCode_ERROR_CODE_CONFLICT, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED:
			writeStoreError(w, platform.ErrConflict)
		case agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
			writeStoreError(w, platform.ErrValidation)
		case agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
			writeStoreError(w, platform.ErrAccessDenied)
		default:
			writeError(w, http.StatusServiceUnavailable, "bundle_publication_unavailable", "Bundle publication could not confirm authoritative state; retry the same approval")
		}
		return
	}
	switch {
	case errors.Is(err, platform.ErrNotFound), errors.Is(err, platform.ErrAccessDenied), errors.Is(err, platform.ErrValidation), errors.Is(err, platform.ErrConflict), errors.Is(err, platform.ErrInvalidState):
		writeStoreError(w, err)
	case errors.Is(err, client.ErrResource):
		writeStoreError(w, platform.ErrValidation)
	default:
		writeError(w, http.StatusServiceUnavailable, "bundle_publication_unavailable", "Bundle publication could not confirm authoritative state; retry the same approval")
	}
}
