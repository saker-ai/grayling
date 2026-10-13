package model_test

import (
	"strings"
	"testing"

	"github.com/godeps/grayling/pkg/bucketing"
	"github.com/godeps/grayling/pkg/model"
)

// clientIPFlag builds a gray-routing flag that targets the reserved
// client_ip attribute (injected by golab-proxy from the trusted proxy
// chain) with in_cidr.
func clientIPFlag(register bool) *model.Flag {
	f := &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           "ip-gray", Type: model.FlagString,
		Variations: map[string]any{
			"control": "control-value", "candidate": "candidate-value",
		},
		DefaultRule: &model.Rule{Variation: "control"},
		Targeting: []model.Rule{{
			Name: "internal-net",
			Constraints: []model.Constraint{
				{Field: model.ClientIPField, Op: model.OpInCIDR, Value: "10.0.0.0/8"},
			},
			Variation: "candidate",
		}},
	}
	if register {
		f.Fields = []string{model.ClientIPField}
	}
	return f
}

// TestClientIPFieldRegistration pins the §5.2 closed-field-set contract for
// the reserved client_ip attribute: usable with in_cidr (and any other
// closed-set operator) once explicitly registered in Flag.Fields — it is
// NOT implicitly registered like targetingKey — and rejected otherwise.
func TestClientIPFieldRegistration(t *testing.T) {
	if model.ClientIPField != "client_ip" {
		t.Fatalf("wire contract: ClientIPField = %q, want \"client_ip\"", model.ClientIPField)
	}

	if err := model.ValidateFlag(clientIPFlag(true), nil); err != nil {
		t.Fatalf("registered client_ip + in_cidr must validate: %v", err)
	}

	err := model.ValidateFlag(clientIPFlag(false), nil)
	if err == nil {
		t.Fatal("unregistered client_ip constraint must be rejected")
	}
	if !strings.Contains(err.Error(), `"client_ip" is not registered`) {
		t.Fatalf("unexpected rejection reason: %v", err)
	}

	// Invalid CIDR operand on a registered client_ip is still rejected.
	bad := clientIPFlag(true)
	bad.Targeting[0].Constraints[0].Value = "10.0.0.0/33"
	if err := model.ValidateFlag(bad, nil); err == nil {
		t.Fatal("invalid in_cidr operand must be rejected")
	}

	// Segments may reference client_ip when the FLAG registers it.
	seg := &model.Segment{
		SchemaVersion: model.SchemaVersion,
		Key:           "internal-ips",
		Constraints: []model.Constraint{
			{Field: model.ClientIPField, Op: model.OpInCIDR, Value: "192.168.0.0/16"},
		},
	}
	if err := model.ValidateSegment(seg, map[string]*model.Segment{"internal-ips": seg}); err != nil {
		t.Fatalf("standalone segment with client_ip: %v", err)
	}
	viaSeg := clientIPFlag(true)
	viaSeg.Targeting = []model.Rule{{Name: "seg", Segments: []string{"internal-ips"}, Variation: "candidate"}}
	if err := model.ValidateFlag(viaSeg, map[string]*model.Segment{"internal-ips": seg}); err != nil {
		t.Fatalf("flag registering client_ip + segment constraint: %v", err)
	}
	viaSegUnregistered := clientIPFlag(false)
	viaSegUnregistered.Targeting = viaSeg.Targeting
	if err := model.ValidateFlag(viaSegUnregistered, map[string]*model.Segment{"internal-ips": seg}); err == nil {
		t.Fatal("segment constraint on unregistered client_ip must be rejected by the flag allowlist")
	}
}

// TestClientIPBucketingUnitAllowed pins the deliberate policy: client_ip is
// a LEGAL bucketing unit (traffic-routing gray groups) and must NOT be
// blocked by validation — the "experiments should prefer stable IDs"
// caution lives in the docs (技术方案 §6) and pkg/model comments, not in a
// rejection.
func TestClientIPBucketingUnitAllowed(t *testing.T) {
	f := clientIPFlag(true)
	f.Bucketing = &model.Bucketing{
		Algorithm: bucketing.AlgorithmID, Unit: model.ClientIPField, Salt: "ip-gray",
	}
	f.Targeting = append(f.Targeting, model.Rule{
		Name:       "ramp",
		Percentage: map[string]float64{"control": 90, "candidate": 10},
	})
	if err := model.ValidateFlag(f, nil); err != nil {
		t.Fatalf("client_ip bucketing unit must be allowed (docs-warned, not blocked): %v", err)
	}

	// The unit must still be a REGISTERED field like any other unit.
	unregistered := clientIPFlag(false)
	unregistered.Fields = []string{"user_id"}
	unregistered.Bucketing = &model.Bucketing{
		Algorithm: bucketing.AlgorithmID, Unit: model.ClientIPField, Salt: "ip-gray",
	}
	unregistered.DefaultRule = &model.Rule{
		Percentage: map[string]float64{"control": 50, "candidate": 50},
	}
	err := model.ValidateFlag(unregistered, nil)
	if err == nil || !strings.Contains(err.Error(), "bucketing.unit") {
		t.Fatalf("unregistered bucketing unit client_ip must be rejected, got: %v", err)
	}
}
