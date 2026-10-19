//go:build integration

package agent_test

import (
	"testing"
	"time"

	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
)

func TestInstanceDiscoveryEncodingRequiresNamingConfirmation(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	registered := registerInstance(t, f, "discovery-codec")
	before, err := f.a.DiscoveryCandidates(t.Context(), f.session)
	if err != nil {
		t.Fatal(err)
	}
	body, err := control.EncodeDiscoveryCandidates(f.g.Scope, before)
	if err != nil {
		t.Fatal(err)
	}
	empty := decodeAdmin[client.DiscoveryDocument](t, body)
	if empty.Purpose != "registered-candidates" || len(empty.Members) != 0 {
		t.Fatal("unconfirmed member gained signed-payload presence")
	}
	intents, err := f.a.PendingNamingIntents(t.Context(), f.g.Scope)
	if err != nil || len(intents) != 1 {
		t.Fatal("missing intent", err)
	}
	// Simulated readback acknowledgement tests the governance/encoding barrier;
	// real substrate readback is covered by TestInstanceRealEmbeddedNaming.
	if err := f.b.ConfirmNamingIntent(t.Context(), intents[0]); err != nil {
		t.Fatal(err)
	}
	after, err := f.a.DiscoveryCandidates(t.Context(), f.session)
	if err != nil {
		t.Fatal(err)
	}
	body, err = control.EncodeDiscoveryCandidates(f.g.Scope, after)
	if err != nil {
		t.Fatal(err)
	}
	document := decodeAdmin[client.DiscoveryDocument](t, body)
	if len(document.Members) != 1 || document.MembershipRevision <= empty.MembershipRevision || document.Members[0].Lease.ID != registered.Instance.Lease.Id {
		t.Fatal("confirmed set does not match coherent governance capture")
	}
	after.ExpiresAt = after.ExpiresAt.Add(2 * time.Minute)
	if _, err := control.EncodeDiscoveryCandidates(f.g.Scope, after); err == nil {
		t.Fatal("payload expiry exceeds member lease")
	}
	foreign := f.g.Scope
	foreign.EnvironmentID = "foreign"
	if _, err := control.EncodeDiscoveryCandidates(foreign, before); err == nil {
		t.Fatal("mismatched scope accepted")
	}
}
