package agent

import (
	"context"
	"errors"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
)

// prepareUpdate uses durable current intent rather than connection-local state.
// A connection suppresses repeat delivery; reconnect recovers unacknowledged work.
func (c *Channel) prepareUpdate(ctx context.Context, se Session, host, delivered string) (*agentv1.Envelope, error) {
	pending, err := c.sessions.PendingPrepare(ctx, se, c.releaseVerifier)
	if releaseDispatchUnavailable(err) {
		return nil, nil
	}
	if err != nil || pending == nil {
		return nil, err
	}
	if pending.Intent.CommandID == delivered {
		return nil, nil
	}
	r := pending.Release
	body := &agentv1.Prepare{Release: &agentv1.ReleaseIdentity{ReleaseId: r.ReleaseID, Generation: r.Generation, BundleHash: r.Definition.BundleHash}, Bundle: resourceReference(host, se, agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, "bundle-"+r.Definition.BundleHash, pending.Bundle), ExpiresUnixMs: pending.Intent.ExpiresAt.UnixMilli(), TargetRevision: pending.Intent.TargetRevision}
	e := response(se, "", body)
	e.OperationId = pending.Intent.CommandID
	e.DeadlineUnixMs = body.ExpiresUnixMs
	return e, nil
}

func (c *Channel) preparedCommand(ctx context.Context, se Session, e *agentv1.Envelope) *agentv1.Envelope {
	if e.GetRejected() != nil {
		receipt, err := c.sessions.RecordPrepareRejected(ctx, se, e.OperationId, e.GetRejected(), c.releaseVerifier)
		if err != nil {
			return response(se, e.MessageId, publicError(err))
		}
		reply := response(se, e.MessageId, &agentv1.PrepareRejectedAck{Release: receipt.Report.Release, CommandId: receipt.CommandID, EvidenceRevision: receipt.Revision, ReceivedUnixMs: receipt.ReceivedAt.UnixMilli(), ExpiresUnixMs: receipt.ExpiresAt.UnixMilli()})
		reply.OperationId = receipt.CommandID
		return reply
	}

	receipt, err := c.sessions.RecordPrepared(ctx, se, e.OperationId, e.GetPrepared(), c.releaseVerifier)
	if err != nil {
		return response(se, e.MessageId, publicError(err))
	}
	ack := &agentv1.PreparedAck{Release: receipt.Report.Release, CommandId: receipt.CommandID, EvidenceRevision: receipt.Revision, ReceivedUnixMs: receipt.ReceivedAt.UnixMilli(), ExpiresUnixMs: receipt.ExpiresAt.UnixMilli()}
	reply := response(se, e.MessageId, ack)
	reply.OperationId = receipt.CommandID
	return reply
}

// A current release can lose eligibility while the authenticated control channel
// remains valid (notably after its Bundle is revoked). Suppress that positive
// command; closing the channel here can cancel the preceding Revoke delivery.
// Session fencing/authentication and backend failures still terminate polling.
func releaseDispatchUnavailable(err error) bool {
	detail, ok := errors.AsType[*agentv1.ProtocolError](err)
	if !ok {
		return false
	}
	switch detail.Code {
	case agentv1.ErrorCode_ERROR_CODE_NOT_READY, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, agentv1.ErrorCode_ERROR_CODE_CONFLICT, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION:
		return true
	}
	return false
}
