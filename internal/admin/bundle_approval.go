package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"math"
	"slices"

	"github.com/jackc/pgx/v5"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
)

const BundlePublishAction = "agent.bundle.publish"

// ErrBundleOutcomeUnknown prevents an audit persistence error from being
// mistaken for a definitive rejection of an already committed publication.
var ErrBundleOutcomeUnknown = errors.New("bundle publication outcome or audit unconfirmed")
var ErrBundleApprovalAuditUnconfirmed = errors.New("bundle approval created but request audit unconfirmed")

// BundlePublicationProposal contains the exact non-secret publication input
// reviewed by the approver. Policy bytes are read back by the server, not supplied
// by the applicant. SourceRevision is independent of the Agent policy revision.
type BundlePublicationProposal struct {
	SourceRevision         int64                 `json:"sourceRevision"`
	ExpectedPolicyRevision uint64                `json:"expectedPolicyRevision,string"`
	Document               client.BundleDocument `json:"document"`
	PolicyPayload          []byte                `json:"policyPayload"`
}

// BundlePublisher must preserve the durable operation's exact result on retry
// and enforce AuthorizationExpiresAt for every new effect transaction.
type BundlePublisher interface {
	PublishBundle(context.Context, control.PublishBundleRequest) (control.PublishedBundle, error)
}

// RequestBundlePublication binds approval to a verified platform publication and
// an exact immutable Bundle. It does not sign or activate anything.
func (s *Service) RequestBundlePublication(ctx context.Context, actor Actor, scope client.Scope, sourceRevision int64, expectedPolicyRevision uint64, document client.BundleDocument, reason string) (*platform.Approval, error) {
	if err := Authorize(actor, scope.WorkspaceID, PermFlagPublish); err != nil {
		return nil, err
	}
	if document.Scope != scope || actor.TokenID == "" || expectedPolicyRevision == math.MaxUint64 || document.Policy.Revision != expectedPolicyRevision+1 || sourceRevision <= 0 {
		return nil, platform.ErrValidation
	}
	env, err := s.Store.GetEnvironment(scope.WorkspaceID, scope.EnvironmentID)
	if err != nil {
		return nil, err
	}
	if env.ID != scope.EnvironmentID || env.WorkspaceID != scope.WorkspaceID {
		return nil, platform.ErrNotFound
	}
	release, err := s.acquirePublishLockContext(ctx, env)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := s.checkBundlePublisher(actor, scope.WorkspaceID); err != nil {
		return nil, err
	}
	source, err := s.loadPublishedSnapshotLocked(ctx, scope.WorkspaceID, scope.EnvironmentID, sourceRevision)
	if err != nil {
		return nil, err
	}
	if document.Policy.PayloadSHA256 != "" && document.Policy.PayloadSHA256 != hex.EncodeToString(source.SHA256[:]) {
		return nil, platform.ErrConflict
	}
	document.Policy.PayloadSHA256 = hex.EncodeToString(source.SHA256[:])
	if _, err := client.CompileBundlePolicy(document, source.Payload); err != nil {
		return nil, err
	}
	proposal := BundlePublicationProposal{SourceRevision: sourceRevision, ExpectedPolicyRevision: expectedPolicyRevision, Document: document, PolicyPayload: source.Payload}
	payload, err := json.Marshal(proposal, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	approval, err := s.Store.RequestApproval(platform.Approval{WorkspaceID: scope.WorkspaceID, EnvironmentID: scope.EnvironmentID, Action: BundlePublishAction, Object: scope.ServiceKey, PayloadHash: model.HashBytes(payload), BaseRevision: sourceRevision, Payload: string(payload), Reason: reason, Impact: "immutable signed Bundle and exact policy for service " + scope.ServiceKey, Guardrails: "published source digest, exact policy CAS, approval deadline; activation requires separate prepared evidence", RollbackTarget: "previous retained Bundle", SubmittedBy: actor.Name, SubmittedTokenID: actor.TokenID})
	if err != nil {
		return nil, err
	}
	if err := s.bundlePublicationAudit(actor, scope, approval.ID, "agent.bundle.requested", map[string]any{"payloadHash": approval.PayloadHash, "sourceRevision": sourceRevision}); err != nil {
		return approval, errors.Join(ErrBundleApprovalAuditUnconfirmed, err)
	}
	return approval, nil
}

// PublishApprovedBundle recovers a consumed approval using a stable effect
// namespace. Receipt and effect are separate transactions: a returned error may
// leave a consumed receipt, which can retry only this exact approved operation.
// Current caller and reviewer authorization are required even for result replay.
func (s *Service) PublishApprovedBundle(ctx context.Context, actor Actor, scope client.Scope, approvalID string, publisher BundlePublisher) (control.PublishedBundle, error) {
	if err := Authorize(actor, scope.WorkspaceID, PermFlagPublish); err != nil {
		return control.PublishedBundle{}, err
	}
	if publisher == nil || actor.TokenID == "" {
		return control.PublishedBundle{}, platform.ErrValidation
	}
	env, err := s.Store.GetEnvironment(scope.WorkspaceID, scope.EnvironmentID)
	if err != nil {
		return control.PublishedBundle{}, err
	}
	if env.ID != scope.EnvironmentID || env.WorkspaceID != scope.WorkspaceID {
		return control.PublishedBundle{}, platform.ErrNotFound
	}
	release, err := s.acquirePublishLockContext(ctx, env)
	if err != nil {
		return control.PublishedBundle{}, err
	}
	defer release()
	if err := s.checkBundlePublisher(actor, scope.WorkspaceID); err != nil {
		return control.PublishedBundle{}, err
	}
	approval, err := s.Store.GetApproval(approvalID)
	if err != nil {
		return control.PublishedBundle{}, err
	}
	if approval.WorkspaceID != scope.WorkspaceID || approval.EnvironmentID != scope.EnvironmentID || approval.Action != BundlePublishAction || approval.Object != scope.ServiceKey {
		return control.PublishedBundle{}, platform.ErrAccessDenied
	}
	if approval.ApprovedTokenID == "" || approval.ExpiresAt == nil {
		return control.PublishedBundle{}, platform.ErrInvalidState
	}
	if err := s.CheckApprover(approval.ApprovedTokenID, scope.WorkspaceID); err != nil {
		return control.PublishedBundle{}, err
	}
	var proposal BundlePublicationProposal
	if err := json.Unmarshal([]byte(approval.Payload), &proposal, json.RejectUnknownMembers(true)); err != nil {
		return control.PublishedBundle{}, platform.ErrValidation
	}
	canonical, err := json.Marshal(proposal, json.Deterministic(true))
	if err != nil {
		return control.PublishedBundle{}, err
	}
	if model.HashBytes(canonical) != approval.PayloadHash || proposal.SourceRevision != approval.BaseRevision || proposal.Document.Scope != scope || proposal.SourceRevision <= 0 || proposal.ExpectedPolicyRevision == math.MaxUint64 || proposal.Document.Policy.Revision != proposal.ExpectedPolicyRevision+1 {
		return control.PublishedBundle{}, platform.ErrConflict
	}
	digest := sha256.Sum256(proposal.PolicyPayload)
	if proposal.Document.Policy.PayloadSHA256 != hex.EncodeToString(digest[:]) {
		return control.PublishedBundle{}, platform.ErrConflict
	}
	if _, err := client.CompileBundlePolicy(proposal.Document, proposal.PolicyPayload); err != nil {
		return control.PublishedBundle{}, err
	}
	// First consumption must still refer to the current pinned publication. A
	// receipt retry retains the reviewed bytes, so later source changes cannot
	// prevent recovery of an already committed Agent operation.
	if approval.State != platform.ApprovalUsed {
		source, err := s.loadPublishedSnapshotLocked(ctx, scope.WorkspaceID, scope.EnvironmentID, proposal.SourceRevision)
		if err != nil {
			return control.PublishedBundle{}, err
		}
		if source.SHA256 != digest {
			return control.PublishedBundle{}, platform.ErrConflict
		}
	}
	if err := ctx.Err(); err != nil {
		return control.PublishedBundle{}, err
	}
	// A fixed-size digest avoids depending on approval ID length or punctuation.
	principalHash := sha256.Sum256([]byte(approval.ID))
	principal := "approval-" + hex.EncodeToString(principalHash[:])
	receipt, err := s.Store.UseApprovalForOperation(platform.ApprovalOperation{ApprovalID: approval.ID, WorkspaceID: scope.WorkspaceID, EnvironmentID: scope.EnvironmentID, Action: BundlePublishAction, Object: scope.ServiceKey, PayloadHash: approval.PayloadHash, BaseRevision: approval.BaseRevision, PrincipalID: principal, OperationID: "publish"})
	if err != nil {
		return control.PublishedBundle{}, err
	}
	if err := s.bundlePublicationAudit(actor, scope, approval.ID, "agent.bundle.consumed", map[string]any{"principalId": principal, "operationId": "publish", "receiptReplayed": receipt.Replayed, "payloadHash": approval.PayloadHash}); err != nil {
		return control.PublishedBundle{}, errors.Join(ErrBundleOutcomeUnknown, err)
	}
	result, effectErr := publisher.PublishBundle(ctx, control.PublishBundleRequest{PrincipalID: principal, OperationID: "publish", ExpectedPolicyRevision: proposal.ExpectedPolicyRevision, AuthorizationExpiresAt: *receipt.Approval.ExpiresAt, AuthorizeNewPublication: func(ctx context.Context, tx pgx.Tx) error {
		return lockBundlePublicationCredentials(ctx, tx, actor, approval.ApprovedTokenID, scope.WorkspaceID)
	}, Document: proposal.Document, PolicyPayload: proposal.PolicyPayload})
	action := "agent.bundle.published"
	detail := map[string]any{"principalId": principal, "operationId": "publish", "payloadHash": approval.PayloadHash}
	if effectErr != nil {
		action = "agent.bundle.outcome_unknown"
	} else {
		detail["bundleHash"] = hex.EncodeToString(result.BundleHash[:])
		detail["artifactRevision"] = result.Bundle.Revision
		detail["policyRevision"] = result.Policy.Revision
	}
	auditErr := s.bundlePublicationAudit(actor, scope, approval.ID, action, detail)
	if auditErr != nil {
		return result, errors.Join(ErrBundleOutcomeUnknown, effectErr, auditErr)
	}
	return result, effectErr
}

func (s *Service) bundlePublicationAudit(actor Actor, scope client.Scope, approvalID, action string, detail map[string]any) error {
	body, err := json.Marshal(detail, json.Deterministic(true))
	if err != nil {
		return err
	}
	return s.Store.Audit(actor.Name+" ("+actor.TokenID+")", actor.Role, action, scope.WorkspaceID, scope.EnvironmentID, approvalID, string(body))
}

// Check the persisted credential after scope reload, rather than trusting the
// Actor captured before waiting for the publish lock.
func (s *Service) checkBundlePublisher(actor Actor, workspaceID string) error {
	if actor.TokenID == "configured-admin-password" {
		return Authorize(actor, workspaceID, PermFlagPublish)
	}
	for _, token := range s.Store.ListTokens("") {
		if token.ID == actor.TokenID && !token.Revoked {
			return Authorize(Actor{TokenID: token.ID, Name: token.Name, Kind: token.Kind, Role: token.Role, WorkspaceID: token.WorkspaceID}, workspaceID, PermFlagPublish)
		}
	}
	return platform.ErrAccessDenied
}

// Row locks serialize a new mint against concurrent token revocation/downgrade.
// Historical result lookup grants no new authorization and skips this guard.
func lockBundlePublicationCredentials(ctx context.Context, tx pgx.Tx, caller Actor, approverID, workspaceID string) error {
	ids := []string{caller.TokenID, approverID}
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		if id == "configured-admin-password" {
			continue
		} // rechecked by HTTP authentication
		token, err := platform.LockPostgresTokenMetadata(ctx, tx, id)
		if err != nil {
			return err
		}
		actor := Actor{TokenID: token.ID, Name: token.Name, Kind: token.Kind, Role: token.Role, WorkspaceID: token.WorkspaceID}
		if id == caller.TokenID {
			if err := Authorize(actor, workspaceID, PermFlagPublish); err != nil {
				return err
			}
		}
		if id == approverID {
			if err := Authorize(actor, workspaceID, PermFlagApprove); err != nil {
				return err
			}
		}
	}
	return nil
}
