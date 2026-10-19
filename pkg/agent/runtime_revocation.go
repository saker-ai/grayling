package agent

import (
	"context"
	"slices"
	"strconv"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// ApplyRevocation downloads and authenticates an explicitly selected WSS Revoke
// event. The signed per-Bundle cancellation set controls old streams; frame
// metadata alone cannot authorize cancellation or erase a durable denial.
func (c *Client) ApplyRevocation(ctx context.Context, event *agentv1.Envelope, runtime *RuntimeManager) error {
	if c == nil || runtime == nil || event == nil || c.opts.Hello == nil || c.welcome == nil || c.welcome.GetWelcome() == nil {
		return ErrProtocol
	}
	event = proto.Clone(event).(*agentv1.Envelope)
	revoke := event.GetRevoke()
	if revoke == nil || revoke.View == nil || revoke.Epoch == 0 || event.OperationId != "revoke-"+strconv.FormatUint(revoke.Epoch, 10)+"-"+strconv.FormatUint(revoke.View.Revision, 10) || revoke.View.Kind != agentv1.ResourceKind_RESOURCE_KIND_REVOCATION || revoke.View.Key != RevocationKey || revoke.View.Revision == 0 || event.DeadlineUnixMs <= 0 {
		return ErrProtocol
	}
	ctx, done, err := runtime.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(min(event.DeadlineUnixMs, revoke.View.ExpiresUnixMs)))
	defer cancel()
	resource, err := c.Download(ctx, revoke.View)
	if err != nil {
		return err
	}
	now, err := runtime.guard.time()
	if err != nil {
		return err
	}
	d, claims, err := VerifyRevocation(runtime.guard.verifier.Load(), resource, runtime.opts.Scope, Watermark{}, now)
	if err != nil {
		return err
	}
	if claims.Revision != revoke.View.Revision || d.Epoch != revoke.Epoch || !slices.Equal(d.BundleHashes, revoke.BundleHashes) || revoke.CancelActiveStreams != (len(d.CancelledHashes) > 0) || revoke.View.ExpiresUnixMs > claims.ExpiresUnixMs {
		return ErrResource
	}
	return runtime.InstallRevocation(ctx, resource)
}
