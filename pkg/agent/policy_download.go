package agent

import (
	"context"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// DownloadPolicy binds the authenticated reference's metadata to the signed
// policy claims and compiles the whole candidate. It does not persist or apply
// the result; callers must atomically save resource/high-watermark before use.
// now must come from the caller's checked clock, never from the resource itself.
func (c *Client) DownloadPolicy(ctx context.Context, ref *agentv1.ResourceRef, v *Verifier, scope Scope, mark Watermark, now time.Time) (*Policy, Claims, error) {
	if ref == nil || ref.Kind != agentv1.ResourceKind_RESOURCE_KIND_POLICY {
		return nil, Claims{}, ErrResource
	}
	ref = proto.Clone(ref).(*agentv1.ResourceRef)
	resource, err := c.Download(ctx, ref)
	if err != nil {
		return nil, Claims{}, err
	}
	policy, claims, err := VerifyPolicy(v, resource, scope, ref.Key, mark, now)
	if err != nil {
		return nil, Claims{}, err
	}
	if claims.Revision != ref.Revision || ref.ExpiresUnixMs > claims.ExpiresUnixMs {
		return nil, Claims{}, ErrResource
	}
	return policy, claims, nil
}
