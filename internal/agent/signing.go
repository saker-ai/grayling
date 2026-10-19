// Package agent implements control-plane Agent services.
package agent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	agentclient "github.com/saker-ai/grayling/pkg/agent"
)

// SignResource is control-plane-only; the public Agent library holds no signing
// keys. The caller authorizes scope and assigns revisions before invoking it.
func SignResource(c agentclient.Claims, payload []byte, key ed25519.PrivateKey) (*agentv1.SignedResource, error) {
	if len(key) != ed25519.PrivateKeySize || len(payload) > agentclient.MaxResourceBytes {
		return nil, agentclient.ErrResource
	}
	c.PayloadHash = sha256.Sum256(payload)
	claims, err := agentclient.MarshalClaims(c)
	if err != nil {
		return nil, err
	}
	signature := ed25519.Sign(key, append([]byte(agentclient.SignatureDomain), claims...))
	return &agentv1.SignedResource{Claims: claims, Payload: bytes.Clone(payload), KeyId: c.KeyID, Signature: signature}, nil
}
