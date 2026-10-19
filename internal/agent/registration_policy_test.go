package agent

import (
	"testing"
)

func TestRegistrationAddressAdmission(t *testing.T) {
	t.Parallel()
	policy := RegistrationPolicy{AllowedNetworks: []string{"10.20.0.0/16", "fd00::/8", "127.0.0.0/8"}, ForbiddenAddresses: []string{"10.20.0.1"}, MaxInstances: 2}
	if err := policy.validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		address string
		allowed bool
	}{
		{"10.20.0.10", true}, {"fd00::10", true}, {"10.20.0.1", false}, {"10.21.0.10", false},
		{"127.0.0.1", false}, {"::1", false}, {"0.0.0.0", false}, {"::", false},
		{"169.254.169.254", false}, {"fe80::1", false}, {"fe80::1%eth0", false}, {"224.0.0.1", false},
		{"::ffff:10.20.0.10", false}, {"localhost", false}, {"10.20.0.10:8443", false},
		{"http://10.20.0.10", false}, {"fd00:0:0:0:0:0:0:10", false}, {"255.255.255.255", false},
	} {
		if got := policy.allows(test.address); got != test.allowed {
			t.Errorf("address %q allowed=%v, want %v", test.address, got, test.allowed)
		}
	}
	policy.AllowLoopback = true
	if !policy.allows("127.0.0.1") {
		t.Fatal("explicit local deployment profile ignored")
	}
}
func TestRegistrationPolicyRejectsUnboundedOrAmbiguousConfiguration(t *testing.T) {
	t.Parallel()
	for _, policy := range []RegistrationPolicy{
		{}, {AllowedNetworks: []string{"0.0.0.0/0"}, MaxInstances: 1},
		{AllowedNetworks: []string{"10.20.0.1/16"}, MaxInstances: 1},
		{AllowedNetworks: []string{"10.20.0.0/16", "10.20.0.0/16"}, MaxInstances: 1},
		{AllowedNetworks: []string{"FD00::/8"}, MaxInstances: 1},
		{AllowedNetworks: []string{"10.20.0.0/16"}, MaxInstances: 100001},
		{AllowedNetworks: []string{"10.20.0.0/16"}, ForbiddenAddresses: []string{"hostname"}, MaxInstances: 1},
	} {
		if err := policy.validate(); err == nil {
			t.Errorf("accepted invalid policy: %+v", policy)
		}
	}
}
func TestRegistrationServerIdentityIsExplicitDNSName(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"checkout.internal", "build-a.checkout.example"} {
		if !validServerName(value) {
			t.Fatalf("valid server identity %q rejected", value)
		}
	}
	for _, value := range []string{"", "10.20.0.10", "checkout.internal.", "*.internal", "CHECKOUT.internal", "https://checkout.internal", "name@checkout.internal", "bad..internal", "-bad.internal", "bad_.internal", "bad\x00.internal"} {
		if validServerName(value) {
			t.Fatalf("ambiguous server identity %q accepted", value)
		}
	}
}
