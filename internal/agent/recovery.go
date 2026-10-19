package agent

import (
	"crypto/ed25519"
	client "github.com/saker-ai/grayling/pkg/agent"
)

// SignRecovery requires an independently managed recovery signing key. Its
// caller must authorize and audit the backup recovery before invoking it.
func SignRecovery(claims client.RecoveryClaims, key ed25519.PrivateKey) (*client.RecoveryGrant, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, client.ErrResource
	}
	body, err := client.MarshalRecoveryClaims(claims)
	if err != nil {
		return nil, err
	}
	return &client.RecoveryGrant{Claims: body, Signature: ed25519.Sign(key, append([]byte(client.RecoverySignatureDomain), body...))}, nil
}
