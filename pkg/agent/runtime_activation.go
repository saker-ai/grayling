package agent

import (
	"context"
	"encoding/hex"
	"strconv"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// RuntimeActivationResult distinguishes an actual process installation from its
// remote receipt. Applied remains available when the receipt is lost; retry that
// exact report while authority is valid, or query ReleaseStatus after expiry.
type RuntimeActivationResult struct {
	CommandID string
	Applied   *agentv1.Applied
	Receipt   *agentv1.ActivationReportAck
}

// InstallActivation handles an authenticated Activate event explicitly selected
// by the caller. It downloads only exact signed dependencies, installs complete
// process resources, then reports actual installation. It never sends a negative
// outcome automatically: an error may follow a successful durable installation.
func (c *Client) InstallActivation(ctx context.Context, event *agentv1.Envelope, runtime *RuntimeManager) (RuntimeActivationResult, error) {
	var result RuntimeActivationResult
	if c == nil || runtime == nil || event == nil || c.opts.Hello == nil || c.welcome == nil || c.welcome.GetWelcome() == nil {
		return result, ErrProtocol
	}
	event = proto.Clone(event).(*agentv1.Envelope)
	activate := event.GetActivate()
	if activate == nil || activate.Release == nil || activate.Release.Generation == 0 || !bundleDigest(activate.Release.BundleHash) || activate.Release.ReleaseId != "release-"+strconv.FormatUint(activate.Release.Generation, 10) || !discoveryID(event.OperationId) || activate.View == nil || activate.View.Kind != agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION || activate.View.Key != ActivationKey(activate.Release.Generation) || activate.View.Revision == 0 || activate.ExpiresUnixMs <= 0 || event.DeadlineUnixMs <= 0 || activate.PolicyRevision == 0 || activate.DiscoveryRevision == 0 || activate.PreparedRevision == 0 {
		return result, ErrProtocol
	}
	result.CommandID = event.OperationId
	if c.opts.Hello.InstanceId != runtime.opts.InstanceID || c.opts.Hello.BootId != runtime.opts.BootID {
		return result, ErrScope
	}
	ctx, done, err := runtime.begin(ctx)
	if err != nil {
		return result, err
	}
	defer done()
	deadline := min(event.DeadlineUnixMs, activate.ExpiresUnixMs, activate.View.ExpiresUnixMs)
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(deadline))
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	signed, err := c.Download(ctx, activate.View)
	if err != nil {
		return result, err
	}
	now, err := runtime.guard.time()
	if err != nil {
		return result, err
	}
	rawHash, _ := hex.DecodeString(activate.Release.BundleHash)
	var hash [32]byte
	copy(hash[:], rawHash)
	view, claims, err := VerifyActivation(runtime.guard.verifier.Load(), signed, runtime.opts.Scope, activate.Release.Generation, hash, Watermark{}, now)
	if err != nil {
		return result, err
	}
	d := view.document
	if claims.Revision != activate.View.Revision || activate.View.ExpiresUnixMs > claims.ExpiresUnixMs || activate.ExpiresUnixMs > claims.ExpiresUnixMs || d.ReleaseID != activate.Release.ReleaseId || d.PolicyRevision != activate.PolicyRevision || d.DiscoveryRevision != activate.DiscoveryRevision || d.PreparedRevision != activate.PreparedRevision || d.RevocationEpoch != activate.RevocationEpoch {
		return result, ErrResource
	}
	source := false
	for _, s := range d.Sources {
		if s.InstanceID == runtime.opts.InstanceID && s.BootID == runtime.opts.BootID && s.CommandID == event.OperationId {
			source = true
			break
		}
	}
	if !source {
		return result, ErrScope
	}
	if d.RevocationEpoch > 0 {
		floor, err := runtime.RevocationFloor(ctx)
		if err != nil {
			return result, err
		}
		if floor.Epoch < d.RevocationEpoch {
			ref, err := c.FetchResource(ctx, &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_REVOCATION, Key: RevocationKey})
			if err != nil {
				return result, err
			}
			if ref.Kind != agentv1.ResourceKind_RESOURCE_KIND_REVOCATION || ref.Key != RevocationKey {
				return result, ErrResource
			}
			proof, err := c.Download(ctx, ref)
			if err != nil {
				return result, err
			}
			now, err := runtime.guard.time()
			if err != nil {
				return result, err
			}
			proofDoc, proofClaims, err := VerifyRevocation(runtime.guard.verifier.Load(), proof, runtime.opts.Scope, Watermark{}, now)
			if err != nil {
				return result, err
			}
			if proofClaims.Revision != ref.Revision || proofDoc.Epoch != d.RevocationEpoch {
				return result, ErrRollback
			}
			if err := runtime.InstallRevocation(ctx, proof); err != nil {
				return result, err
			}
		}
	}
	bundleResource, err := c.downloadExact(ctx, agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, BundleKey(hash), d.BundleArtifactRevision)
	if err != nil {
		return result, err
	}
	now, err = runtime.guard.time()
	if err != nil {
		return result, err
	}
	bundle, _, err := view.VerifyBundle(runtime.guard.verifier.Load(), bundleResource, Watermark{}, now)
	if err != nil {
		return result, err
	}
	if bundle.document.Policy.Key != runtime.opts.PolicyKey {
		return result, ErrScope
	}
	policy, err := c.downloadExact(ctx, agentv1.ResourceKind_RESOURCE_KIND_POLICY, bundle.document.Policy.Key, d.PolicyRevision)
	if err != nil {
		return result, err
	}
	result.Applied, err = runtime.Install(ctx, event.OperationId, RuntimeInput{Activation: signed, Bundle: bundleResource, Policy: policy})
	if err != nil {
		return result, err
	}
	current, err := runtime.Applied()
	if err != nil {
		return result, err
	}
	if !proto.Equal(current, result.Applied) {
		return result, ErrRollback
	}
	result.Receipt, err = c.ReportApplied(ctx, event.OperationId, result.Applied)
	return result, err
}
func (c *Client) downloadExact(ctx context.Context, kind agentv1.ResourceKind, key string, revision uint64) (*agentv1.SignedResource, error) {
	ref, err := c.FetchResource(ctx, &agentv1.FetchResource{Kind: kind, Key: key, Revision: revision})
	if err != nil {
		return nil, err
	}
	if ref.Kind != kind || ref.Key != key || ref.Revision != revision {
		return nil, ErrResource
	}
	return c.Download(ctx, ref)
}
