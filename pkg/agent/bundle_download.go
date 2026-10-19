package agent

import (
	"context"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// DownloadBundle binds an authenticated reference to the exact signed Bundle.
// It neither persists a watermark nor installs a RuntimeView or grants readiness.
// now must come from the caller's independently checked clock.
func (c *Client) DownloadBundle(ctx context.Context, ref *agentv1.ResourceRef, v *Verifier, scope Scope, hash [32]byte, mark Watermark, now time.Time) (*Bundle, Claims, error) {
	if ref == nil || ref.Kind != agentv1.ResourceKind_RESOURCE_KIND_BUNDLE || hash == [32]byte{} || ref.Key != BundleKey(hash) || ref.Revision == 0 {
		return nil, Claims{}, ErrResource
	}
	ref = proto.Clone(ref).(*agentv1.ResourceRef)
	resource, err := c.Download(ctx, ref)
	if err != nil {
		return nil, Claims{}, err
	}
	bundle, claims, err := VerifyBundle(v, resource, scope, hash, mark, now)
	if err != nil {
		return nil, Claims{}, err
	}
	if claims.Revision != ref.Revision || ref.ExpiresUnixMs > claims.ExpiresUnixMs {
		return nil, Claims{}, ErrResource
	}
	return bundle, claims, nil
}
