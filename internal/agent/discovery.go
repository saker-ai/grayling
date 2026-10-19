package agent

import (
	"cmp"
	client "github.com/saker-ai/grayling/pkg/agent"
	"slices"
)

// EncodeDiscoveryCandidates converts a coherent governance capture into the
// signed payload schema. Only exact Naming-confirmed members are published.
// It assigns no artifact revision, signs nothing, and grants no Prepared or
// routing eligibility: the durable publisher must allocate immutable versions.
func EncodeDiscoveryCandidates(scope client.Scope, snapshot CandidateSnapshot) ([]byte, error) {
	if !scopeValid(scope) || snapshot.Revision == 0 || snapshot.ExpiresAt.IsZero() || len(snapshot.Members) > client.MaxDiscoveryMembers {
		return nil, client.ErrResource
	}
	document := client.DiscoveryDocument{SchemaVersion: 1, Purpose: "registered-candidates", Scope: scope, MembershipRevision: snapshot.Revision, Members: make([]client.DiscoveryMember, 0, len(snapshot.Members))}
	for _, candidate := range snapshot.Members {
		i := candidate.Instance
		if !validInstanceDescriptor(i, scope) || i.Lease == nil || !identifier(i.Lease.Id) || i.Lease.Epoch == 0 {
			return nil, client.ErrResource
		}
		if !candidate.NamingConfirmed {
			continue
		}
		if i.Lease.ExpiresUnixMs < snapshot.ExpiresAt.UnixMilli() {
			return nil, client.ErrResource
		}
		document.Members = append(document.Members, client.DiscoveryMember{InstanceID: i.InstanceId, BootID: i.BootId, BuildID: i.BuildId, ImageDigest: i.ImageDigest, Address: i.Address, Port: i.Port, TLSServerName: i.TlsServerName, Zone: i.Zone, ProtocolVersion: i.ProtocolVersion, Ready: i.Ready, Draining: i.Draining, Lease: client.DiscoveryLease{ID: i.Lease.Id, Epoch: i.Lease.Epoch, ExpiresUnixMs: i.Lease.ExpiresUnixMs}})
	}
	slices.SortFunc(document.Members, func(a, b client.DiscoveryMember) int { return cmp.Compare(a.InstanceID, b.InstanceID) })
	return client.MarshalDiscovery(document)
}
