package handlers

import (
	"testing"

	"github.com/freeradius-manager/backend/internal/auth"
	"github.com/gin-gonic/gin"
)

// Locks and sessions are kept in TIMESTAMP columns (wall-clock time in the database's zone).
// Comparing them with Go's clock read them as UTC, so in Kenya a 15-minute lock lasted 3 h 15 min
// and a portal session 27 hours. The database decides now. Shows the bug only outside UTC,
// as on a Kenyan server: TZ=Africa/Nairobi PGTZ=Africa/Nairobi.
func TestLocksEndOnTimeOutsideUTC(t *testing.T) {
	db := resetTestDB(t)
	h := newTestHandler(t)
	mustExec(t, db, `DELETE FROM portal_sessions`)
	mustExec(t, db, `DELETE FROM cred_stuffing_blocks`)
	if code, resp := callHandler(h.CreateRadiusUser, nil, nil, map[string]interface{}{
		"username": "locky", "password": "right-password-1"}); code != 201 {
		t.Fatalf("create: %d %v", code, resp)
	}
	portalLogin := func() int {
		code, _ := callHandler(h.PortalLogin, nil, nil, map[string]interface{}{"username": "locky", "password": "right-password-1"})
		return code
	}

	// Customer portal lock: on until it ends, off a minute after.
	mustExec(t, db, `UPDATE radius_users SET portal_failed_attempts = 5, portal_locked_until = NOW() + INTERVAL '1 minute' WHERE username = 'locky'`)
	if code := portalLogin(); code != 429 {
		t.Errorf("portal login during the lock: %d, want 429", code)
	}
	mustExec(t, db, `UPDATE radius_users SET portal_locked_until = NOW() - INTERVAL '1 minute' WHERE username = 'locky'`)
	if code := portalLogin(); code != 200 {
		t.Errorf("portal login a minute after the lock ended: %d, want 200", code)
	}

	// Portal session: valid until it ends, refused a minute after.
	mustExec(t, db, `INSERT INTO portal_sessions (token, username, expires_at) VALUES
		('t-live', 'locky', NOW() + INTERVAL '1 minute'), ('t-old', 'locky', NOW() - INTERVAL '1 minute')`)
	if code := callWithPortalToken(h, "t-live"); code != 200 {
		t.Errorf("portal session with a minute left: %d, want 200", code)
	}
	if code := callWithPortalToken(h, "t-old"); code != 401 {
		t.Errorf("portal session that ended a minute ago: %d, want 401", code)
	}

	// Staff sign-in lock.
	hash, _ := auth.HashPassword("staff-password-1")
	mustExec(t, db, `UPDATE app_users SET password_hash = $1, failed_attempts = 5, locked_until = NOW() + INTERVAL '1 minute' WHERE username = 'staff'`, hash)
	staffLogin := func() int {
		code, _ := callHandler(h.Login, nil, nil, map[string]interface{}{"username": "staff", "password": "staff-password-1"})
		return code
	}
	if code := staffLogin(); code != 429 {
		t.Errorf("staff login during the lock: %d, want 429", code)
	}
	mustExec(t, db, `UPDATE app_users SET locked_until = NOW() - INTERVAL '1 minute' WHERE username = 'staff'`)
	if code := staffLogin(); code == 429 {
		t.Error("staff login a minute after the lock ended is still locked")
	}

	// A failed sign-in that locks sets a lock that is on now (by the same clock).
	mustExec(t, db, `UPDATE radius_users SET portal_failed_attempts = 4, portal_locked_until = NULL WHERE username = 'locky'`)
	callHandler(h.PortalLogin, nil, nil, map[string]interface{}{"username": "locky", "password": "wrong"})
	if n := scanInt(t, db, `SELECT COUNT(*) FROM radius_users WHERE username = 'locky'
		AND portal_locked_until BETWEEN NOW() + INTERVAL '14 minutes' AND NOW() + INTERVAL '16 minutes'`); n != 1 {
		t.Error("the portal lock set after 5 failures does not end 15 minutes from now")
	}

	// IP block for credential stuffing.
	mustExec(t, db, `INSERT INTO cred_stuffing_blocks (ip_address, fail_count, blocked_until, reason)
		VALUES ('198.51.100.7', 10, NOW() - INTERVAL '1 minute', 'test')`)
	if CheckCredStuffing(h, "198.51.100.7", "x") {
		t.Error("an IP block that ended a minute ago still blocks")
	}
	mustExec(t, db, `UPDATE cred_stuffing_blocks SET blocked_until = NOW() + INTERVAL '1 minute' WHERE ip_address = '198.51.100.7'`)
	if !CheckCredStuffing(h, "198.51.100.7", "x") {
		t.Error("an IP block with a minute left does not block")
	}
}

func callWithPortalToken(h *Handler, token string) int {
	code, _ := callHandler(func(c *gin.Context) {
		h.PortalAuthMiddleware()(c)
		if !c.IsAborted() {
			c.JSON(200, gin.H{})
		}
	}, nil, map[string]string{"X-Portal-Token": token}, nil)
	return code
}

// A customer reports their account stolen: staff reset the password or suspend it. Whoever
// is signed in to the portal with the old password is signed out.
func TestPasswordResetAndSuspendEndPortalSessions(t *testing.T) {
	db := resetTestDB(t)
	h := newTestHandler(t)
	mustExec(t, db, `DELETE FROM portal_sessions`)
	for _, u := range []string{"stolen", "suspendme"} {
		if code, resp := callHandler(h.CreateRadiusUser, nil, nil, map[string]interface{}{"username": u, "password": "old-password-1"}); code != 201 {
			t.Fatalf("create %s: %d %v", u, code, resp)
		}
		mustExec(t, db, `INSERT INTO portal_sessions (token, username, expires_at) VALUES ($1, $2, NOW() + INTERVAL '1 hour')`, "t-"+u, u)
	}
	id := func(u string) gin.Params {
		return gin.Params{{Key: "id", Value: scanString(t, db, `SELECT id::text FROM radius_users WHERE username = $1`, u)}}
	}

	if code, resp := callHandler(h.ResetRadiusUserPassword, id("stolen"), nil, map[string]interface{}{"new_password": "new-password-2"}); code != 200 {
		t.Fatalf("reset: %d %v", code, resp)
	}
	if code := callWithPortalToken(h, "t-stolen"); code != 401 {
		t.Errorf("portal session after a password reset: %d, want 401", code)
	}
	if code, resp := callHandler(h.SuspendRadiusUser, id("suspendme"), nil, nil); code != 200 {
		t.Fatalf("suspend: %d %v", code, resp)
	}
	if code := callWithPortalToken(h, "t-suspendme"); code != 401 {
		t.Errorf("portal session after suspension: %d, want 401", code)
	}
}
