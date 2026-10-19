package agent

import (
	"context"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
)

func (c *Channel) activationUpdate(ctx context.Context, se Session, host, delivered string) (*agentv1.Envelope, error) {
	pending, err := c.sessions.PendingActivation(ctx, se, c.releaseVerifier)
	if releaseDispatchUnavailable(err) {
		return nil, nil
	}
	if err != nil || pending == nil {
		return nil, err
	}
	if pending.Source.CommandID == delivered {
		return nil, nil
	}
	r := pending.Release
	d := pending.Document
	body := &agentv1.Activate{Release: &agentv1.ReleaseIdentity{ReleaseId: r.ReleaseID, Generation: r.Generation, BundleHash: r.Definition.BundleHash}, PolicyRevision: d.PolicyRevision, PreparedRevision: d.PreparedRevision, View: resourceReference(host, se, agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION, client.ActivationKey(r.Generation), pending.Resource), DiscoveryRevision: d.DiscoveryRevision, RevocationEpoch: d.RevocationEpoch, ExpiresUnixMs: pending.Resource.ExpiresAt.UnixMilli()}
	e := response(se, "", body)
	e.OperationId = pending.Source.CommandID
	e.DeadlineUnixMs = body.ExpiresUnixMs
	return e, nil
}
func (c *Channel) activationCommand(ctx context.Context, se Session, e *agentv1.Envelope) *agentv1.Envelope {
	var ack *agentv1.ActivationReportAck
	var err error
	if report := e.GetApplied(); report != nil {
		ack, err = c.sessions.RecordApplied(ctx, se, e.OperationId, report, c.releaseVerifier)
	} else {
		ack, err = c.sessions.RecordActivationRejected(ctx, se, e.OperationId, e.GetRejected(), c.releaseVerifier)
	}
	if err != nil {
		return response(se, e.MessageId, publicError(err))
	}
	reply := response(se, e.MessageId, ack)
	reply.OperationId = e.OperationId
	return reply
}
