package middleware

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/freeradius-manager/backend/internal/auth"
	"github.com/freeradius-manager/backend/internal/database"
	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

const (
	claimsKey = "claims"
)

// RequireAuth accepts either
//   - "Authorization: Bearer <JWT>" from a staff login, whose session must still be active, or
//   - "Authorization: ApiKey <key>" from the API Keys page (see apiKeyClaims).
//
// Tokens are never read from the query string: URLs end up in access logs and browser
// history. The live-stats stream and receipts are fetched with the header instead.
func RequireAuth(db *database.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization header required"})
			return
		}

		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
			return
		}

		if strings.EqualFold(parts[0], "ApiKey") {
			claims, readOnly, err := apiKeyClaims(db, strings.TrimSpace(parts[1]))
			if err == errKeyLookup {
				abortUnavailable(c)
				return
			}
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
				return
			}
			if readOnly && c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "this API key is read-only"})
				return
			}
			c.Set(claimsKey, claims)
			c.Next()
			return
		}

		if !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
			return
		}

		claims, err := auth.ValidateAccessToken(parts[1])
		if err != nil {
			switch err {
			case auth.ErrTokenExpired:
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token expired", "code": "TOKEN_EXPIRED"})
			default:
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			}
			return
		}

		// A valid JWT is not enough: the matching refresh_tokens row must still be
		// unrevoked and recently active, and the account still enabled. This is what makes
		// logout, idle timeout, disabling an account and changing its role take effect
		// immediately instead of when the access token expires.
		st, err := auth.ValidateSession(db.DB, claims.UserID, claims.SessionID)
		if err != nil {
			// The database could not be asked (restarting, network blip). That is not a
			// sign-out: answering 401 SESSION_REVOKED here made the UI log every member of
			// staff out at once.
			abortUnavailable(c)
			return
		}
		if !st.Active {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "session expired or signed out",
				"code":  "SESSION_REVOKED",
			})
			return
		}
		claims.Role = st.Role

		// "Require MFA" (Settings page): until the account has MFA enabled it may only use
		// the /auth endpoints, which include MFA setup.
		if !st.MFAEnabled && settings.Bool("mfa_required", "", false) &&
			!strings.HasPrefix(c.FullPath(), "/api/v1/auth/") {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "two-factor authentication must be set up before using the system",
				"code":  "MFA_SETUP_REQUIRED",
			})
			return
		}

		auth.TouchSession(db.DB, claims.UserID, claims.SessionID)

		c.Set(claimsKey, claims)
		c.Next()
	}
}

// apiKeyClaims validates an API key. Keys act with the identity of the admin who created
// them (who must still be active): permission "write" maps to the admin role, otherwise the
// key is a read-only operator. Super-admin-only routes are therefore never reachable by key.
func apiKeyClaims(db *database.DB, rawKey string) (*auth.Claims, bool, error) {
	sum := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(sum[:])

	var keyID, userID int
	var name, username string
	var perms []string
	var expiresAt *time.Time
	var lastUsed *time.Time
	err := db.QueryRow(`
		SELECT ak.id, ak.name, ak.permissions, ak.expires_at, ak.last_used, au.id, au.username
		FROM api_keys ak
		JOIN app_users au ON au.id = ak.created_by AND au.is_active = TRUE
		WHERE ak.key_hash = $1 AND ak.is_active = TRUE`, keyHash).
		Scan(&keyID, &name, pq.Array(&perms), &expiresAt, &lastUsed, &userID, &username)
	if err == sql.ErrNoRows {
		return nil, false, errInvalidKey
	}
	if err != nil {
		return nil, false, errKeyLookup
	}
	if expiresAt != nil && expiresAt.Before(time.Now()) {
		return nil, false, errKeyExpired
	}
	if lastUsed == nil || time.Since(*lastUsed) > time.Minute {
		db.Exec(`UPDATE api_keys SET last_used = NOW() WHERE id = $1`, keyID)
	}

	role, readOnly := auth.RoleOperator, true
	for _, p := range perms {
		if p == "write" || p == "admin" {
			role, readOnly = auth.RoleAdmin, false
		}
	}
	return &auth.Claims{UserID: userID, Username: username + " (API key: " + name + ")", Role: role}, readOnly, nil
}

// abortUnavailable answers 503 when the session or key cannot be checked because the
// database is unreachable, so clients retry instead of signing the user out.
func abortUnavailable(c *gin.Context) {
	c.Header("Retry-After", "5")
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"error": "the database is not reachable right now; try again in a moment",
		"code":  "DB_UNAVAILABLE",
	})
}

type authError string

func (e authError) Error() string { return string(e) }

const (
	errInvalidKey authError = "invalid API key"
	errKeyExpired authError = "API key expired"
	errKeyLookup  authError = "API key check failed"
)

// RequireRole returns a middleware that allows only users with specific roles.
func RequireRole(roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := GetClaims(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		if !auth.HasRole(claims.Role, roles...) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":          "insufficient permissions",
				"required_roles": roles,
			})
			return
		}

		c.Next()
	}
}

// GetClaims extracts JWT claims from the Gin context.
func GetClaims(c *gin.Context) (*auth.Claims, bool) {
	raw, exists := c.Get(claimsKey)
	if !exists {
		return nil, false
	}
	claims, ok := raw.(*auth.Claims)
	return claims, ok
}
