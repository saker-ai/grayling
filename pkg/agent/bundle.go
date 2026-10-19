package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"maps"
	"slices"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/pkg/model"
	"google.golang.org/protobuf/proto"
)

const MaxBundleBuilds = 256
const MaxBuildResources = 1024

// BundleDocument fixes policy, build mapping and application bytes before
// Prepare. Membership and release generation deliberately remain independent.
type BundleDocument struct {
	SchemaVersion          uint32            `json:"schemaVersion"`
	Purpose                string            `json:"purpose"`
	Scope                  Scope             `json:"scope"`
	Policy                 BundlePolicy      `json:"policy"`
	RouteFlag              string            `json:"routeFlag"`
	ValueBuilds            map[string]string `json:"valueBuilds"`
	MinimumProtocolVersion uint32            `json:"minimumProtocolVersion"`
	DefaultBehavior        string            `json:"defaultBehavior"`
	DefaultBuild           string            `json:"defaultBuild"`
	ValidUntilUnixMs       int64             `json:"validUntilUnixMs,string"`
	Builds                 []BundleBuild     `json:"builds"`
}
type BundlePolicy struct {
	Key               string `json:"key"`
	ControlPlaneEpoch uint64 `json:"controlPlaneEpoch,string"`
	Revision          uint64 `json:"revision,string"`
	PayloadSHA256     string `json:"payloadSha256"`
}
type BundleBuild struct {
	ID              string           `json:"id"`
	ImageDigest     string           `json:"imageDigest"`
	ProtocolVersion uint32           `json:"protocolVersion"`
	Resources       []BundleResource `json:"resources"`
}
type BundleResource struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
}

// Bundle is immutable, verified release input. It grants neither Prepared
// membership nor activation; its dependencies still need exact verification.
type Bundle struct {
	document BundleDocument
	claims   Claims
	resource *agentv1.SignedResource
}

func BundleKey(hash [32]byte) string { return "bundle-" + hex.EncodeToString(hash[:]) }
func (b *Bundle) Hash() [32]byte     { return b.claims.BundleHash }
func (b *Bundle) Claims() Claims     { return b.claims }
func (b *Bundle) Document() BundleDocument {
	d := b.document
	d.ValueBuilds = maps.Clone(d.ValueBuilds)
	d.Builds = slices.Clone(d.Builds)
	for i := range d.Builds {
		d.Builds[i].Resources = slices.Clone(d.Builds[i].Resources)
	}
	return d
}
func bundleDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(s)
	return err == nil && hex.EncodeToString(decoded) == s && !bytes.Equal(decoded, make([]byte, 32))
}
func (d BundleDocument) valid() bool {
	if d.SchemaVersion != 1 || d.Purpose != "release-bundle" || !discoveryID(d.Scope.WorkspaceID) || !discoveryID(d.Scope.EnvironmentID) || !discoveryID(d.Scope.ServiceKey) || !model.KeyPattern.MatchString(d.Policy.Key) || d.Policy.ControlPlaneEpoch == 0 || d.Policy.Revision == 0 || !bundleDigest(d.Policy.PayloadSHA256) || !model.KeyPattern.MatchString(d.RouteFlag) || d.MinimumProtocolVersion != agentv1.ProtocolMajor || d.ValidUntilUnixMs <= 0 || len(d.Builds) == 0 || len(d.Builds) > MaxBundleBuilds || len(d.ValueBuilds) == 0 || len(d.ValueBuilds) > 1024 {
		return false
	}
	builds := make(map[string]bool, len(d.Builds))
	previous := ""
	for _, build := range d.Builds {
		if !discoveryID(build.ID) || build.ID <= previous || build.ProtocolVersion != agentv1.ProtocolMajor || len(build.ImageDigest) != 71 || build.ImageDigest[:7] != "sha256:" || !bundleDigest(build.ImageDigest[7:]) || build.Resources == nil || len(build.Resources) > MaxBuildResources {
			return false
		}
		previous = build.ID
		builds[build.ID] = true
		previousKey := ""
		for _, resource := range build.Resources {
			if !model.KeyPattern.MatchString(resource.Key) || resource.Key <= previousKey || !bundleDigest(resource.SHA256) {
				return false
			}
			previousKey = resource.Key
		}
	}
	if d.DefaultBehavior != "reject" && d.DefaultBehavior != "build" || d.DefaultBehavior == "reject" && d.DefaultBuild != "" || d.DefaultBehavior == "build" && !builds[d.DefaultBuild] {
		return false
	}
	for value, build := range d.ValueBuilds {
		if !discoveryID(value) || !builds[build] {
			return false
		}
	}
	return true
}

// MarshalBundle requires sorted builds/resources and emits deterministic bytes.
// The SHA-256 of these bytes is the Bundle identity, including its hard expiry.
func MarshalBundle(d BundleDocument) ([]byte, error) {
	if !d.valid() {
		return nil, ErrResource
	}
	body, err := json.Marshal(d, json.Deterministic(true))
	if err != nil || len(body) > MaxResourceBytes {
		return nil, ErrResource
	}
	return body, nil
}
func VerifyBundle(v *Verifier, r *agentv1.SignedResource, scope Scope, hash [32]byte, mark Watermark, now time.Time) (*Bundle, Claims, error) {
	if hash == [32]byte{} {
		return nil, Claims{}, ErrResource
	}
	claims, err := v.Verify(r, scope, agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, BundleKey(hash), mark, now)
	if err != nil {
		return nil, Claims{}, err
	}
	if claims.BundleHash != hash || sha256.Sum256(r.Payload) != hash {
		return nil, Claims{}, ErrResource
	}
	var d BundleDocument
	if err := json.Unmarshal(r.Payload, &d, json.RejectUnknownMembers(true)); err != nil {
		return nil, Claims{}, ErrResource
	}
	canonical, err := MarshalBundle(d)
	if err != nil || !bytes.Equal(canonical, r.Payload) || d.Policy.ControlPlaneEpoch != claims.ControlPlaneEpoch || claims.ExpiresUnixMs > d.ValidUntilUnixMs || claims.ExpiresUnixMs-claims.IssuedUnixMs > int64((24*time.Hour)/time.Millisecond) {
		return nil, Claims{}, ErrResource
	}
	if d.Scope != scope {
		return nil, Claims{}, ErrScope
	}
	return &Bundle{document: d, claims: claims, resource: proto.Clone(r).(*agentv1.SignedResource)}, claims, nil
}

// VerifyPolicy verifies the exact signed policy dependency and all routing
// variations. A policy default or failed evaluation is never implicit consent
// to a build; runtime code must apply DefaultBehavior explicitly.
func (b *Bundle) VerifyPolicy(v *Verifier, r *agentv1.SignedResource, mark Watermark, now time.Time) (*Policy, Claims, error) {
	if _, err := v.Verify(b.resource, b.document.Scope, agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, BundleKey(b.Hash()), b.claims.Watermark(), now); err != nil {
		return nil, Claims{}, err
	}
	p, claims, err := VerifyPolicy(v, r, b.document.Scope, b.document.Policy.Key, mark, now)
	if err != nil {
		return nil, Claims{}, err
	}
	expected := b.document.Policy
	if claims.ControlPlaneEpoch != expected.ControlPlaneEpoch || claims.Revision != expected.Revision || claims.BundleHash != b.Hash() || hex.EncodeToString(claims.PayloadHash[:]) != expected.PayloadSHA256 {
		return nil, Claims{}, ErrResource
	}
	if err := checkBundleRoutes(b.document, p); err != nil {
		return nil, Claims{}, err
	}
	return p, claims, nil
}

// CompileBundlePolicy validates the complete unsigned candidate before a
// control-plane transaction. It does not confer approval or verify signatures.
func CompileBundlePolicy(d BundleDocument, payload []byte) (*Policy, error) {
	if !d.valid() {
		return nil, ErrResource
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != d.Policy.PayloadSHA256 {
		return nil, ErrResource
	}
	p, err := CompilePolicy(payload, d.Scope)
	if err != nil {
		return nil, err
	}
	if err := checkBundleRoutes(d, p); err != nil {
		return nil, err
	}
	return p, nil
}
func checkBundleRoutes(d BundleDocument, p *Policy) error {
	flag := p.evaluator.Snapshot().Flags[d.RouteFlag]
	if flag == nil || flag.Type != model.FlagString {
		return ErrResource
	}
	values := make(map[string]bool, len(flag.Variations))
	for _, value := range flag.Variations {
		text, ok := value.(string)
		if !ok {
			return ErrResource
		}
		if _, ok := d.ValueBuilds[text]; !ok {
			return ErrResource
		}
		values[text] = true
	}
	if len(values) != len(d.ValueBuilds) {
		return ErrResource
	}
	return nil
}
