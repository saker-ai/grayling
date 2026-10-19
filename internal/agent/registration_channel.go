package agent

import (
	"context"
	"errors"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// registrationCommand acknowledges governance persistence, never Naming
// readback or routing readiness. The durable projection is reconciled separately.
func (c *Channel) registrationCommand(ctx context.Context, se Session, e *agentv1.Envelope) (*agentv1.Envelope, bool) {
	switch e.Payload.(type) {
	case *agentv1.Envelope_Register, *agentv1.Envelope_RenewLease, *agentv1.Envelope_UpdateInstance, *agentv1.Envelope_Deregister:
	default:
		return nil, false
	}
	command, cancel := context.WithDeadline(ctx, commandDeadline(e, time.Now()))
	defer cancel()
	var body proto.Message
	var err error
	switch p := e.Payload.(type) {
	case *agentv1.Envelope_Register:
		body, err = c.sessions.RegisterInstance(command, se, e.OperationId, p.Register)
	case *agentv1.Envelope_RenewLease:
		body, err = c.sessions.RenewInstanceLease(command, se, e.OperationId, p.RenewLease)
	case *agentv1.Envelope_UpdateInstance:
		body, err = c.sessions.UpdateRegisteredInstance(command, se, e.OperationId, p.UpdateInstance)
	case *agentv1.Envelope_Deregister:
		body, err = c.sessions.DeregisterInstance(command, se, e.OperationId, p.Deregister)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		err = protocolError(agentv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED, "instance command deadline exceeded")
	}
	if err != nil {
		return response(se, e.MessageId, publicError(err)), true
	}
	return response(se, e.MessageId, body), true
}
