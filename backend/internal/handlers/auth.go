package handlers

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/freeradius-manager/backend/internal/auth"
	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/freeradius-manager/backend/internal/models"
	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"
)

// Login authenticates an app user and issues JWT tokens.
// Protections, in order: blocked IP / GeoIP rules (loginGuard), per-account lockout after
// brute_force_attempts failures for brute_force_lockout minutes (Settings page), and the
// per-IP credential-stuffing window (CheckCredStuffing). A disabled account is only reported
// after the correct password, so the response does not reveal which usernames exist. Each
// login opens a new session; several devices can be signed in at once.
func (h *Handler) Login(c *gin.Context) {
	var req models.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if !h.loginGuard(c, req.Username) {
		return
	}

	// Fetch user
	var user models.AppUser
	err := h.db.QueryRow(`
		SELECT id, username, password_hash, email,
		       COALESCE(full_name, ''), role,
		       mfa_enabled, COALESCE(mfa_secret, ''), is_active, failed_attempts, locked_until
		FROM app_users WHERE username = $1`,
		req.Username,
	).Scan(
		&user.ID, &user.Username, &user.PasswordHash, &user.Email,
		&user.FullName, &user.Role, &user.MFAEnabled, &user.MFASecret,
		&user.IsActive, &user.FailedAttempts, &user.LockedUntil,
	)

	if err == sql.ErrNoRows {
		// Same cost as a real check, so response time does not reveal unknown usernames.
		auth.CheckPassword(dummyHash, req.Password)
		CheckCredStuffing(h, c.ClientIP(), req.Username)
		respondError(c, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		h.log.WithError(err).Error("database error on login")
		respondError(c, http.StatusInternalServerError, "internal server error")
		return
	}

	// Check lockout
	maxAttempts := getBruteForceAttempts()
	if user.LockedUntil != nil && time.Now().Before(*user.LockedUntil) {
		respondError(c, http.StatusTooManyRequests, "account temporarily locked due to too many failed attempts")
		return
	}
	// A lock that has run out starts a fresh count.
	if user.LockedUntil != nil {
		user.FailedAttempts = 0
	}

	// Verify password
	if !auth.CheckPassword(user.PasswordHash, req.Password) {
		newAttempts := user.FailedAttempts + 1
		var lockUntil interface{} = nil

		if newAttempts >= maxAttempts {
			lockDuration := time.Duration(getBruteForceLockout()) * time.Minute
			lockTime := time.Now().Add(lockDuration)
			lockUntil = lockTime
		}

		h.db.Exec(`
			UPDATE app_users SET failed_attempts = $1, locked_until = $2
			WHERE id = $3`,
			newAttempts, lockUntil, user.ID,
		)
		CheckCredStuffing(h, c.ClientIP(), req.Username)

		respondError(c, http.StatusUnauthorized, "invalid credentials")
		return
	}

	if !user.IsActive {
		respondError(c, http.StatusForbidden, "account is disabled")
		return
	}

	// MFA verification
	if user.MFAEnabled {
		if req.MFACode == "" {
			c.JSON(http.StatusOK, gin.H{"mfa_required": true})
			return
		}
		if !totp.Validate(req.MFACode, user.MFASecret) {
			// A wrong code counts like a wrong password, so the 6 digits cannot be brute-forced.
			newAttempts := user.FailedAttempts + 1
			var lockUntil interface{}
			if newAttempts >= maxAttempts {
				lockUntil = time.Now().Add(time.Duration(getBruteForceLockout()) * time.Minute)
			}
			h.db.Exec(`UPDATE app_users SET failed_attempts = $1, locked_until = $2 WHERE id = $3`,
				newAttempts, lockUntil, user.ID)
			CheckCredStuffing(h, c.ClientIP(), req.Username)
			respondError(c, http.StatusUnauthorized, "invalid MFA code")
			return
		}
	}

	// Reset failed attempts on success
	h.db.Exec(`
		UPDATE app_users SET failed_attempts = 0, locked_until = NULL, last_login = NOW()
		WHERE id = $1`, user.ID,
	)

	sessionID, err := auth.GenerateSessionID()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to create session")
		return
	}

	// Issue tokens
	accessToken, err := auth.GenerateAccessToken(user.ID, user.Username, user.Role, sessionID)
	if err != nil {
		h.log.WithError(err).Error("failed to generate access token")
		respondError(c, http.StatusInternalServerError, "failed to generate token")
		return
	}

	refreshToken, refreshHash, err := auth.GenerateRefreshToken()
	if err != nil {
		h.log.WithError(err).Error("failed to generate refresh token")
		respondError(c, http.StatusInternalServerError, "failed to generate token")
		return
	}

	expiry := time.Now().Add(auth.RefreshExpiry())
	if _, err := h.db.Exec(`
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at, session_id, client_ip, user_agent, last_activity)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		user.ID, refreshHash, expiry, sessionID, c.ClientIP(), c.GetHeader("User-Agent"),
	); err != nil {
		h.log.WithError(err).Error("store session")
		respondError(c, http.StatusInternalServerError, "failed to create session")
		return
	}

	user.PasswordHash = ""
	user.MFASecret = ""

	c.JSON(http.StatusOK, models.TokenResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    int(auth.AccessExpiry().Seconds()),
		User:         &user,
	})
}

// RefreshToken issues a new access token using a valid refresh token.
func (h *Handler) RefreshToken(c *gin.Context) {
	var req models.RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "refresh_token is required")
		return
	}

	hash := auth.HashRefreshToken(req.RefreshToken)

	var userID int
	var role, username, sessionID string
	err := h.db.QueryRow(`
		SELECT rt.user_id, au.username, au.role, rt.session_id
		FROM refresh_tokens rt
		JOIN app_users au ON au.id = rt.user_id
		WHERE rt.token_hash = $1
		  AND rt.revoked = FALSE
		  AND rt.expires_at > NOW()
		  AND rt.last_activity > $2
		  AND au.is_active = TRUE`,
		hash, time.Now().Add(-auth.SessionTimeout()),
	).Scan(&userID, &username, &role, &sessionID)

	if err == sql.ErrNoRows {
		respondError(c, http.StatusUnauthorized, "invalid or expired refresh token")
		return
	}
	if err != nil {
		h.log.WithError(err).Error("db error on refresh")
		respondError(c, http.StatusInternalServerError, "internal server error")
		return
	}

	// Rotate refresh token (same session). The old token is revoked here but if the client
	// loses the response the retry fails with 401; a stolen-and-replayed old token is simply
	// rejected (it is not treated as a theft signal that revokes the whole session).
	h.db.Exec(`UPDATE refresh_tokens SET revoked = TRUE WHERE token_hash = $1`, hash)

	newAccessToken, err := auth.GenerateAccessToken(userID, username, role, sessionID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to generate token")
		return
	}

	newRefreshToken, newHash, err := auth.GenerateRefreshToken()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to generate token")
		return
	}

	expiry := time.Now().Add(auth.RefreshExpiry())
	h.db.Exec(`
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at, session_id, client_ip, user_agent, last_activity)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		userID, newHash, expiry, sessionID, c.ClientIP(), c.GetHeader("User-Agent"),
	)

	c.JSON(http.StatusOK, gin.H{
		"access_token":  newAccessToken,
		"refresh_token": newRefreshToken,
		"expires_in":    int(auth.AccessExpiry().Seconds()),
	})
}

// Logout revokes only the current session (other devices stay signed in).
func (h *Handler) Logout(c *gin.Context) {
	claims, _ := middleware.GetClaims(c)
	auth.RevokeSession(h.db.DB, claims.UserID, claims.SessionID)
	c.JSON(http.StatusOK, gin.H{"message": "logged out successfully"})
}

// GetProfile returns the authenticated user's profile.
func (h *Handler) GetProfile(c *gin.Context) {
	claims, _ := middleware.GetClaims(c)

	var user models.AppUser
	err := h.db.QueryRow(`
		SELECT id, username, email, COALESCE(full_name, ''), role,
		       mfa_enabled, is_active, last_login, created_at, updated_at
		FROM app_users WHERE id = $1`,
		claims.UserID,
	).Scan(
		&user.ID, &user.Username, &user.Email, &user.FullName, &user.Role,
		&user.MFAEnabled, &user.IsActive, &user.LastLogin, &user.CreatedAt, &user.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		respondError(c, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		h.log.WithError(err).Error("failed to fetch profile")
		respondError(c, http.StatusInternalServerError, "failed to fetch profile")
		return
	}

	c.JSON(http.StatusOK, user)
}

// UpdateProfile updates the authenticated user's email and full name.
func (h *Handler) UpdateProfile(c *gin.Context) {
	claims, _ := middleware.GetClaims(c)

	var req models.UpdateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	_, err := h.db.Exec(`
		UPDATE app_users
		SET email = $1, full_name = $2, updated_at = NOW()
		WHERE id = $3`,
		req.Email, req.FullName, claims.UserID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			respondError(c, http.StatusConflict, "email is already in use")
			return
		}
		h.log.WithError(err).Error("failed to update profile")
		respondError(c, http.StatusInternalServerError, "failed to update profile")
		return
	}

	var user models.AppUser
	h.db.QueryRow(`
		SELECT id, username, email, COALESCE(full_name, ''), role,
		       mfa_enabled, is_active, last_login, created_at, updated_at
		FROM app_users WHERE id = $1`,
		claims.UserID,
	).Scan(
		&user.ID, &user.Username, &user.Email, &user.FullName, &user.Role,
		&user.MFAEnabled, &user.IsActive, &user.LastLogin, &user.CreatedAt, &user.UpdatedAt,
	)

	c.JSON(http.StatusOK, gin.H{
		"message": "profile updated successfully",
		"user":    user,
	})
}

// ChangePassword changes the current user's password.
func (h *Handler) ChangePassword(c *gin.Context) {
	claims, _ := middleware.GetClaims(c)

	var req models.ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	if err := auth.ValidatePasswordComplexity(req.NewPassword); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	var currentHash string
	h.db.QueryRow(`SELECT password_hash FROM app_users WHERE id = $1`, claims.UserID).Scan(&currentHash)

	if !auth.CheckPassword(currentHash, req.CurrentPassword) {
		respondError(c, http.StatusUnauthorized, "current password is incorrect")
		return
	}

	newHash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to hash password")
		return
	}

	if _, err := h.db.Exec(`UPDATE app_users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
		newHash, claims.UserID); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to change password")
		return
	}

	// Force re-login everywhere.
	auth.RevokeAllUserSessions(h.db.DB, claims.UserID)

	c.JSON(http.StatusOK, gin.H{"message": "password changed successfully; sign in again"})
}

// MFASetup generates and returns a TOTP secret and QR code URI. It is refused while MFA is
// enabled (a super admin can reset it on the Admin Users page), so a stolen session cannot
// replace the second factor. The secret is stored unencrypted in app_users.mfa_secret.
// The "mfa_required" setting is enforced by middleware.RequireAuth.
func (h *Handler) MFASetup(c *gin.Context) {
	claims, _ := middleware.GetClaims(c)
	var enabled bool
	h.db.QueryRow(`SELECT mfa_enabled FROM app_users WHERE id = $1`, claims.UserID).Scan(&enabled)
	if enabled {
		respondError(c, http.StatusConflict, "MFA is already enabled; ask a super admin to reset it first")
		return
	}

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "FreeRADIUS Manager",
		AccountName: claims.Username,
	})
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to generate MFA secret")
		return
	}

	h.db.Exec(`UPDATE app_users SET mfa_secret = $1 WHERE id = $2`, key.Secret(), claims.UserID)

	c.JSON(http.StatusOK, gin.H{
		"secret":   key.Secret(),
		"otpauth": key.URL(),
		"message": "Scan the QR code or enter the secret in your authenticator app, then verify with /auth/mfa/verify",
	})
}

// MFAVerify enables MFA after user confirms the TOTP code.
func (h *Handler) MFAVerify(c *gin.Context) {
	claims, _ := middleware.GetClaims(c)

	var req struct {
		Code string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "code is required")
		return
	}

	var secret string
	h.db.QueryRow(`SELECT mfa_secret FROM app_users WHERE id = $1`, claims.UserID).Scan(&secret)

	if secret == "" {
		respondError(c, http.StatusBadRequest, "MFA setup not initiated, call /auth/mfa/setup first")
		return
	}

	if !totp.Validate(req.Code, secret) {
		respondError(c, http.StatusUnauthorized, "invalid MFA code")
		return
	}

	h.db.Exec(`UPDATE app_users SET mfa_enabled = TRUE WHERE id = $1`, claims.UserID)
	c.JSON(http.StatusOK, gin.H{"message": "MFA enabled successfully"})
}

// dummyHash is compared against when the username does not exist (bcrypt of a random string).
var dummyHash, _ = auth.HashPassword("unused-placeholder-password-for-timing")

// getBruteForceAttempts: Settings page "brute_force_attempts", else BRUTE_FORCE_ATTEMPTS, else 5.
func getBruteForceAttempts() int {
	if v := settings.Int("brute_force_attempts", "BRUTE_FORCE_ATTEMPTS", 5); v > 0 {
		return v
	}
	return 5
}

// getBruteForceLockout (minutes): "brute_force_lockout", else BRUTE_FORCE_LOCKOUT, else 15.
func getBruteForceLockout() int {
	if v := settings.Int("brute_force_lockout", "BRUTE_FORCE_LOCKOUT", 15); v > 0 {
		return v
	}
	return 15
}
