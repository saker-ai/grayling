package agent

import (
	"context"
	"encoding/hex"
	"strconv"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

type PreparationResult struct {
	CommandID string
	Workload  *PreparedWorkload
	Prepared  *agentv1.Prepared
	Receipt   *agentv1.PreparedAck
}

// PrepareRelease verifies exact resources, constructs an owned local candidate,
// checks health, and only then reports Prepared. It never activates a view or
// automatically sends NACK: a lost receipt may follow a successful report.
func (c *Client) PrepareRelease(ctx context.Context, event *agentv1.Envelope, o PreparationOptions) (PreparationResult, error) {
	var result PreparationResult
	var err error
	event, err = preparationEvent(c, event, o)
	if err != nil {
		return result, err
	}
	prepare := event.GetPrepare()
	result.CommandID = event.OperationId
	deadline := min(event.DeadlineUnixMs, prepare.ExpiresUnixMs, prepare.Bundle.ExpiresUnixMs)
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(deadline))
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	signed, err := c.Download(ctx, prepare.Bundle)
	if err != nil {
		return result, err
	}
	raw, err := hex.DecodeString(prepare.Release.BundleHash)
	if err != nil {
		return result, err
	}
	var hash [32]byte
	copy(hash[:], raw)
	bundle, claims, err := VerifyBundle(o.Verifier, signed, o.Scope, hash, Watermark{}, o.Now())
	if err != nil {
		return result, err
	}
	if claims.Revision != prepare.Bundle.Revision || claims.ControlPlaneEpoch != c.welcome.GetWelcome().ControlPlaneEpoch {
		return result, ErrResource
	}
	if bundle.document.Policy.Key != o.PolicyKey {
		return result, ErrScope
	}
	policy, err := c.downloadExact(ctx, agentv1.ResourceKind_RESOURCE_KIND_POLICY, bundle.document.Policy.Key, bundle.document.Policy.Revision)
	if err != nil {
		return result, err
	}
	result.Workload, err = newPreparedWorkload(ctx, event.OperationId, prepare.Release, signed, policy, o)
	if err != nil {
		return result, err
	}
	result.Prepared, err = result.Workload.PreparedReport(ctx)
	if err != nil {
		result.Workload.retire()
		return result, err
	}
	result.Receipt, err = c.ReportPrepared(ctx, event.OperationId, result.Prepared)
	return result, err
}

func preparationOptionsValid(o PreparationOptions) bool {
	return o.Verifier != nil && o.Now != nil && o.Builder != nil && discoveryID(o.Scope.WorkspaceID) && discoveryID(o.Scope.EnvironmentID) && discoveryID(o.Scope.ServiceKey) && discoveryID(o.InstanceID) && discoveryID(o.BootID) && discoveryID(o.BuildID) && validText(o.PolicyKey) && len(o.ImageDigest) == 71 && o.ImageDigest[:7] == "sha256:" && bundleDigest(o.ImageDigest[7:])
}
func preparationEvent(c *Client, event *agentv1.Envelope, o PreparationOptions) (*agentv1.Envelope, error) {
	if c == nil || event == nil || c.opts.Hello == nil || c.welcome == nil || c.welcome.GetWelcome() == nil || !preparationOptionsValid(o) {
		return nil, ErrProtocol
	}
	event = proto.Clone(event).(*agentv1.Envelope)
	prepare := event.GetPrepare()
	if prepare == nil || prepare.Release == nil || prepare.Release.Generation == 0 || prepare.Release.ReleaseId != "release-"+strconv.FormatUint(prepare.Release.Generation, 10) || !bundleDigest(prepare.Release.BundleHash) || !discoveryID(event.OperationId) || prepare.Bundle == nil || prepare.Bundle.Kind != agentv1.ResourceKind_RESOURCE_KIND_BUNDLE || prepare.Bundle.Key != "bundle-"+prepare.Release.BundleHash || prepare.Bundle.Revision == 0 || event.DeadlineUnixMs <= 0 || prepare.ExpiresUnixMs <= 0 {
		return nil, ErrProtocol
	}
	if c.opts.Hello.InstanceId != o.InstanceID || c.opts.Hello.BootId != o.BootID {
		return nil, ErrScope
	}

	welcome := c.welcome.GetWelcome()
	scope := welcome.Scope
	if scope == nil || welcome.ControlPlaneEpoch == 0 || scope.WorkspaceId != o.Scope.WorkspaceID || scope.EnvironmentId != o.Scope.EnvironmentID || scope.ServiceKey != o.Scope.ServiceKey || !proto.Equal(prepare.Bundle.Scope, scope) {
		return nil, ErrScope
	}
	digest, err := hex.DecodeString(prepare.Bundle.Sha256)
	if err != nil || len(digest) != 32 {
		return nil, ErrResource
	}
	return event, nil
}
