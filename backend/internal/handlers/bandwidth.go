package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// BandwidthProfile holds speed plan data.
type BandwidthProfile struct {
	ID                int       `json:"id"`
	Name              string    `json:"name"`
	Description       *string   `json:"description"`
	UploadKbps        int       `json:"upload_kbps"`
	DownloadKbps      int       `json:"download_kbps"`
	BurstUploadKbps   int       `json:"burst_upload_kbps"`
	BurstDownloadKbps int       `json:"burst_download_kbps"`
	MikrotikRateLimit *string   `json:"mikrotik_rate_limit"`
	IsActive          bool      `json:"is_active"`
	CreatedAt         time.Time `json:"created_at"`
}

// BandwidthProfileRequest is the create/update payload.
type BandwidthProfileRequest struct {
	Name              string `json:"name" binding:"required,min=2,max=100"`
	Description       string `json:"description"`
	UploadKbps        int    `json:"upload_kbps" binding:"required,min=1"`
	DownloadKbps      int    `json:"download_kbps" binding:"required,min=1"`
	BurstUploadKbps   int    `json:"burst_upload_kbps"`
	BurstDownloadKbps int    `json:"burst_download_kbps"`
	MikrotikRateLimit string `json:"mikrotik_rate_limit"`
	IsActive          *bool  `json:"is_active"`
}

// kbpsToMikrotik builds a Mikrotik-Rate-Limit value ("rx/tx" from the router's point of
// view, so upload/download as the customer sees them). Only the base rates are used:
// burst_upload_kbps / burst_download_kbps are stored but never put into the rate string
// unless an admin types a full mikrotik_rate_limit by hand. Other vendors need their own
// attributes; the WISPr bandwidth attributes written alongside are in bits per second.
// Rate-limit writes below are delete-then-insert (no ON CONFLICT), which is why they work
// even though radreply has no unique index.
func kbpsToMikrotik(upload, download int) string {
	unit := func(kbps int) string {
		if kbps >= 1000 && kbps%1000 == 0 {
			return fmt.Sprintf("%dM", kbps/1000)
		}
		return fmt.Sprintf("%dk", kbps)
	}
	return fmt.Sprintf("%s/%s", unit(upload), unit(download))
}

func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ListBandwidthProfiles returns all bandwidth speed plans.
func (h *Handler) ListBandwidthProfiles(c *gin.Context) {
	rows, err := h.db.Query(`
		SELECT id, name, description, upload_kbps, download_kbps,
		       burst_upload_kbps, burst_download_kbps, mikrotik_rate_limit, is_active, created_at
		FROM bandwidth_profiles
		ORDER BY download_kbps ASC`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch profiles")
		return
	}
	defer rows.Close()

	profiles := []BandwidthProfile{}
	for rows.Next() {
		var p BandwidthProfile
		rows.Scan(&p.ID, &p.Name, &p.Description, &p.UploadKbps, &p.DownloadKbps,
			&p.BurstUploadKbps, &p.BurstDownloadKbps, &p.MikrotikRateLimit, &p.IsActive, &p.CreatedAt)
		profiles = append(profiles, p)
	}
	c.JSON(http.StatusOK, gin.H{"data": profiles})
}

// CreateBandwidthProfile creates a new speed plan.
func (h *Handler) CreateBandwidthProfile(c *gin.Context) {
	var req BandwidthProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.MikrotikRateLimit == "" {
		req.MikrotikRateLimit = kbpsToMikrotik(req.UploadKbps, req.DownloadKbps)
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	var id int
	err := h.db.QueryRow(`
		INSERT INTO bandwidth_profiles (name, description, upload_kbps, download_kbps,
		    burst_upload_kbps, burst_download_kbps, mikrotik_rate_limit, is_active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		req.Name, nullableString(req.Description), req.UploadKbps, req.DownloadKbps,
		req.BurstUploadKbps, req.BurstDownloadKbps, req.MikrotikRateLimit, isActive,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			respondError(c, http.StatusConflict, "profile name already exists")
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to create profile")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id, "message": "bandwidth profile created"})
}

// UpdateBandwidthProfile edits a speed plan and re-applies it to all assigned users.
func (h *Handler) UpdateBandwidthProfile(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req BandwidthProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.MikrotikRateLimit == "" {
		req.MikrotikRateLimit = kbpsToMikrotik(req.UploadKbps, req.DownloadKbps)
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	_, err = h.db.Exec(`
		UPDATE bandwidth_profiles
		SET name = $1, description = $2, upload_kbps = $3, download_kbps = $4,
		    burst_upload_kbps = $5, burst_download_kbps = $6,
		    mikrotik_rate_limit = $7, is_active = $8, updated_at = NOW()
		WHERE id = $9`,
		req.Name, nullableString(req.Description), req.UploadKbps, req.DownloadKbps,
		req.BurstUploadKbps, req.BurstDownloadKbps, req.MikrotikRateLimit, isActive, id)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update profile")
		return
	}
	go h.reapplyBandwidthProfile(id)
	c.JSON(http.StatusOK, gin.H{"message": "profile updated and will be re-applied to all users"})
}

// DeleteBandwidthProfile removes a speed plan (unlinking it from users first).
func (h *Handler) DeleteBandwidthProfile(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	// The customers on it lose its speed attributes too, or the router would keep applying
	// a profile that no longer exists anywhere in the app.
	err = h.inTx(func(tx *sql.Tx) error {
		users, err := usersWithProfile(tx, id)
		if err != nil {
			return err
		}
		for _, u := range users {
			if err := clearBandwidthTx(tx, u.id, u.name); err != nil {
				return err
			}
		}
		res, err := tx.Exec(`DELETE FROM bandwidth_profiles WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
	if err == sql.ErrNoRows {
		respondError(c, http.StatusNotFound, "profile not found")
		return
	}
	if err != nil {
		h.log.WithError(err).Error("delete bandwidth profile")
		respondError(c, http.StatusInternalServerError, "failed to delete profile")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "profile deleted"})
}

type profileUser struct {
	id   int
	name string
}

// usersWithProfile lists (and locks) the customers a speed profile is applied to.
func usersWithProfile(tx *sql.Tx, profileID int) ([]profileUser, error) {
	rows, err := tx.Query(`SELECT id, username FROM radius_users WHERE bandwidth_profile_id = $1 FOR UPDATE`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []profileUser
	for rows.Next() {
		var u profileUser
		if err := rows.Scan(&u.id, &u.name); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

var errProfileNotFound = errors.New("bandwidth profile not found")

// ApplyBandwidthProfile assigns a speed plan to a RADIUS user and sets radreply attributes.
// The profile is checked before anything is removed, and all of it happens in one
// transaction: a wrong profile ID used to strip the customer's working limits and then say 404.
func (h *Handler) ApplyBandwidthProfile(c *gin.Context) {
	userID, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid user ID")
		return
	}
	var body struct {
		ProfileID *int `json:"profile_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	var msg string
	err = h.inTx(func(tx *sql.Tx) error {
		var username string
		if err := tx.QueryRow(`SELECT username FROM radius_users WHERE id = $1 FOR UPDATE`, userID).Scan(&username); err != nil {
			return err
		}
		if body.ProfileID == nil {
			msg = "bandwidth profile removed from user"
			return clearBandwidthTx(tx, userID, username)
		}
		var name string
		if err := tx.QueryRow(`SELECT name FROM bandwidth_profiles WHERE id = $1`, *body.ProfileID).Scan(&name); err == sql.ErrNoRows {
			return errProfileNotFound
		} else if err != nil {
			return err
		}
		msg = fmt.Sprintf("applied '%s' to user %s", name, username)
		return applyBandwidthProfileTx(tx, userID, username, *body.ProfileID)
	})
	switch {
	case err == sql.ErrNoRows:
		respondError(c, http.StatusNotFound, "user not found")
	case err == errProfileNotFound:
		respondError(c, http.StatusNotFound, err.Error())
	case err != nil:
		h.log.WithError(err).Error("apply bandwidth profile")
		respondError(c, http.StatusInternalServerError, "failed to apply profile")
	default:
		c.JSON(http.StatusOK, gin.H{"message": msg})
	}
}

// reapplyBandwidthProfile re-syncs radreply for every user assigned to a profile (after it
// changed). Each customer is locked and re-checked, so one moved to another profile in the
// meantime keeps the new one.
func (h *Handler) reapplyBandwidthProfile(profileID int) {
	rows, err := h.db.Query(`SELECT id, username FROM radius_users WHERE bandwidth_profile_id = $1`, profileID)
	if err != nil {
		h.log.WithError(err).Warnf("re-apply bandwidth profile %d", profileID)
		return
	}
	var users []profileUser
	for rows.Next() {
		var u profileUser
		if rows.Scan(&u.id, &u.name) == nil {
			users = append(users, u)
		}
	}
	rows.Close()
	for _, u := range users {
		err := h.inTx(func(tx *sql.Tx) error {
			var still bool
			if err := tx.QueryRow(`SELECT COALESCE(bandwidth_profile_id = $2, FALSE) FROM radius_users WHERE id = $1 FOR UPDATE`,
				u.id, profileID).Scan(&still); err != nil || !still {
				return nil // deleted or moved meanwhile
			}
			return applyBandwidthProfileTx(tx, u.id, u.name, profileID)
		})
		if err != nil {
			h.log.WithError(err).Warnf("re-apply bandwidth profile %d to %s", profileID, u.name)
		}
	}
}
