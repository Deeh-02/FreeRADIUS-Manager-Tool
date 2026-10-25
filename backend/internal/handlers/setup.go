package handlers

import (
	"crypto/subtle"
	"database/sql"
	"net/http"
	"os"
	"strconv"

	"github.com/freeradius-manager/backend/internal/auth"
	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/gin-gonic/gin"
)

// SetupRequired reports whether the first-run wizard may run: setup_complete is not "true"
// AND no staff account exists. The second condition means a reset setup_complete flag can
// never reopen the wizard on a system that already has admins.
func SetupRequired(db *sql.DB) bool {
	var value string
	db.QueryRow(`SELECT value FROM system_settings WHERE key = 'setup_complete'`).Scan(&value)
	if value == "true" {
		return false
	}
	var admins int
	if err := db.QueryRow(`SELECT COUNT(*) FROM app_users`).Scan(&admins); err != nil {
		return false
	}
	return admins == 0
}

// SetupStatus reports whether the first-run wizard has been completed.
func (h *Handler) SetupStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"setup_required": SetupRequired(h.db.DB),
		"token_required": os.Getenv("SETUP_TOKEN") != "",
		"password_min":   auth.MinPasswordLength(),
		"version":        AppVersion,
	})
}

// SetupComplete processes the wizard form submission and creates the first super_admin.
// It only runs while SetupRequired is true (no staff account exists yet). If SETUP_TOKEN is
// set (setup.sh generates one and prints it), the request must carry it, so a stranger who
// reaches a fresh server first cannot claim it. The security values it stores take effect
// (they are read from system_settings, see internal/settings). The RADIUS shared secret is
// configured in .env (RADIUS_SECRET), not here.
func (h *Handler) SetupComplete(c *gin.Context) {
	if !SetupRequired(h.db.DB) {
		respondError(c, http.StatusForbidden, "setup has already been completed")
		return
	}

	var req struct {
		SetupToken   string `json:"setup_token"`
		Organization struct {
			Name     string `json:"name"      binding:"required,min=2,max=100"`
			Timezone string `json:"timezone"`
			LogoText string `json:"logo_text"`
		} `json:"organization" binding:"required"`
		Admin struct {
			Username string `json:"username"  binding:"required,min=3,max=50"`
			Email    string `json:"email"     binding:"required,email"`
			FullName string `json:"full_name" binding:"required"`
			Password string `json:"password"  binding:"required,min=8,max=128"`
		} `json:"admin" binding:"required"`
		RADIUS struct {
			DefaultSecret string `json:"default_secret"`
			MaxDevices    int    `json:"max_devices"`
		} `json:"radius"`
		Security struct {
			PasswordMinLength  int  `json:"password_min_length"`
			PasswordExpiryDays int  `json:"password_expiry_days"`
			SessionTimeout     int  `json:"session_timeout"`
			MFARequired        bool `json:"mfa_required"`
			BruteForceAttempts int  `json:"brute_force_attempts"`
		} `json:"security"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if want := os.Getenv("SETUP_TOKEN"); want != "" &&
		subtle.ConstantTimeCompare([]byte(want), []byte(req.SetupToken)) != 1 {
		respondError(c, http.StatusForbidden, "invalid setup token (printed by setup.sh, SETUP_TOKEN in .env)")
		return
	}

	// Apply safe defaults for optional numeric fields
	if req.Security.PasswordMinLength < 8 {
		req.Security.PasswordMinLength = 12
	}
	if req.Security.PasswordExpiryDays == 0 {
		req.Security.PasswordExpiryDays = 90
	}
	if req.Security.SessionTimeout == 0 {
		req.Security.SessionTimeout = 3600
	}
	if req.Security.BruteForceAttempts == 0 {
		req.Security.BruteForceAttempts = 5
	}
	if req.RADIUS.MaxDevices == 0 {
		req.RADIUS.MaxDevices = 20
	}
	if req.Organization.Timezone == "" {
		req.Organization.Timezone = "UTC"
	}

	// Same rules as every other staff password, with the length the wizard is saving.
	if err := auth.ValidatePasswordWithMin(req.Admin.Password, req.Security.PasswordMinLength); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	// Hash the admin password
	hash, err := auth.HashPassword(req.Admin.Password)
	if err != nil {
		respondHashError(c, err)
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "transaction failed")
		return
	}
	defer tx.Rollback()

	// Serialise concurrent submissions; the second one finds an admin and stops.
	if _, err = tx.Exec(`LOCK TABLE app_users IN EXCLUSIVE MODE`); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to lock users")
		return
	}
	var admins int
	tx.QueryRow(`SELECT COUNT(*) FROM app_users`).Scan(&admins)
	if admins > 0 {
		respondError(c, http.StatusForbidden, "setup has already been completed")
		return
	}
	if _, err = tx.Exec(`
		INSERT INTO app_users (username, password_hash, email, full_name, role, is_active)
		VALUES ($1, $2, $3, $4, 'super_admin', true)`,
		req.Admin.Username, hash, req.Admin.Email, req.Admin.FullName,
	); err != nil {
		if isUniqueViolation(err) {
			respondError(c, http.StatusConflict, "username already exists")
			return
		}
		h.log.WithError(err).Error("setup: create admin user failed")
		respondError(c, http.StatusInternalServerError, "failed to create admin user")
		return
	}

	// Upsert all settings
	upsert := func(key, value, desc string) {
		tx.Exec(`
			INSERT INTO system_settings (key, value, description)
			VALUES ($1, $2, $3)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
			key, value, desc,
		)
	}

	upsert("setup_complete", "true", "Initial setup wizard completed")
	upsert("org_name", req.Organization.Name, "Organisation display name")
	upsert("org_timezone", req.Organization.Timezone, "Organisation timezone")
	upsert("org_logo_text", req.Organization.LogoText, "Short logo/brand text")
	upsert("password_min_length", strconv.Itoa(req.Security.PasswordMinLength), "Minimum password length")
	upsert("password_expiry_days", strconv.Itoa(req.Security.PasswordExpiryDays), "Password expiry in days")
	upsert("session_timeout", strconv.Itoa(req.Security.SessionTimeout), "Session timeout in seconds")
	upsert("mfa_required", strconv.FormatBool(req.Security.MFARequired), "Require MFA for admin users")
	upsert("brute_force_attempts", strconv.Itoa(req.Security.BruteForceAttempts), "Failed attempts before lockout")
	upsert("max_device_limit", strconv.Itoa(req.RADIUS.MaxDevices), "Max devices per RADIUS user")

	if err = tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to save setup")
		return
	}

	settings.Invalidate()
	h.log.Infof("Setup completed — admin account '%s' created", req.Admin.Username)
	c.JSON(http.StatusOK, gin.H{
		"message":  "Setup completed successfully",
		"username": req.Admin.Username,
	})
}

