package handlers

import "time"

// SetSSEHeartbeatInterval overrides the SSE keepalive period for a test and
// returns a function that restores it.
func SetSSEHeartbeatInterval(d time.Duration) func() {
	prev := sseHeartbeatInterval
	sseHeartbeatInterval = d
	return func() { sseHeartbeatInterval = prev }
}
