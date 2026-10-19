package agent

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
)

// QueuePrepareRequest is trusted control-plane intent, not an Agent operation.
// ExpectedTargetRevision fences replacement of an earlier command for a target.
type QueuePrepareRequest struct {
	Scope                              client.Scope
	PrincipalID, OperationID           string
	Generation, ExpectedTargetRevision uint64
	InstanceID                         string
	AuthorizationExpiresAt             time.Time
	Authorize                          func(context.Context, pgx.Tx) error
}

// PrepareIntent is immutable historical intent. Dispatch must still validate
// current release, target head, session, lease, registration and expiry.
type PrepareIntent struct {
	Scope                                    client.Scope
	Generation, TargetRevision               uint64
	CommandID                                string
	Instance                                 *agentv1.Instance
	CredentialID, TargetPrincipalID, AgentID string
	LeaseID                                  string
	LeaseEpoch                               uint64
	CreatedAt, ExpiresAt                     time.Time
}

// PreparedReceipt confirms persistence, never routing eligibility or activation.
type PreparedReceipt struct {
	CommandID             string
	Generation, Revision  uint64
	ReceivedAt, ExpiresAt time.Time
	Report                *agentv1.Prepared
}
