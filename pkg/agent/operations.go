package agent

import (
	"context"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// Metadata identifies the resource and stable logical operation, independently
// of the message IDs regenerated for each transport attempt.
type Metadata struct {
	OperationID    string
	SubscriptionID string
	ResourceKind   agentv1.ResourceKind
	ResourceKey    string
}

func envelope(body proto.Message, m Metadata) (*agentv1.Envelope, error) {
	e, err := Message(body)
	if err != nil {
		return nil, err
	}
	e.OperationId = m.OperationID
	e.SubscriptionId = m.SubscriptionID
	e.ResourceKind = m.ResourceKind
	e.ResourceKey = m.ResourceKey
	return e, nil
}

// SendMessage supports every generated client report, including Prepared,
// Applied, Rejected, readiness, drain, and resource acceptance/rejection.
func (c *Client) SendMessage(ctx context.Context, body proto.Message, m Metadata) error {
	e, err := envelope(body, m)
	if err != nil {
		return err
	}
	return c.Send(ctx, e)
}

// CallMessage supports every client request with a correlated response.
func (c *Client) CallMessage(ctx context.Context, body proto.Message, m Metadata) (*agentv1.Envelope, error) {
	e, err := envelope(body, m)
	if err != nil {
		return nil, err
	}
	return c.Call(ctx, e)
}

func (c *Client) Register(ctx context.Context, operationID string, r *agentv1.Register) (*agentv1.Registered, error) {
	e, err := c.CallMessage(ctx, r, Metadata{OperationID: operationID})
	if err != nil {
		return nil, err
	}
	if e.GetRegistered() == nil {
		return nil, ErrProtocol
	}
	return e.GetRegistered(), nil
}
func (c *Client) RenewLease(ctx context.Context, operationID string, r *agentv1.RenewLease) (*agentv1.LeaseRenewed, error) {
	e, err := c.CallMessage(ctx, r, Metadata{OperationID: operationID})
	if err != nil {
		return nil, err
	}
	if e.GetLeaseRenewed() == nil {
		return nil, ErrProtocol
	}
	return e.GetLeaseRenewed(), nil
}
func (c *Client) FetchResource(ctx context.Context, r *agentv1.FetchResource) (*agentv1.ResourceRef, error) {
	e, err := c.CallMessage(ctx, r, Metadata{})
	if err != nil {
		return nil, err
	}
	if e.GetResourceRef() == nil {
		return nil, ErrProtocol
	}
	return e.GetResourceRef(), nil
}
func (c *Client) ReleaseStatus(ctx context.Context, r *agentv1.ReleaseStatusQuery) (*agentv1.ReleaseStatus, error) {
	e, err := c.CallMessage(ctx, r, Metadata{})
	if err != nil {
		return nil, err
	}
	status := e.GetReleaseStatus()
	if !validReleaseStatus(r, status) {
		return nil, ErrProtocol
	}
	return status, nil
}
func (c *Client) Collect(ctx context.Context, r *agentv1.EventBatch) (*agentv1.EventBatchAck, error) {
	maxEvents := c.welcome.GetWelcome().Limits.MaxEventsPerBatch
	if r == nil || maxEvents == 0 || len(r.Events) == 0 || uint64(len(r.Events)) > uint64(maxEvents) {
		return nil, ErrProtocol
	}
	e, err := c.CallMessage(ctx, r, Metadata{})
	if err != nil {
		return nil, err
	}
	if e.GetEventBatchAck() == nil {
		return nil, ErrProtocol
	}
	return e.GetEventBatchAck(), nil
}

// Subscribe delivers snapshots and subsequent notifications through Events.
// Receipt alone never advances an applied cursor. Resume tokens and cursors
// should only be saved after the application accepts a verified resource.
func (c *Client) Subscribe(ctx context.Context, id string, cursor *agentv1.Cursor) error {
	if id == "" || cursor == nil || cursor.Key == "" || cursor.Kind < 1 || cursor.Kind > 4 {
		return ErrProtocol
	}
	return c.SendMessage(ctx, &agentv1.Subscribe{Scope: c.welcome.GetWelcome().Scope, Cursor: cursor}, Metadata{SubscriptionID: id, ResourceKind: cursor.Kind, ResourceKey: cursor.Key})
}
func (c *Client) Unsubscribe(ctx context.Context, id string) error {
	if id == "" {
		return ErrProtocol
	}
	return c.SendMessage(ctx, &agentv1.Unsubscribe{}, Metadata{SubscriptionID: id})
}
func (c *Client) Resume(ctx context.Context, id string, token []byte) error {
	if id == "" || len(token) == 0 {
		return ErrProtocol
	}
	return c.SendMessage(ctx, &agentv1.Resume{Token: token}, Metadata{SubscriptionID: id})
}

func (c *Client) UpdateInstance(ctx context.Context, operationID string, r *agentv1.UpdateInstance) (*agentv1.InstanceChanged, error) {
	e, err := c.CallMessage(ctx, r, Metadata{OperationID: operationID})
	if err != nil {
		return nil, err
	}
	if e.GetInstanceChanged() == nil {
		return nil, ErrProtocol
	}
	return e.GetInstanceChanged(), nil
}
func (c *Client) Deregister(ctx context.Context, operationID string, r *agentv1.Deregister) (*agentv1.InstanceChanged, error) {
	e, err := c.CallMessage(ctx, r, Metadata{OperationID: operationID})
	if err != nil {
		return nil, err
	}
	if e.GetInstanceChanged() == nil {
		return nil, ErrProtocol
	}
	return e.GetInstanceChanged(), nil
}

// ReportPrepared confirms durable evidence only, never route eligibility or ACTIVE.
// Retry the identical report with the Prepare operation ID after an unknown result.
func (c *Client) ReportPrepared(ctx context.Context, commandID string, report *agentv1.Prepared) (*agentv1.PreparedAck, error) {
	if report == nil || report.Release == nil || commandID == "" {
		return nil, ErrProtocol
	}
	e, err := c.CallMessage(ctx, report, Metadata{OperationID: commandID})
	if err != nil {
		return nil, err
	}
	ack := e.GetPreparedAck()
	if ack == nil || ack.Release == nil || !proto.Equal(ack.Release, report.Release) || ack.CommandId != commandID || e.OperationId != commandID || ack.EvidenceRevision == 0 || ack.ReceivedUnixMs <= 0 || ack.ExpiresUnixMs <= ack.ReceivedUnixMs || ack.ExpiresUnixMs > report.ExpiresUnixMs || ack.ExpiresUnixMs-ack.ReceivedUnixMs > 300000 {
		return nil, ErrProtocol
	}
	return ack, nil
}

// ReportPrepareRejected records a terminal negative outcome for one directed
// Prepare. A retry must keep command ID and report unchanged.
func (c *Client) ReportPrepareRejected(ctx context.Context, commandID string, report *agentv1.Rejected) (*agentv1.PrepareRejectedAck, error) {
	if report == nil || report.Release == nil || commandID == "" {
		return nil, ErrProtocol
	}
	e, err := c.CallMessage(ctx, report, Metadata{OperationID: commandID})
	if err != nil {
		return nil, err
	}
	ack := e.GetPrepareRejectedAck()
	if ack == nil || ack.Release == nil || !proto.Equal(ack.Release, report.Release) || ack.CommandId != commandID || e.OperationId != commandID || ack.EvidenceRevision == 0 || ack.ReceivedUnixMs <= 0 || ack.ExpiresUnixMs <= ack.ReceivedUnixMs {
		return nil, ErrProtocol
	}
	return ack, nil
}

// ReportApplied records the caller's actual installed view. Call only after the
// complete RuntimeView swap; a successful ACK never means business probes passed.
func (c *Client) ReportApplied(ctx context.Context, commandID string, report *agentv1.Applied) (*agentv1.ActivationReportAck, error) {
	if report == nil || report.Release == nil {
		return nil, ErrProtocol
	}
	return c.reportActivation(ctx, commandID, report, report.Release, false)
}
func (c *Client) ReportActivationRejected(ctx context.Context, commandID string, report *agentv1.Rejected) (*agentv1.ActivationReportAck, error) {
	if report == nil || report.Release == nil || report.ActivationArtifactRevision == 0 {
		return nil, ErrProtocol
	}
	return c.reportActivation(ctx, commandID, report, report.Release, true)
}

// ReplayLegacyActivationRejected retries an exact v10 NACK whose artifact field
// was absent. The server permits only a matching immutable receipt, never a new
// outcome. New installation failures must use ReportActivationRejected.
func (c *Client) ReplayLegacyActivationRejected(ctx context.Context, commandID string, report *agentv1.Rejected) (*agentv1.ActivationReportAck, error) {
	if report == nil || report.Release == nil || report.ActivationArtifactRevision != 0 {
		return nil, ErrProtocol
	}
	return c.reportActivation(ctx, commandID, report, report.Release, true)
}

func (c *Client) reportActivation(ctx context.Context, commandID string, report proto.Message, identity *agentv1.ReleaseIdentity, rejected bool) (*agentv1.ActivationReportAck, error) {
	if commandID == "" {
		return nil, ErrProtocol
	}
	e, err := c.CallMessage(ctx, report, Metadata{OperationID: commandID})
	if err != nil {
		return nil, err
	}
	ack := e.GetActivationReportAck()
	if ack == nil || !proto.Equal(ack.Release, identity) || ack.CommandId != commandID || e.OperationId != commandID || ack.ReportRevision == 0 || ack.ReceivedUnixMs <= 0 || ack.Rejected != rejected || ack.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING || ack.ReleaseRevision == 0 {
		return nil, ErrProtocol
	}
	return ack, nil
}

func validReleaseStatus(q *agentv1.ReleaseStatusQuery, r *agentv1.ReleaseStatus) bool {
	if q == nil || r == nil || r.Release == nil || r.Release.ReleaseId != q.ReleaseId || r.Release.Generation == 0 || !bundleDigest(r.Release.BundleHash) || r.ReleaseRevision == 0 || r.State < agentv1.ReleaseState_RELEASE_STATE_DRAFT || r.State > agentv1.ReleaseState_RELEASE_STATE_ROLLED_BACK || r.ActivationArtifactRevision == 0 || r.RequiredSources == 0 || r.RequiredSources > MaxDiscoveryMembers {
		return false
	}
	if q.ExpectedActivationArtifactRevision != nil && *q.ExpectedActivationArtifactRevision != r.ActivationArtifactRevision || q.ExpectedReportRevision != nil && *q.ExpectedReportRevision != r.ReportRevision || q.ExpectedReleaseRevision != nil && *q.ExpectedReleaseRevision != r.ReleaseRevision {
		return false
	}
	limit := q.PageSize
	if limit == 0 {
		limit = 64
	}
	count := len(r.Applied) + len(r.Rejected) + len(r.PendingSourceIds)
	if count > int(limit) || uint64(count) > r.RequiredSources || r.More && (r.NextInstanceId == "" || r.NextInstanceId <= q.AfterInstanceId) || !r.More && r.NextInstanceId != "" {
		return false
	}
	seen := map[string]bool{}
	accept := func(id string) bool {
		if !discoveryID(id) || id <= q.AfterInstanceId || seen[id] || r.More && id > r.NextInstanceId {
			return false
		}
		seen[id] = true
		return true
	}
	for _, report := range r.Applied {
		if report == nil || report.ActivationArtifactRevision != r.ActivationArtifactRevision || !proto.Equal(report.Release, r.Release) || !accept(report.InstanceId) {
			return false
		}
	}
	for _, report := range r.Rejected {
		if report == nil || report.ActivationArtifactRevision != r.ActivationArtifactRevision && !(report.ActivationArtifactRevision == 0 && r.ActivationArtifactRevision == 1) || !proto.Equal(report.Release, r.Release) || !accept(report.InstanceId) {
			return false
		}
	}
	for _, id := range r.PendingSourceIds {
		if !accept(id) {
			return false
		}
	}
	return true
}
