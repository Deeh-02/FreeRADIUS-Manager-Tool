package middleware

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type visitor struct {
	count        int
	windowStart  time.Time
	lastSeen     time.Time
	blocked      bool
	blockedUntil time.Time
}

// RateLimiter is a simple in-memory, fixed-window rate limiter per IP (RATE_LIMIT requests
// per minute, default 300; the admin UI makes several calls per page, and one office behind
// NAT shares an IP). Logins have their own, much stricter, brute-force limits.
// State lives in this process only (lost on restart, not shared between replicas).
// The key is c.ClientIP(). main.go limits trusted proxies to TRUSTED_PROXIES (default:
// loopback and the Docker ranges), so Gin walks X-Forwarded-For from the right and stops at
// the first address that is not a trusted proxy: a client-supplied X-Forwarded-For entry
// cannot choose the IP it is counted under.
type RateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	limit    int
	window   time.Duration
}

// NewRateLimiter creates a new rate limiter.
func NewRateLimiter() *RateLimiter {
	limit := 300
	if raw := os.Getenv("RATE_LIMIT"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}

	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		limit:    limit,
		window:   time.Minute,
	}

	// Periodically clean up old visitors
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		for range ticker.C {
			rl.cleanup()
		}
	}()

	return rl
}

// Middleware returns the Gin rate limiting handler.
func (rl *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()

		// Safaricom sends every M-Pesa confirmation from a handful of addresses; limiting them
		// per IP would refuse real payments on a busy day. These paths are guarded by the
		// long secret in the URL instead (handlers.callbackAllowed).
		if strings.HasPrefix(c.Request.URL.Path, "/api/v1/pay/") {
			c.Next()
			return
		}

		if rl.isLimited(ip) {
			c.Header("X-RateLimit-Limit", strconv.Itoa(rl.limit))
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded, please try again later",
			})
			return
		}

		c.Header("X-RateLimit-Limit", strconv.Itoa(rl.limit))
		c.Next()
	}
}

func (rl *RateLimiter) isLimited(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	v, ok := rl.visitors[ip]
	now := time.Now()

	if !ok {
		rl.visitors[ip] = &visitor{count: 1, windowStart: now, lastSeen: now}
		return false
	}

	// Once over the limit an IP is blocked for a full window.
	if v.blocked && now.Before(v.blockedUntil) {
		return true
	}
	if v.blocked {
		v.blocked = false
		v.count = 0
		v.windowStart = now
	}

	// The count restarts every window. (It used to restart only after a quiet minute, so a
	// UI that polled kept counting up until it was blocked.)
	if now.Sub(v.windowStart) >= rl.window {
		v.count = 0
		v.windowStart = now
	}

	v.count++
	v.lastSeen = now

	if v.count > rl.limit {
		v.blocked = true
		v.blockedUntil = now.Add(rl.window)
		return true
	}

	return false
}

func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-10 * time.Minute)
	for ip, v := range rl.visitors {
		// A block that has ended counts as gone too: the flag is only cleared by the next
		// request from that IP, so one that never came back stayed in memory for good.
		if v.lastSeen.Before(cutoff) && (!v.blocked || now.After(v.blockedUntil)) {
			delete(rl.visitors, ip)
		}
	}
}
