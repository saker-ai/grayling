package agent

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

var ErrDiscoveryStale = errors.New("agent: discovery freshness expired")

// DiscoveryCacheOptions binds one Bundle to a dedicated sealed cache. Minimum
// membership is an external floor paired with Minimum.ControlPlaneEpoch.
type DiscoveryCacheOptions struct {
	CacheOptions
	BundleHash                [32]byte
	MinimumMembershipRevision uint64
}
type DiscoveryCache struct{ cache *PolicyCache }
type DiscoveryView struct{ view *PolicyView }

func (v *DiscoveryView) Claims() Claims             { return v.view.claims }
func (v *DiscoveryView) MembershipRevision() uint64 { return v.view.discovery.MembershipRevision() }
func (v *DiscoveryView) Candidates() ([]*agentv1.Instance, error) {
	if err := v.view.check(); err != nil {
		return nil, err
	}
	return v.view.discovery.Candidates(), nil
}
func discoveryOptions(o DiscoveryCacheOptions) (CacheOptions, error) {
	base := o.CacheOptions
	if o.BundleHash == [32]byte{} || base.Minimum != (Watermark{}) && base.Minimum.BundleHash != o.BundleHash || base.Key != "" && base.Key != DiscoveryKey(o.BundleHash) || o.MinimumMembershipRevision > 0 && base.Minimum.ControlPlaneEpoch == 0 {
		return base, ErrCacheCorrupt
	}
	base.kind = agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY
	base.bundleHash = o.BundleHash
	base.minimumMembership = o.MinimumMembershipRevision
	base.Key = DiscoveryKey(o.BundleHash)
	if base.MaxAge == 0 {
		base.MaxAge = time.Minute
	}
	if base.MaxAge > time.Minute {
		return base, ErrCacheCorrupt
	}
	return base, nil
}
func InitializeDiscoveryCache(ctx context.Context, o DiscoveryCacheOptions) (*DiscoveryCache, error) {
	base, err := discoveryOptions(o)
	if err != nil {
		return nil, err
	}
	cache, err := openPolicyCache(ctx, base, true)
	if err != nil {
		return nil, err
	}
	return &DiscoveryCache{cache: cache}, nil
}
func OpenDiscoveryCache(ctx context.Context, o DiscoveryCacheOptions) (*DiscoveryCache, error) {
	base, err := discoveryOptions(o)
	if err != nil {
		return nil, err
	}
	cache, err := openPolicyCache(ctx, base, false)
	if err != nil {
		return nil, err
	}
	return &DiscoveryCache{cache: cache}, nil
}
func (c *DiscoveryCache) Capture() (*DiscoveryView, error) {
	view, err := c.cache.Capture()
	if err != nil {
		return nil, err
	}
	if view.discovery == nil {
		return nil, ErrCacheCorrupt
	}
	return &DiscoveryView{view: view}, nil
}
func (c *DiscoveryCache) Install(ctx context.Context, r *agentv1.SignedResource) error {
	return c.cache.Install(ctx, r)
}
func (c *DiscoveryCache) InstallRecovery(ctx context.Context, r *agentv1.SignedResource, g *RecoveryGrant) error {
	return c.cache.InstallRecovery(ctx, r, g)
}
func (c *DiscoveryCache) Watermark(ctx context.Context) (Watermark, error) {
	return c.cache.Watermark(ctx)
}
func (c *DiscoveryCache) SetVerifier(ctx context.Context, v *Verifier) error {
	return c.cache.SetVerifier(ctx, v)
}
func (c *DiscoveryCache) Revoke(ctx context.Context, epoch uint64, bundles [][32]byte) error {
	return c.cache.Revoke(ctx, epoch, bundles)
}
func (c *DiscoveryCache) Close() error { return c.cache.Close() }

func discoveryDocument(r *agentv1.SignedResource) (DiscoveryDocument, error) {
	var doc DiscoveryDocument
	if r == nil || len(r.Payload) > MaxResourceBytes {
		return doc, ErrCacheCorrupt
	}
	if err := json.Unmarshal(r.Payload, &doc, json.RejectUnknownMembers(true)); err != nil || !doc.valid() {
		return doc, ErrCacheCorrupt
	}
	return doc, nil
}
func (c *PolicyCache) checkDiscoveryRecord(r cacheRecord) error {
	if c.opts.kind != agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY || len(r.resource) == 0 {
		return nil
	}
	signed := new(agentv1.SignedResource)
	if err := proto.Unmarshal(r.resource, signed); err != nil {
		return ErrCacheCorrupt
	}
	doc, err := discoveryDocument(signed)
	if err != nil || doc.Scope != c.opts.Scope || r.mark.BundleHash != c.opts.bundleHash {
		return ErrCacheCorrupt
	}
	if r.mark.ControlPlaneEpoch == c.opts.Minimum.ControlPlaneEpoch && doc.MembershipRevision < c.opts.minimumMembership {
		return ErrRollback
	}
	return nil
}
func (c *PolicyCache) checkDiscoveryUpdate(old cacheRecord, r *agentv1.SignedResource, claims Claims) error {
	if c.opts.kind != agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY {
		return nil
	}
	doc, err := discoveryDocument(r)
	if err != nil {
		return err
	}
	if claims.ControlPlaneEpoch == c.opts.Minimum.ControlPlaneEpoch && doc.MembershipRevision < c.opts.minimumMembership {
		return ErrRollback
	}
	if len(old.resource) == 0 || old.mark.ControlPlaneEpoch != claims.ControlPlaneEpoch {
		return nil
	}
	prior := new(agentv1.SignedResource)
	if err := proto.Unmarshal(old.resource, prior); err != nil {
		return ErrCacheCorrupt
	}
	previous, err := discoveryDocument(prior)
	if err != nil {
		return err
	}
	if doc.MembershipRevision < previous.MembershipRevision {
		return ErrRollback
	}
	if doc.MembershipRevision == previous.MembershipRevision {
		a, err := MarshalDiscovery(doc)
		if err != nil {
			return err
		}
		b, err := MarshalDiscovery(previous)
		if err != nil {
			return err
		}
		if !bytes.Equal(a, b) {
			return ErrRollback
		}
	}
	return nil
}
