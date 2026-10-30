//go:build integration

package agent_test

import "testing"

// These Agent scenarios own separate scratch databases (where needed), ephemeral
// loopback listeners, temporary runtime directories and their workers. Overlap their
// real polling/renewal/expiry waits instead of accumulating them serially in
// the five-minute integration gate. Bound fixture pressure independently of
// the host CPU count (and go test's default -parallel).
var agentFixtureSlots = make(chan struct{}, 4)

func parallelAgentFixture(t *testing.T) {
	t.Helper()
	t.Parallel()
	agentFixtureSlots <- struct{}{}
	// Registered first, released last: client/worker/server/pool/database
	// cleanup must finish before the next fixture acquires this slot.
	t.Cleanup(func() { <-agentFixtureSlots })
}
