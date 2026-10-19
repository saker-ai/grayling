package agent

import (
	"encoding/hex"
	"encoding/json/v2"
	"net/netip"
	"strings"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

const MaxDiscoveryMembers = 100000

// DiscoveryDocument is the versioned payload inside a signed discovery resource.
// MembershipRevision is independent of the immutable artifact revision in Claims.
// Members have Naming readback evidence, never implicit Prepared authorization.
type DiscoveryDocument struct {
	SchemaVersion      uint32            `json:"schemaVersion"`
	Purpose            string            `json:"purpose"`
	Scope              Scope             `json:"scope"`
	MembershipRevision uint64            `json:"membershipRevision,string"`
	Members            []DiscoveryMember `json:"members"`
}
type DiscoveryMember struct {
	InstanceID      string         `json:"instanceId"`
	BootID          string         `json:"bootId"`
	BuildID         string         `json:"buildId"`
	ImageDigest     string         `json:"imageDigest"`
	Address         string         `json:"address"`
	Port            uint32         `json:"port"`
	TLSServerName   string         `json:"tlsServerName"`
	Zone            string         `json:"zone"`
	ProtocolVersion uint32         `json:"protocolVersion"`
	Ready           bool           `json:"ready"`
	Draining        bool           `json:"draining"`
	Lease           DiscoveryLease `json:"lease"`
}
type DiscoveryLease struct {
	ID            string `json:"id"`
	Epoch         uint64 `json:"epoch,string"`
	ExpiresUnixMs int64  `json:"expiresUnixMs,string"`
}

// Discovery holds a fully checked immutable candidate set. Routing must join
// candidates with independently verified policy, Prepared and release evidence.
type Discovery struct {
	members            []*agentv1.Instance
	membershipRevision uint64
	expiresAt          time.Time
}

func (d *Discovery) MembershipRevision() uint64 { return d.membershipRevision }
func (d *Discovery) ExpiresAt() time.Time       { return d.expiresAt }
func (d *Discovery) Candidates() []*agentv1.Instance {
	members := make([]*agentv1.Instance, len(d.members))
	for i, member := range d.members {
		members[i] = proto.Clone(member).(*agentv1.Instance)
	}
	return members
}

// DiscoveryKey gives each target Bundle an independent rollback watermark.
func DiscoveryKey(bundleHash [32]byte) string { return "bundle-" + hex.EncodeToString(bundleHash[:]) }

func discoveryID(s string) bool { return validText(s) && len(s) <= 128 && strings.TrimSpace(s) == s }
func discoverySNI(s string) bool {
	if len(s) == 0 || len(s) > 253 || strings.ToLower(s) != s {
		return false
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return false
	}
	for part := range strings.SplitSeq(s, ".") {
		if len(part) == 0 || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}
func (m DiscoveryMember) valid() bool {
	if !discoveryID(m.InstanceID) || !discoveryID(m.BootID) || !discoveryID(m.BuildID) || !discoveryID(m.Zone) || !discoveryID(m.Lease.ID) || m.Lease.Epoch == 0 || m.Lease.ExpiresUnixMs <= 0 || m.Port == 0 || m.Port > 65535 || m.ProtocolVersion != agentv1.ProtocolMajor || !discoverySNI(m.TLSServerName) || len(m.ImageDigest) != 71 || !strings.HasPrefix(m.ImageDigest, "sha256:") || strings.ToLower(m.ImageDigest) != m.ImageDigest {
		return false
	}
	if _, err := hex.DecodeString(m.ImageDigest[7:]); err != nil {
		return false
	}
	ip, err := netip.ParseAddr(m.Address)
	return err == nil && ip.String() == m.Address && ip.Zone() == "" && !ip.Is4In6() && (ip.IsGlobalUnicast() || ip.IsLoopback()) && !ip.IsLinkLocalUnicast() && !ip.IsMulticast() && !ip.IsUnspecified()
}
func (d DiscoveryDocument) valid() bool {
	if d.SchemaVersion != 1 || d.Purpose != "registered-candidates" || !discoveryID(d.Scope.WorkspaceID) || !discoveryID(d.Scope.EnvironmentID) || !discoveryID(d.Scope.ServiceKey) || d.MembershipRevision == 0 || d.Members == nil || len(d.Members) > MaxDiscoveryMembers {
		return false
	}
	previous := ""
	leases := make(map[string]bool, len(d.Members))
	for _, m := range d.Members {
		if !m.valid() || m.InstanceID <= previous || leases[m.Lease.ID] {
			return false
		}
		previous = m.InstanceID
		leases[m.Lease.ID] = true
	}
	return true
}

// MarshalDiscovery preserves a unique, sorted full-set payload. A refresh must
// receive a new immutable artifact revision even when membership is unchanged.
func MarshalDiscovery(d DiscoveryDocument) ([]byte, error) {
	if !d.valid() {
		return nil, ErrResource
	}
	body, err := json.Marshal(d, json.Deterministic(true))
	if err != nil || len(body) > MaxResourceBytes {
		return nil, ErrResource
	}
	return body, nil
}

// VerifyDiscovery checks signed candidates against an independently trusted
// clock, exact target Bundle and durable per-resource watermark. It neither
// advances durable state nor grants routing readiness.
func VerifyDiscovery(v *Verifier, r *agentv1.SignedResource, scope Scope, bundleHash [32]byte, mark Watermark, now time.Time) (*Discovery, Claims, error) {
	if bundleHash == [32]byte{} {
		return nil, Claims{}, ErrResource
	}
	claims, err := v.Verify(r, scope, agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY, DiscoveryKey(bundleHash), mark, now)
	if err != nil {
		return nil, Claims{}, err
	}
	if claims.BundleHash != bundleHash {
		return nil, Claims{}, ErrScope
	}
	if claims.ExpiresUnixMs-claims.IssuedUnixMs > 60000 {
		return nil, Claims{}, ErrResource
	}
	var document DiscoveryDocument
	if err := json.Unmarshal(r.Payload, &document, json.RejectUnknownMembers(true)); err != nil || !document.valid() {
		return nil, Claims{}, ErrResource
	}
	if document.Scope != scope {
		return nil, Claims{}, ErrScope
	}
	result := &Discovery{membershipRevision: document.MembershipRevision, expiresAt: time.UnixMilli(claims.ExpiresUnixMs), members: make([]*agentv1.Instance, 0, len(document.Members))}
	for _, m := range document.Members {
		if m.Lease.ExpiresUnixMs < claims.ExpiresUnixMs {
			return nil, Claims{}, ErrResource
		}
		result.members = append(result.members, &agentv1.Instance{Scope: &agentv1.Scope{WorkspaceId: scope.WorkspaceID, EnvironmentId: scope.EnvironmentID, ServiceKey: scope.ServiceKey}, InstanceId: m.InstanceID, BootId: m.BootID, BuildId: m.BuildID, ImageDigest: m.ImageDigest, Address: m.Address, Port: m.Port, TlsServerName: m.TLSServerName, Zone: m.Zone, ProtocolVersion: m.ProtocolVersion, Ready: m.Ready, Draining: m.Draining, Lease: &agentv1.Lease{Id: m.Lease.ID, Epoch: m.Lease.Epoch, ExpiresUnixMs: m.Lease.ExpiresUnixMs}})
	}
	return result, claims, nil
}
