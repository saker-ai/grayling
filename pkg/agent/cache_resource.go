package agent

import (
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
)

type verifiedCacheResource struct {
	policy    *Policy
	discovery *Discovery
	bundle    *Bundle
	claims    Claims
}

func (c *PolicyCache) verifyResource(r *agentv1.SignedResource, mark Watermark, now time.Time) (verifiedCacheResource, error) {
	var verified verifiedCacheResource
	var err error
	switch c.opts.kind {
	case agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY:
		verified.discovery, verified.claims, err = VerifyDiscovery(c.opts.Verifier, r, c.opts.Scope, c.opts.bundleHash, mark, now)
	case agentv1.ResourceKind_RESOURCE_KIND_BUNDLE:
		verified.bundle, verified.claims, err = VerifyBundle(c.opts.Verifier, r, c.opts.Scope, c.opts.bundleHash, mark, now)
	default:
		verified.policy, verified.claims, err = VerifyPolicy(c.opts.Verifier, r, c.opts.Scope, c.opts.Key, mark, now)
	}
	return verified, err
}
