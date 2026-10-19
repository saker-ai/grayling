package agent

import (
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"time"
)

// commandDeadline is fixed when dispatch starts so authorization and resource
// work share a single budget. An envelope may shorten, never extend, it.
func commandDeadline(e *agentv1.Envelope, now time.Time) time.Time {
	deadline := now.Add(2 * time.Second)
	if e.DeadlineUnixMs != 0 {
		if requested := time.UnixMilli(e.DeadlineUnixMs); requested.Before(deadline) {
			deadline = requested
		}
	}
	return deadline
}
