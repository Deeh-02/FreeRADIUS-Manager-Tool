package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"time"

	"github.com/freeradius-manager/backend/internal/settings"
)

// A "session" is a login on one device. Its id is copied into the JWT and into every
// refresh_tokens row created for it (refresh rotates the row but keeps the id), so one
// session_id ties the short-lived access token to a revocable server-side record.

// GenerateSessionID creates a unique session identifier stored in JWT and refresh_tokens.
func GenerateSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SessionTimeout returns how long a session stays valid with no API activity before logout:
// Settings page "session_timeout" (seconds), else SESSION_TIMEOUT, else one hour.
func SessionTimeout() time.Duration {
	secs := settings.Int("session_timeout", "SESSION_TIMEOUT", 3600)
	if secs < 60 {
		secs = 3600
	}
	return time.Duration(secs) * time.Second
}

// RevokeAllUserSessions invalidates every active session for a user (password change,
// account disabled, MFA reset).
func RevokeAllUserSessions(db *sql.DB, userID int) {
	db.Exec(`UPDATE refresh_tokens SET revoked = TRUE WHERE user_id = $1 AND revoked = FALSE`, userID)
}

// RevokeSession invalidates refresh tokens for one login session.
func RevokeSession(db *sql.DB, userID int, sessionID string) {
	if sessionID == "" {
		return
	}
	db.Exec(`
		UPDATE refresh_tokens SET revoked = TRUE
		WHERE user_id = $1 AND session_id = $2 AND revoked = FALSE`,
		userID, sessionID)
}

// SessionState is the live account state behind a valid JWT.
type SessionState struct {
	Active     bool   // session unrevoked, not idle-expired, and the account is enabled
	Role       string // current role from app_users (a demotion applies immediately)
	MFAEnabled bool
}

// ValidateSession checks that the JWT session is still valid (not revoked or idle-expired)
// and that the account is still enabled, returning its current role.
func ValidateSession(db *sql.DB, userID int, sessionID string) (SessionState, error) {
	var st SessionState
	if sessionID == "" {
		return st, nil
	}
	idleCutoff := time.Now().Add(-SessionTimeout())
	err := db.QueryRow(`
		SELECT au.role, au.mfa_enabled
		FROM refresh_tokens rt
		JOIN app_users au ON au.id = rt.user_id
		WHERE rt.user_id = $1
		  AND rt.session_id = $2
		  AND rt.revoked = FALSE
		  AND rt.expires_at > NOW()
		  AND rt.last_activity > $3
		  AND au.is_active = TRUE
		LIMIT 1`, userID, sessionID, idleCutoff).Scan(&st.Role, &st.MFAEnabled)
	if err == sql.ErrNoRows {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Active = true
	return st, nil
}

// TouchSession updates last_activity for the active session.
func TouchSession(db *sql.DB, userID int, sessionID string) {
	if sessionID == "" {
		return
	}
	db.Exec(`
		UPDATE refresh_tokens SET last_activity = NOW()
		WHERE user_id = $1 AND session_id = $2 AND revoked = FALSE`,
		userID, sessionID)
}
