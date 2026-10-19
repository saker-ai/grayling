package agent

import (
	"context"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// DownloadActivation authenticates an exact signed view and this client's source
// identity. Durable generation floors and actual RuntimeView installation remain
// the caller's responsibility; downloading must never trigger an Applied report.
func (c *Client) DownloadActivation(ctx context.Context, ref *agentv1.ResourceRef, v *Verifier, scope Scope, generation uint64, bundle [32]byte, mark Watermark, now time.Time) (*ActivationView, Claims, error) {
	if ref == nil || ref.Kind != agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION || generation == 0 || ref.Key != ActivationKey(generation) || ref.Revision == 0 {
		return nil, Claims{}, ErrResource
	}
	ref = proto.Clone(ref).(*agentv1.ResourceRef)
	signed, err := c.Download(ctx, ref)
	if err != nil {
		return nil, Claims{}, err
	}
	view, claims, err := VerifyActivation(v, signed, scope, generation, bundle, mark, now)
	if err != nil {
		return nil, Claims{}, err
	}
	if claims.Revision != ref.Revision || ref.ExpiresUnixMs > claims.ExpiresUnixMs {
		return nil, Claims{}, ErrResource
	}
	for _, source := range view.document.Sources {
		if source.InstanceID == c.opts.Hello.InstanceId && source.BootID == c.opts.Hello.BootId {
			return view, claims, nil
		}
	}
	return nil, Claims{}, ErrScope
}
