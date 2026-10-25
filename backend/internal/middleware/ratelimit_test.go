package middleware

import (
	"testing"
	"time"
)

// An IP that got blocked and never came back stayed in memory for good: cleanup skipped
// blocked entries and only a new request cleared the flag.
func TestRateLimiterForgetsEndedBlocks(t *testing.T) {
	rl := &RateLimiter{visitors: map[string]*visitor{}, limit: 1, window: time.Minute}
	old := time.Now().Add(-time.Hour)
	rl.visitors["198.51.100.1"] = &visitor{lastSeen: old, blocked: true, blockedUntil: old.Add(time.Minute)}
	rl.visitors["198.51.100.2"] = &visitor{lastSeen: old, blocked: true, blockedUntil: time.Now().Add(time.Hour)}
	rl.cleanup()
	if _, ok := rl.visitors["198.51.100.1"]; ok {
		t.Error("an IP whose block ended an hour ago is still kept")
	}
	if _, ok := rl.visitors["198.51.100.2"]; !ok {
		t.Error("an IP that is still blocked was forgotten")
	}
}
