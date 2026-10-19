package agent_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json/v2"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

func activationSDKFixture(t *testing.T) (*cacheFixture, agent.ActivationDocument, *agentv1.SignedResource, [32]byte) {
	t.Helper()
	f, bundle, _ := bundleFixture(t)
	body, err := agent.MarshalBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	signed, hash := signBundle(t, f, body, nil)
	now := f.opts.Now()
	m := agent.ActivationMember{Instance: agent.DiscoveryMember{InstanceID: "instance", BootID: "boot", BuildID: "stable", ImageDigest: "sha256:" + strings.Repeat("a", 64), Address: "10.20.0.10", Port: 8443, TLSServerName: "checkout.internal", Zone: "az-a", ProtocolVersion: 1, Ready: true, Lease: agent.DiscoveryLease{ID: "lease", Epoch: math.MaxUint64, ExpiresUnixMs: now.Add(time.Minute).UnixMilli()}}, PrepareCommandID: "prepare-1", EvidenceRevision: math.MaxUint64, PreparedUnixMs: now.UnixMilli(), ExpiresUnixMs: now.Add(30 * time.Second).UnixMilli()}
	d := agent.ActivationDocument{SchemaVersion: 1, Purpose: "activation-view", Scope: f.opts.Scope, ControlPlaneEpoch: 1, Generation: math.MaxUint64, ReleaseID: "release-" + strconv.FormatUint(math.MaxUint64, 10), BundleHash: hex.EncodeToString(hash[:]), BundleArtifactRevision: math.MaxUint64, PolicyRevision: math.MaxUint64, DiscoveryRevision: math.MaxUint64, PreparedRevision: math.MaxUint64, Members: []agent.ActivationMember{m}, Sources: []agent.ActivationSource{{InstanceID: m.Instance.InstanceID, BootID: m.Instance.BootID, LeaseID: m.Instance.Lease.ID, LeaseEpoch: m.Instance.Lease.Epoch, PrepareCommandID: m.PrepareCommandID, CommandID: "activate-1"}}}
	return f, d, signed, hash
}
func signActivationSDK(t *testing.T, f *cacheFixture, d agent.ActivationDocument, body []byte, hash [32]byte) *agentv1.SignedResource {
	t.Helper()
	signed, err := control.SignResource(agent.Claims{Scope: f.opts.Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION, Key: agent.ActivationKey(d.Generation), ControlPlaneEpoch: 1, Revision: math.MaxUint64, BundleHash: hash, IssuedUnixMs: f.opts.Now().UnixMilli(), ExpiresUnixMs: f.opts.Now().Add(20 * time.Second).UnixMilli(), KeyID: "key"}, body, f.private)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
func TestActivationSignedSchemaPrecisionAndExactInputs(t *testing.T) {
	f, d, bundle, hash := activationSDKFixture(t)
	body, err := agent.MarshalActivation(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"generation":"18446744073709551615"`)) {
		t.Fatal("lost generation precision")
	}
	signed := signActivationSDK(t, f, d, body, hash)
	view, claims, err := agent.VerifyActivation(f.opts.Verifier, signed, f.opts.Scope, d.Generation, hash, agent.Watermark{}, f.opts.Now())
	if err != nil || claims.Revision != math.MaxUint64 {
		t.Fatal("activation rejected", err)
	}
	if _, _, err := view.VerifyBundle(f.opts.Verifier, bundle, agent.Watermark{}, f.opts.Now()); err != nil {
		t.Fatal("exact inputs rejected", err)
	}
	copy := view.Document()
	copy.Members[0].Instance.BootID = "mutated"
	copy.Sources[0].BootID = "mutated"
	if view.Document().Members[0].Instance.BootID == "mutated" || view.Document().Sources[0].BootID == "mutated" {
		t.Fatal("mutable view alias")
	}
	signed.Payload[0] ^= 1
	signed.Signature[0] ^= 1
	if _, _, err := view.VerifyBundle(f.opts.Verifier, bundle, agent.Watermark{}, f.opts.Now()); err != nil {
		t.Fatal("retained caller alias", err)
	}
	bad := proto.Clone(bundle).(*agentv1.SignedResource)
	bad.Payload[0] ^= 1
	if _, _, err := view.VerifyBundle(f.opts.Verifier, bad, agent.Watermark{}, f.opts.Now()); err == nil {
		t.Fatal("tampered Bundle joined")
	}
	if _, _, err := view.VerifyBundle(f.opts.Verifier, bundle, agent.Watermark{}, f.opts.Now().Add(21*time.Second)); err == nil {
		t.Fatal("expired activation joined")
	}
}
func TestActivationRejectsSignedInconsistentViews(t *testing.T) {
	cases := []struct {
		name   string
		change func(*agent.ActivationDocument)
	}{
		{"scope", func(d *agent.ActivationDocument) { d.Scope.ServiceKey = "other" }},
		{"generation", func(d *agent.ActivationDocument) {
			d.Generation--
			d.ReleaseID = "release-" + strconv.FormatUint(d.Generation, 10)
		}},
		{"bundle", func(d *agent.ActivationDocument) { d.BundleHash = strings.Repeat("c", 64) }},
		{"source boot", func(d *agent.ActivationDocument) { d.Sources[0].BootID = "other" }},
		{"source lease", func(d *agent.ActivationDocument) { d.Sources[0].LeaseEpoch-- }},
		{"duplicate member", func(d *agent.ActivationDocument) { d.Members = append(d.Members, d.Members[0]) }},
		{"not ready", func(d *agent.ActivationDocument) { d.Members[0].Instance.Ready = false }},
		{"draining", func(d *agent.ActivationDocument) { d.Members[0].Instance.Draining = true }},
		{"future evidence", func(d *agent.ActivationDocument) { d.Members[0].PreparedUnixMs += 6000 }},
		{"short evidence", func(d *agent.ActivationDocument) { d.Members[0].ExpiresUnixMs -= 15000 }},
		{"catalog", func(d *agent.ActivationDocument) { d.PreparedRevision-- }},
		{"epoch", func(d *agent.ActivationDocument) { d.ControlPlaneEpoch++ }},
		{"schema", func(d *agent.ActivationDocument) { d.SchemaVersion++ }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, d, _, hash := activationSDKFixture(t)
			generation := d.Generation
			tc.change(&d)
			body, err := json.Marshal(d, json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			signed := signActivationSDK(t, f, d, body, hash)
			if _, _, err := agent.VerifyActivation(f.opts.Verifier, signed, f.opts.Scope, generation, hash, agent.Watermark{}, f.opts.Now()); err == nil {
				t.Fatal("signed inconsistent view accepted")
			}
		})
	}
}

func TestActivationExactDependencyMismatchAndCanonicalPayload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*agent.ActivationDocument)
	}{
		{"policy revision", func(d *agent.ActivationDocument) { d.PolicyRevision-- }},
		{"bundle artifact", func(d *agent.ActivationDocument) { d.BundleArtifactRevision-- }},
		{"member image", func(d *agent.ActivationDocument) {
			d.Members[0].Instance.ImageDigest = "sha256:" + strings.Repeat("b", 64)
		}},
		{"member build", func(d *agent.ActivationDocument) { d.Members[0].Instance.BuildID = "unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, d, bundle, hash := activationSDKFixture(t)
			tc.change(&d)
			body, err := agent.MarshalActivation(d)
			if err != nil {
				t.Fatal(err)
			}
			view, _, err := agent.VerifyActivation(f.opts.Verifier, signActivationSDK(t, f, d, body, hash), f.opts.Scope, d.Generation, hash, agent.Watermark{}, f.opts.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := view.VerifyBundle(f.opts.Verifier, bundle, agent.Watermark{}, f.opts.Now()); err == nil {
				t.Fatal("inexact dependency joined")
			}
		})
	}
	f, d, _, hash := activationSDKFixture(t)
	body, err := agent.MarshalActivation(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range [][]byte{append([]byte(" "), body...), append(append([]byte{}, body[:len(body)-1]...), []byte(`,"unknown":true}`)...)} {
		if _, _, err := agent.VerifyActivation(f.opts.Verifier, signActivationSDK(t, f, d, payload, hash), f.opts.Scope, d.Generation, hash, agent.Watermark{}, f.opts.Now()); err == nil {
			t.Fatal("noncanonical or extended view accepted")
		}
	}
}
