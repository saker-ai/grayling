package agent

import (
	"context"
	"encoding/hex"
	"errors"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
)

var ErrBundleStale = errors.New("agent: bundle freshness expired")

// BundleCacheOptions binds a cache to one exact content-addressed Bundle.
// A new Bundle uses a distinct deployment-owned directory; policy generations
// and control-plane recovery authority remain independent of this cache.
type BundleCacheOptions struct {
	CacheOptions
	BundleHash [32]byte
}
type BundleCache struct{ cache *PolicyCache }
type BundleView struct{ view *PolicyView }

func bundleOptions(o BundleCacheOptions) (CacheOptions, error) {
	base := o.CacheOptions
	if o.BundleHash == [32]byte{} || base.Key != "" && base.Key != BundleKey(o.BundleHash) || base.Minimum != (Watermark{}) && base.Minimum.BundleHash != o.BundleHash || !discoveryID(base.Scope.WorkspaceID) || !discoveryID(base.Scope.EnvironmentID) || !discoveryID(base.Scope.ServiceKey) {
		return base, ErrCacheCorrupt
	}
	base.kind = agentv1.ResourceKind_RESOURCE_KIND_BUNDLE
	base.bundleHash = o.BundleHash
	base.Key = BundleKey(o.BundleHash)
	return base, nil
}
func InitializeBundleCache(ctx context.Context, o BundleCacheOptions) (*BundleCache, error) {
	base, err := bundleOptions(o)
	if err != nil {
		return nil, err
	}
	cache, err := openPolicyCache(ctx, base, true)
	if err != nil {
		return nil, err
	}
	return &BundleCache{cache: cache}, nil
}
func OpenBundleCache(ctx context.Context, o BundleCacheOptions) (*BundleCache, error) {
	base, err := bundleOptions(o)
	if err != nil {
		return nil, err
	}
	cache, err := openPolicyCache(ctx, base, false)
	if err != nil {
		return nil, err
	}
	return &BundleCache{cache: cache}, nil
}
func (c *BundleCache) Install(ctx context.Context, r *agentv1.SignedResource) error {
	return c.cache.Install(ctx, r)
}
func (c *BundleCache) Capture() (*BundleView, error) {
	view, err := c.cache.Capture()
	if err != nil {
		return nil, err
	}
	if view.bundle == nil {
		return nil, ErrCacheCorrupt
	}
	return &BundleView{view: view}, nil
}
func (c *BundleCache) Watermark(ctx context.Context) (Watermark, error) {
	return c.cache.Watermark(ctx)
}
func (c *BundleCache) SetVerifier(ctx context.Context, v *Verifier) error {
	return c.cache.SetVerifier(ctx, v)
}
func (c *BundleCache) Revoke(ctx context.Context, epoch uint64, bundles [][32]byte) error {
	return c.cache.Revoke(ctx, epoch, bundles)
}
func (c *BundleCache) Close() error  { return c.cache.Close() }
func (v *BundleView) Claims() Claims { return v.view.claims }
func (v *BundleView) Document() (BundleDocument, error) {
	if err := v.view.check(); err != nil {
		return BundleDocument{}, err
	}
	return v.view.bundle.Document(), nil
}

// ValidatePolicy joins two independently durable views without refreshing either
// resource. Runtime construction must still join discovery, Prepared and release
// evidence and keep these live guards on every captured request view.
func (v *BundleView) ValidatePolicy(policy *PolicyView) error {
	if err := v.view.check(); err != nil {
		return err
	}
	if policy == nil || policy.policy == nil {
		return ErrCacheNotReady
	}
	if err := policy.check(); err != nil {
		return err
	}
	d := v.view.bundle.document
	c := policy.claims
	if c.Scope != d.Scope || c.Kind != agentv1.ResourceKind_RESOURCE_KIND_POLICY || c.Key != d.Policy.Key || c.ControlPlaneEpoch != d.Policy.ControlPlaneEpoch || c.Revision != d.Policy.Revision || c.BundleHash != v.view.claims.BundleHash || hex.EncodeToString(c.PayloadHash[:]) != d.Policy.PayloadSHA256 {
		return ErrResource
	}
	return checkBundleRoutes(d, policy.policy)
}
