package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"slices"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
)

const RevocationKey = "revocation"
const MaxRevokedBundles = maxRevokedBundles

// RevocationDocument is a cumulative denial catalog. Cancellation is sticky per
// Bundle, so a later urgent denial does not cancel previously graceful streams.
type RevocationDocument struct {
	SchemaVersion     uint32   `json:"schemaVersion"`
	Purpose           string   `json:"purpose"`
	Scope             Scope    `json:"scope"`
	ControlPlaneEpoch uint64   `json:"controlPlaneEpoch,string"`
	Epoch             uint64   `json:"epoch,string"`
	BundleHashes      []string `json:"bundleHashes"`
	CancelledHashes   []string `json:"cancelledHashes"`
}

func MarshalRevocation(d RevocationDocument) ([]byte, error) {
	if d.SchemaVersion != 1 || d.Purpose != "bundle-revocation" || !validText(d.Scope.WorkspaceID) || !validText(d.Scope.EnvironmentID) || !validText(d.Scope.ServiceKey) || d.ControlPlaneEpoch == 0 || d.Epoch == 0 || len(d.BundleHashes) == 0 || len(d.BundleHashes) > MaxRevokedBundles || len(d.CancelledHashes) > len(d.BundleHashes) || !slices.IsSorted(d.BundleHashes) || !slices.IsSorted(d.CancelledHashes) {
		return nil, ErrResource
	}
	for i, h := range d.BundleHashes {
		if !bundleDigest(h) || i > 0 && h == d.BundleHashes[i-1] {
			return nil, ErrResource
		}
	}
	for i, h := range d.CancelledHashes {
		if i > 0 && h == d.CancelledHashes[i-1] || !slices.Contains(d.BundleHashes, h) {
			return nil, ErrResource
		}
	}
	return json.Marshal(d, json.Deterministic(true))
}
func VerifyRevocation(v *Verifier, r *agentv1.SignedResource, scope Scope, mark Watermark, now time.Time) (RevocationDocument, Claims, error) {
	claims, err := v.Verify(r, scope, agentv1.ResourceKind_RESOURCE_KIND_REVOCATION, RevocationKey, mark, now)
	if err != nil {
		return RevocationDocument{}, Claims{}, err
	}
	var d RevocationDocument
	if json.Unmarshal(r.Payload, &d, json.RejectUnknownMembers(true)) != nil {
		return d, claims, ErrResource
	}
	body, err := MarshalRevocation(d)
	if err != nil || !bytes.Equal(body, r.Payload) || d.Scope != scope || d.Epoch > claims.Revision || d.ControlPlaneEpoch != claims.ControlPlaneEpoch || sha256.Sum256(body) != claims.BundleHash {
		return d, claims, ErrResource
	}
	return d, claims, nil
}
