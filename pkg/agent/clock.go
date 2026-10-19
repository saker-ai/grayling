package agent

import "time"

// Heartbeat can detect clock drift but never extends a signed authorization.
func clockWithinBudget(serverMs int64) bool {
	now := time.Now().UnixMilli()
	return serverMs >= now-5000 && serverMs <= now+5000
}
