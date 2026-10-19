package agent

import "testing"

func TestReleaseCapacityRequiresExplicitProfileAndCompleteZones(t *testing.T) {
	valid := ReleaseCapacity{Profile: "production-v1", RequiredZones: []string{"az-a", "az-b"}, MinimumPerBuildZone: 2, MaximumFailureBasisPoints: 1000}
	if !valid.valid() {
		t.Fatal("valid production profile rejected")
	}
	for _, change := range []func(*ReleaseCapacity){func(c *ReleaseCapacity) { c.Profile = "" }, func(c *ReleaseCapacity) { c.MinimumPerBuildZone = 1 }, func(c *ReleaseCapacity) { c.RequiredZones = nil }, func(c *ReleaseCapacity) { c.RequiredZones = []string{"az-b", "az-a"} }, func(c *ReleaseCapacity) { c.RequiredZones = []string{"az-a", "az-a"} }, func(c *ReleaseCapacity) { c.MaximumFailureBasisPoints = 10001 }} {
		c := valid
		change(&c)
		if c.valid() {
			t.Fatal("accepted ambiguous or weakened capacity intent", c)
		}
	}
	demo := valid
	demo.Profile = "demo-v1"
	demo.MinimumPerBuildZone = 1
	if !demo.valid() {
		t.Fatal("explicit demo profile rejected")
	}
}
