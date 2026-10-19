package agent

import (
	"bytes"
	"encoding/hex"
	"encoding/json/v2"
	"slices"
	"strconv"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// ActivationDocument freezes exact resources, prepared membership and required
// sources at the activation decision. It proves authorization, never Applied.
type ActivationDocument struct {
	SchemaVersion          uint32             `json:"schemaVersion"`
	Purpose                string             `json:"purpose"`
	Scope                  Scope              `json:"scope"`
	ControlPlaneEpoch      uint64             `json:"controlPlaneEpoch,string"`
	Generation             uint64             `json:"generation,string"`
	ReleaseID              string             `json:"releaseId"`
	BundleHash             string             `json:"bundleHash"`
	BundleArtifactRevision uint64             `json:"bundleArtifactRevision,string"`
	PolicyRevision         uint64             `json:"policyRevision,string"`
	DiscoveryRevision      uint64             `json:"discoveryRevision,string"`
	PreparedRevision       uint64             `json:"preparedRevision,string"`
	RevocationEpoch        uint64             `json:"revocationEpoch,string"`
	Members                []ActivationMember `json:"members"`
	Sources                []ActivationSource `json:"sources"`
}
type ActivationMember struct {
	Instance         DiscoveryMember `json:"instance"`
	PrepareCommandID string          `json:"prepareCommandId"`
	EvidenceRevision uint64          `json:"evidenceRevision,string"`
	PreparedUnixMs   int64           `json:"preparedUnixMs,string"`
	ExpiresUnixMs    int64           `json:"expiresUnixMs,string"`
}
type ActivationSource struct {
	InstanceID       string `json:"instanceId"`
	BootID           string `json:"bootId"`
	LeaseID          string `json:"leaseId"`
	LeaseEpoch       uint64 `json:"leaseEpoch,string"`
	PrepareCommandID string `json:"prepareCommandId"`
	CommandID        string `json:"commandId"`
}

func ActivationKey(generation uint64) string {
	return "activation-" + strconv.FormatUint(generation, 10)
}
func (d ActivationDocument) valid() bool {
	if d.SchemaVersion != 1 || d.Purpose != "activation-view" || !discoveryID(d.Scope.WorkspaceID) || !discoveryID(d.Scope.EnvironmentID) || !discoveryID(d.Scope.ServiceKey) || d.ControlPlaneEpoch == 0 || d.Generation == 0 || d.ReleaseID != "release-"+strconv.FormatUint(d.Generation, 10) || !bundleDigest(d.BundleHash) || d.BundleArtifactRevision == 0 || d.PolicyRevision == 0 || d.DiscoveryRevision == 0 || d.PreparedRevision == 0 || len(d.Members) == 0 || len(d.Members) > MaxDiscoveryMembers || len(d.Sources) == 0 || len(d.Sources) > MaxDiscoveryMembers {
		return false
	}
	members := make(map[string]ActivationMember, len(d.Members))
	candidates := DiscoveryDocument{SchemaVersion: 1, Purpose: "registered-candidates", Scope: d.Scope, MembershipRevision: d.DiscoveryRevision, Members: make([]DiscoveryMember, 0, len(d.Members))}
	for _, m := range d.Members {
		if !discoveryID(m.PrepareCommandID) || m.EvidenceRevision == 0 || m.EvidenceRevision > d.PreparedRevision || m.PreparedUnixMs <= 0 || m.ExpiresUnixMs <= m.PreparedUnixMs || m.ExpiresUnixMs-m.PreparedUnixMs > 300000 || m.ExpiresUnixMs > m.Instance.Lease.ExpiresUnixMs || !m.Instance.Ready || m.Instance.Draining {
			return false
		}
		members[m.Instance.InstanceID] = m
		candidates.Members = append(candidates.Members, m.Instance)
	}
	if !candidates.valid() {
		return false
	}
	previous := ""
	commands := map[string]bool{}
	for _, s := range d.Sources {
		m, ok := members[s.InstanceID]
		if !ok || s.InstanceID <= previous || s.BootID != m.Instance.BootID || s.LeaseID != m.Instance.Lease.ID || s.LeaseEpoch != m.Instance.Lease.Epoch || s.PrepareCommandID != m.PrepareCommandID || !discoveryID(s.CommandID) || commands[s.CommandID] {
			return false
		}
		previous = s.InstanceID
		commands[s.CommandID] = true
	}
	return true
}
func MarshalActivation(d ActivationDocument) ([]byte, error) {
	if !d.valid() {
		return nil, ErrResource
	}
	body, err := json.Marshal(d, json.Deterministic(true))
	if err != nil || len(body) > MaxResourceBytes {
		return nil, ErrResource
	}
	return body, nil
}

type ActivationView struct {
	document ActivationDocument
	claims   Claims
	resource *agentv1.SignedResource
}

func (a *ActivationView) Document() ActivationDocument {
	d := a.document
	d.Members = slices.Clone(d.Members)
	d.Sources = slices.Clone(d.Sources)
	return d
}
func (a *ActivationView) Claims() Claims { return a.claims }
func VerifyActivation(v *Verifier, r *agentv1.SignedResource, scope Scope, generation uint64, bundle [32]byte, mark Watermark, now time.Time) (*ActivationView, Claims, error) {
	if v == nil || generation == 0 || bundle == [32]byte{} {
		return nil, Claims{}, ErrResource
	}
	claims, err := v.Verify(r, scope, agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION, ActivationKey(generation), mark, now)
	if err != nil {
		return nil, Claims{}, err
	}
	if claims.BundleHash != bundle || claims.ExpiresUnixMs-claims.IssuedUnixMs > 60000 {
		return nil, Claims{}, ErrResource
	}
	var d ActivationDocument
	if json.Unmarshal(r.Payload, &d, json.RejectUnknownMembers(true)) != nil || !d.valid() || d.Scope != scope || d.Generation != generation || d.ControlPlaneEpoch != claims.ControlPlaneEpoch || d.BundleHash != hex.EncodeToString(bundle[:]) {
		return nil, Claims{}, ErrResource
	}
	body, err := MarshalActivation(d)
	if err != nil || !bytes.Equal(body, r.Payload) {
		return nil, Claims{}, ErrResource
	}
	for _, m := range d.Members {
		if m.ExpiresUnixMs < claims.ExpiresUnixMs || m.PreparedUnixMs > claims.IssuedUnixMs+5000 {
			return nil, Claims{}, ErrResource
		}
	}
	return &ActivationView{document: d, claims: claims, resource: proto.Clone(r).(*agentv1.SignedResource)}, claims, nil
}

// VerifyBundle joins exact immutable inputs. It does not install a RuntimeView,
// advance generation floors or report that application resources are healthy.
func (a *ActivationView) VerifyBundle(v *Verifier, r *agentv1.SignedResource, mark Watermark, now time.Time) (*Bundle, Claims, error) {
	if a == nil || v == nil {
		return nil, Claims{}, ErrResource
	}
	if _, err := v.Verify(a.resource, a.document.Scope, agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION, ActivationKey(a.document.Generation), a.claims.Watermark(), now); err != nil {
		return nil, Claims{}, err
	}
	b, claims, err := VerifyBundle(v, r, a.document.Scope, a.claims.BundleHash, mark, now)
	if err != nil {
		return nil, Claims{}, err
	}
	d := b.Document()
	if claims.Revision != a.document.BundleArtifactRevision || claims.ControlPlaneEpoch != a.claims.ControlPlaneEpoch || d.Policy.Revision != a.document.PolicyRevision || a.claims.ExpiresUnixMs > claims.ExpiresUnixMs {
		return nil, Claims{}, ErrResource
	}
	builds := map[string]BundleBuild{}
	for _, build := range d.Builds {
		builds[build.ID] = build
	}
	for _, m := range a.document.Members {
		build, ok := builds[m.Instance.BuildID]
		if !ok || build.ImageDigest != m.Instance.ImageDigest || build.ProtocolVersion != m.Instance.ProtocolVersion || build.ProtocolVersion < d.MinimumProtocolVersion {
			return nil, Claims{}, ErrResource
		}
	}
	return b, claims, nil
}
