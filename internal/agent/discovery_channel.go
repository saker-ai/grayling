package agent

import (
	"context"
	"encoding/hex"
	"strings"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
)

func discoveryBundle(key string) ([32]byte, error) {
	var bundle [32]byte
	raw, err := hex.DecodeString(strings.TrimPrefix(key, "bundle-"))
	if err != nil || len(raw) != 32 {
		return bundle, denied()
	}
	copy(bundle[:], raw)
	if bundle == [32]byte{} || key != client.DiscoveryKey(bundle) {
		return [32]byte{}, denied()
	}
	return bundle, nil
}
func (c *Channel) readResource(ctx context.Context, se Session, kind agentv1.ResourceKind, key string, revision uint64) (StoredResource, error) {
	switch kind {
	case agentv1.ResourceKind_RESOURCE_KIND_REVOCATION:
		if key != client.RevocationKey {
			return StoredResource{}, denied()
		}
		return c.sessions.RevocationResource(ctx, se, revision, c.releaseVerifier)
	case agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION:
		return c.sessions.ActivationResource(ctx, se, key, revision, c.releaseVerifier)
	case agentv1.ResourceKind_RESOURCE_KIND_POLICY:
		return c.sessions.Resource(ctx, se, key, revision)
	case agentv1.ResourceKind_RESOURCE_KIND_BUNDLE:
		bundle, err := discoveryBundle(key)
		if err != nil {
			return StoredResource{}, err
		}
		return c.sessions.BundleResource(ctx, se, bundle, revision)
	case agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY:
		if c.discovery == nil {
			return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, "discovery unavailable")
		}
		bundle, err := discoveryBundle(key)
		if err != nil {
			return StoredResource{}, err
		}
		if revision == 0 {
			return c.discovery.Discovery(ctx, se, bundle)
		}
		return c.sessions.DiscoveryResource(ctx, se, bundle, revision)
	default:
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, "resource capability unavailable")
	}
}

func (c *Channel) discoveryUpdate(ctx context.Context, se Session, host, id string, sub subscription) (*agentv1.Envelope, subscription, error) {
	read, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resource, err := c.readResource(read, se, sub.kind, sub.key, 0)
	if err != nil {
		return nil, sub, err
	}
	if resource.Revision <= sub.revision {
		return nil, sub, nil
	}
	sub.revision = resource.Revision
	sub.sequence++
	message := resourceMessage(host, se, &agentv1.Envelope{SubscriptionId: id}, sub.kind, sub.key, resource, true)
	message.StreamSeq = sub.sequence
	return message, sub, nil
}
