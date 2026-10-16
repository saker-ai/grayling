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
	if e.GetReleaseStatus() == nil {
		return nil, ErrProtocol
	}
	return e.GetReleaseStatus(), nil
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
