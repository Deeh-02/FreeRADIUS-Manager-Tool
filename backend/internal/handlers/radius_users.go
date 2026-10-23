package handlers

import (
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	strs "strings"
	"time"

	"github.com/freeradius-manager/backend/internal/auth"
	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/freeradius-manager/backend/internal/models"
	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/gin-gonic/gin"
)

// radiusUserColumns are selected by List/Get. Nullable text columns are COALESCEd: scanning
// NULL into a Go string fails, which used to drop such users from the list silently.
const radiusUserColumns = `
	ru.id, ru.username, COALESCE(ru.email,''), COALESCE(ru.full_name,''), COALESCE(ru.department,''),
	COALESCE(ru.phone,''), COALESCE(ru.notes,''),
	ru.status, ru.device_limit, ru.account_expiry, ru.grace_days, ru.password_expiry,
	COALESCE(ru.force_password_change,false), ru.plan_id, up.name, up.price, up.currency,
	(SELECT COUNT(*) FROM invoices i WHERE i.user_id = ru.id AND i.status IN ('pending','overdue')),
	ru.created_by, au.username, ru.created_at, ru.updated_at,
	(SELECT COUNT(*) FROM radacct ra WHERE ra.username = ru.username AND ra.acctstoptime IS NULL)`

func scanRadiusUser(scan func(...interface{}) error, u *models.RadiusUser) error {
	var createdBy sql.NullString
	err := scan(&u.ID, &u.Username, &u.Email, &u.FullName, &u.Department, &u.Phone, &u.Notes,
		&u.Status, &u.DeviceLimit, &u.AccountExpiry, &u.GraceDays, &u.PasswordExpiry,
		&u.ForcePasswordChange, &u.PlanID, &u.PlanName, &u.PlanPrice, &u.PlanCurrency, &u.OpenInvoices,
		&u.CreatedBy, &createdBy, &u.CreatedAt, &u.UpdatedAt, &u.ActiveSessions)
	u.CreatedByUsername = createdBy.String
	return err
}

// ListRadiusUsers returns paginated RADIUS users with optional filters.
func (h *Handler) ListRadiusUsers(c *gin.Context) {
	offset, limit := paginationParams(c)
	search := c.Query("search")
	status := c.Query("status")
	department := c.Query("department")

	where := "WHERE 1=1"
	filterArgs := []interface{}{}
	argN := 1

	if search != "" {
		where += fmt.Sprintf(
			` AND (ru.username ILIKE $%d OR ru.email ILIKE $%d OR ru.full_name ILIKE $%d OR ru.phone ILIKE $%d)`,
			argN, argN, argN, argN,
		)
		filterArgs = append(filterArgs, "%"+search+"%")
		argN++
	}
	if status != "" {
		where += fmt.Sprintf(` AND ru.status = $%d`, argN)
		filterArgs = append(filterArgs, status)
		argN++
	}
	if department != "" {
		where += fmt.Sprintf(` AND ru.department ILIKE $%d`, argN)
		filterArgs = append(filterArgs, "%"+department+"%")
		argN++
	}

	var total int
	h.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM radius_users ru %s`, where), filterArgs...).Scan(&total)

	dataArgs := append(filterArgs, limit, offset)
	rows, err := h.db.Query(fmt.Sprintf(`
		SELECT %s
		FROM radius_users ru
		LEFT JOIN app_users au ON au.id = ru.created_by
		LEFT JOIN user_plans up ON up.id = ru.plan_id
		%s
		ORDER BY ru.created_at DESC
		LIMIT $%d OFFSET $%d`, radiusUserColumns, where, argN, argN+1), dataArgs...)
	if err != nil {
		h.log.WithError(err).Error("list radius users query failed")
		respondError(c, http.StatusInternalServerError, "failed to fetch users")
		return
	}
	defer rows.Close()

	users := []models.RadiusUser{}
	for rows.Next() {
		var u models.RadiusUser
		if err := scanRadiusUser(rows.Scan, &u); err != nil {
			h.log.WithError(err).Warn("scan radius user row")
			continue
		}
		users = append(users, u)
	}

	c.JSON(http.StatusOK, gin.H{
		"data":  users,
		"total": total,
	})
}

// GetRadiusUser returns a single RADIUS user by ID.
func (h *Handler) GetRadiusUser(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}

	var u models.RadiusUser
	err = scanRadiusUser(h.db.QueryRow(fmt.Sprintf(`
		SELECT %s
		FROM radius_users ru
		LEFT JOIN app_users au ON au.id = ru.created_by
		LEFT JOIN user_plans up ON up.id = ru.plan_id
		WHERE ru.id = $1`, radiusUserColumns), id).Scan, &u)

	if err == sql.ErrNoRows {
		respondError(c, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		h.log.WithError(err).Error("get radius user")
		respondError(c, http.StatusInternalServerError, "database error")
		return
	}
	c.JSON(http.StatusOK, u)
}

func validEmail(s string) bool {
	if s == "" {
		return true
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

func parseDateParam(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, fmt.Errorf("date must be YYYY-MM-DD")
	}
	return &t, nil
}

// CreateRadiusUser creates a new RADIUS network-access user, optionally on a plan (plan_id,
// start_now, promo_code as in AssignPlan).
func (h *Handler) CreateRadiusUser(c *gin.Context) {
	var req models.CreateRadiusUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.Username = strs.TrimSpace(req.Username)
	if req.DeviceLimit == 0 {
		req.DeviceLimit = 1
	}
	if max := settings.Int("max_device_limit", "", 500); max > 0 && req.DeviceLimit > max {
		respondError(c, http.StatusBadRequest, fmt.Sprintf("device limit cannot exceed %d (Settings → max_device_limit)", max))
		return
	}
	expiry, err := parseDateParam(derefStr(req.AccountExpiry))
	if err != nil {
		respondError(c, http.StatusBadRequest, "account_expiry: "+err.Error())
		return
	}

	hashedPw, err := auth.HashPassword(req.Password)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to hash password")
		return
	}
	claims, _ := middleware.GetClaims(c)
	passwordExpiry := time.Now().AddDate(0, 0, settings.Int("password_expiry_days", "", 90)).Format("2006-01-02")

	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback()

	var newID int
	err = tx.QueryRow(`
		INSERT INTO radius_users (username, password, email, full_name, department, phone, notes,
		    device_limit, account_expiry, password_expiry, force_password_change, created_by, grace_days)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id`,
		req.Username, hashedPw, nullableString(req.Email), nullableString(req.FullName),
		nullableString(req.Department), nullableString(req.Phone), nullableString(req.Notes),
		req.DeviceLimit, expiry, passwordExpiry, req.ForcePasswordChange, claims.UserID, req.GraceDays,
	).Scan(&newID)
	if err != nil {
		if isUniqueViolation(err) {
			respondError(c, http.StatusConflict, "username already exists")
			return
		}
		h.log.WithError(err).Error("create radius user failed")
		respondError(c, http.StatusInternalServerError, "failed to create user")
		return
	}

	// Cleartext-Password is what FreeRADIUS authenticates against (PAP/CHAP/MS-CHAP need the
	// plain password). The bcrypt hash in radius_users.password only serves the self-service
	// portal and password history, so protect read access to radcheck accordingly.
	if err := setCheck(tx, req.Username, "Cleartext-Password", ":=", req.Password); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to configure RADIUS attributes")
		return
	}
	if err := syncAccess(tx, req.Username); err != nil {
		h.log.WithError(err).Error("sync access")
		respondError(c, http.StatusInternalServerError, "failed to configure RADIUS attributes")
		return
	}

	msg := "RADIUS user created successfully"
	var invoiceID int
	if req.PlanID != nil {
		res, err := assignPlanTx(tx, newID, req.Username, *req.PlanID, req.StartNow, req.PromoCode, &claims.UserID, req.TrialDays)
		if err != nil {
			respondError(c, http.StatusBadRequest, "plan: "+err.Error())
			return
		}
		msg, invoiceID = res.Message, res.InvoiceID
	}

	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to save user")
		return
	}
	go h.dispatchWebhook("user.created", gin.H{"id": newID, "username": req.Username, "plan_id": req.PlanID})
	c.JSON(http.StatusCreated, gin.H{"id": newID, "message": msg, "invoice_id": invoiceID})
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// UpdateRadiusUser updates a RADIUS user's non-password fields. Changing the expiry or
// device limit is written to radcheck in the same transaction; moving the expiry into the
// future re-activates an expired account.
func (h *Handler) UpdateRadiusUser(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}

	var req models.UpdateRadiusUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.Email != nil && !validEmail(strs.TrimSpace(*req.Email)) {
		respondError(c, http.StatusBadRequest, "invalid email address")
		return
	}
	if req.DeviceLimit != nil {
		if max := settings.Int("max_device_limit", "", 500); max > 0 && *req.DeviceLimit > max {
			respondError(c, http.StatusBadRequest, fmt.Sprintf("device limit cannot exceed %d", max))
			return
		}
	}

	var username string
	if err := h.db.QueryRow(`SELECT username FROM radius_users WHERE id = $1`, id).Scan(&username); err != nil {
		if err == sql.ErrNoRows {
			respondError(c, http.StatusNotFound, "user not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "database error")
		return
	}

	setExpiry, clearExpiry := false, false
	var expiry *time.Time
	if req.AccountExpiry != nil {
		if *req.AccountExpiry == "" {
			clearExpiry = true
		} else if expiry, err = parseDateParam(*req.AccountExpiry); err != nil {
			respondError(c, http.StatusBadRequest, "account_expiry: "+err.Error())
			return
		} else {
			setExpiry = true
		}
	}

	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback()

	text := func(p *string) interface{} {
		if p == nil {
			return nil
		}
		return strs.TrimSpace(*p)
	}
	_, err = tx.Exec(`
		UPDATE radius_users
		SET email                 = CASE WHEN $1::text IS NULL THEN email ELSE NULLIF($1, '') END,
		    full_name             = CASE WHEN $2::text IS NULL THEN full_name ELSE NULLIF($2, '') END,
		    department            = CASE WHEN $3::text IS NULL THEN department ELSE NULLIF($3, '') END,
		    phone                 = CASE WHEN $4::text IS NULL THEN phone ELSE NULLIF($4, '') END,
		    notes                 = CASE WHEN $5::text IS NULL THEN notes ELSE NULLIF($5, '') END,
		    device_limit          = COALESCE($6, device_limit),
		    account_expiry        = CASE WHEN $7 THEN NULL WHEN $8 THEN $9::date ELSE account_expiry END,
		    force_password_change = COALESCE($10, force_password_change),
		    grace_days            = CASE WHEN $12 THEN NULL WHEN $13::int IS NOT NULL THEN $13::int ELSE grace_days END,
		    updated_at            = NOW()
		WHERE id = $11`,
		text(req.Email), text(req.FullName), text(req.Department), text(req.Phone), text(req.Notes),
		req.DeviceLimit, clearExpiry, setExpiry, expiry, req.ForcePasswordChange, id,
		req.ClearGraceDays, req.GraceDays,
	)
	if err != nil {
		h.log.WithError(err).Error("update radius user")
		respondError(c, http.StatusInternalServerError, "failed to update user")
		return
	}
	// An expiry moved into the future (or cleared) lifts an automatic expiry block; a manual
	// suspension stays until Activate is pressed. Changing only the grace days does the same
	// for a customer who has paid before (a never-paid customer is not let in by a longer
	// grace), and a shorter grace blocks a customer who is now past it.
	graceChanged := req.ClearGraceDays || req.GraceDays != nil
	expiryEdited := setExpiry || clearExpiry
	becameExpired := false
	if expiryEdited || graceChanged {
		if _, err := tx.Exec(`UPDATE radius_users SET status = 'active'
			WHERE id = $1 AND status = 'expired'
			  AND (account_expiry IS NULL OR account_expiry + COALESCE(grace_days, $2::int) >= CURRENT_DATE)
			  AND ($3 OR trial_started IS NOT NULL OR EXISTS (SELECT 1 FROM invoices i WHERE i.user_id = radius_users.id AND i.service_applied)
			         OR EXISTS (SELECT 1 FROM user_plans up WHERE up.id = radius_users.plan_id AND up.price <= 0))`,
			id, graceDays(), expiryEdited); err != nil {
			respondError(c, http.StatusInternalServerError, "failed to update status")
			return
		}
		res, err := tx.Exec(`UPDATE radius_users SET status = 'expired'
			WHERE id = $1 AND status = 'active' AND account_expiry IS NOT NULL
			  AND account_expiry + COALESCE(grace_days, $2::int) < CURRENT_DATE`, id, graceDays())
		if err != nil {
			respondError(c, http.StatusInternalServerError, "failed to update status")
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			becameExpired = true
		}
	}
	if err := syncAccess(tx, username); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update RADIUS attributes")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to save user")
		return
	}
	msg := "user updated successfully"
	if becameExpired {
		msg += disconnectUserSessions(h.db, username, false).stillOnlineNote()
	}
	c.JSON(http.StatusOK, gin.H{"message": msg})
}

// DeleteRadiusUser removes a RADIUS user and all associated RADIUS attributes. Invoices and
// payments keep the username for the record.
func (h *Handler) DeleteRadiusUser(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}

	var username string
	if err := h.db.QueryRow(`SELECT username FROM radius_users WHERE id = $1`, id).Scan(&username); err != nil {
		if err == sql.ErrNoRows {
			respondError(c, http.StatusNotFound, "user not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "database error")
		return
	}
	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "transaction failed")
		return
	}
	defer tx.Rollback()
	if err := deleteRadiusUserTx(tx, id, username); err != nil {
		var unsettled *unsettledInvoicesError
		if errors.As(err, &unsettled) {
			respondError(c, http.StatusConflict, unsettled.Error())
			return
		}
		h.log.WithError(err).Error("delete radius user")
		respondError(c, http.StatusInternalServerError, "failed to delete user")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete user")
		return
	}
	// Kick any live session only now that the delete has happened (a refused delete must leave
	// the customer online); otherwise the router keeps the user online. The rows are closed
	// either way: the customer no longer exists.
	kick := disconnectUserSessions(h.db, username, true)
	c.JSON(http.StatusOK, gin.H{"message": "user deleted successfully" + kick.stillOnlineNote()})
}

// ResetRadiusUserPassword resets a RADIUS user's password.
func (h *Handler) ResetRadiusUserPassword(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}

	var req models.ResetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	var username, oldHash string
	if err := h.db.QueryRow(`SELECT username, password FROM radius_users WHERE id = $1`, id).Scan(&username, &oldHash); err != nil {
		respondError(c, http.StatusNotFound, "user not found")
		return
	}

	// The new password may not be the current one or one of the last five.
	history := []string{oldHash}
	rows, err := h.db.Query(`
		SELECT password_hash FROM radius_user_password_history
		WHERE user_id = $1
		ORDER BY changed_at DESC LIMIT 5`, id)
	if err == nil {
		for rows.Next() {
			var hh string
			if rows.Scan(&hh) == nil {
				history = append(history, hh)
			}
		}
		rows.Close()
	}
	if err := auth.CheckPasswordHistory(req.NewPassword, history); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	newHash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to hash password")
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "transaction failed")
		return
	}
	defer tx.Rollback()
	days := settings.Int("password_expiry_days", "", 90)
	for _, step := range []struct {
		q    string
		args []interface{}
	}{
		{`UPDATE radius_users SET password = $1, password_expiry = CURRENT_DATE + $2::int, updated_at = NOW() WHERE id = $3`,
			[]interface{}{newHash, days, id}},
		{`INSERT INTO radius_user_password_history (user_id, password_hash) VALUES ($1, $2)`, []interface{}{id, oldHash}},
	} {
		if _, err := tx.Exec(step.q, step.args...); err != nil {
			respondError(c, http.StatusInternalServerError, "failed to reset password")
			return
		}
	}
	if err := setCheck(tx, username, "Cleartext-Password", ":=", req.NewPassword); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to reset password")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to reset password")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "password reset successfully"})
}

// SuspendRadiusUser blocks a user at the RADIUS server and disconnects their sessions.
func (h *Handler) SuspendRadiusUser(c *gin.Context) {
	h.setRadiusUserStatus(c, "suspended")
}

// ActivateRadiusUser lifts a suspension. A customer whose paid-until date has passed stays
// blocked (status expired) until they pay or their expiry is changed.
func (h *Handler) ActivateRadiusUser(c *gin.Context) {
	h.setRadiusUserStatus(c, "active")
}

func (h *Handler) setRadiusUserStatus(c *gin.Context, status string) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	username, newStatus, err := setUserStatus(h.db, id, status)
	if err == sql.ErrNoRows {
		respondError(c, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		h.log.WithError(err).Error("set user status")
		respondError(c, http.StatusInternalServerError, "failed to change status")
		return
	}
	msg := fmt.Sprintf("user %s", newStatus)
	if status == "suspended" {
		kick := disconnectUserSessions(h.db, username, false)
		if kick.Closed > 0 {
			msg += fmt.Sprintf("; %d session(s) disconnected", kick.Closed)
		}
		msg += kick.stillOnlineNote()
		go h.dispatchWebhook("user.suspended", gin.H{"id": id, "username": username})
	} else if newStatus == "expired" {
		msg = "suspension lifted, but the customer's paid-until date has passed: they stay blocked until they pay"
	}
	c.JSON(http.StatusOK, gin.H{"message": msg, "status": newStatus})
}

// setUserStatus applies suspend/activate and syncs radcheck in one transaction. Activating
// an account whose paid-until date (plus grace) has passed leaves it 'expired'.
func setUserStatus(db interface {
	Begin() (*sql.Tx, error)
}, id int, status string) (string, string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	var username, newStatus string
	err = tx.QueryRow(`
		UPDATE radius_users SET status = CASE
		    WHEN $1 = 'active' AND account_expiry IS NOT NULL AND account_expiry + COALESCE(grace_days, $3::int) < CURRENT_DATE THEN 'expired'
		    ELSE $1 END,
		  updated_at = NOW()
		WHERE id = $2 RETURNING username, status`, status, id, graceDays()).Scan(&username, &newStatus)
	if err != nil {
		return "", "", err
	}
	if err := syncAccess(tx, username); err != nil {
		return "", "", err
	}
	return username, newStatus, tx.Commit()
}

// DisconnectRadiusUser sends an RFC 5176 Disconnect-Request to the router for each of the
// user's open sessions. Rows are closed when the router acknowledges; with ?force=true they
// are closed regardless (for sessions that ended without a Stop packet).
func (h *Handler) DisconnectRadiusUser(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}

	var username string
	if err := h.db.QueryRow(`SELECT username FROM radius_users WHERE id = $1`, id).Scan(&username); err != nil {
		respondError(c, http.StatusNotFound, "user not found")
		return
	}
	force := c.Query("force") == "true"
	kick := disconnectUserSessions(h.db, username, force)
	c.JSON(http.StatusOK, gin.H{
		"message":      fmt.Sprintf("%d session(s) closed", kick.Closed) + kick.stillOnlineNote(),
		"disconnected": kick.Closed,
		"unconfirmed":  kick.Unconfirmed,
		"details":      kick.Msgs,
	})
}

// RadiusUserSessions returns session history for a RADIUS user.
func (h *Handler) RadiusUserSessions(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}

	var username string
	if err := h.db.QueryRow(`SELECT username FROM radius_users WHERE id = $1`, id).Scan(&username); err != nil {
		respondError(c, http.StatusNotFound, "user not found")
		return
	}

	rows, err := h.db.Query(`
		SELECT acctsessionid, username, host(nasipaddress), nasportid,
		       host(framedipaddress), callingstationid, calledstationid,
		       acctstarttime, COALESCE(acctsessiontime,0), COALESCE(acctinputoctets,0), COALESCE(acctoutputoctets,0),
		       acctstoptime, acctterminatecause
		FROM radacct
		WHERE username = $1
		ORDER BY acctstarttime DESC
		LIMIT 50`, username)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch sessions")
		return
	}
	defer rows.Close()

	type Session struct {
		SessionID      string     `json:"session_id"`
		Username       string     `json:"username"`
		NASIPAddress   string     `json:"nas_ip"`
		NASPortID      *string    `json:"nas_port"`
		FramedIP       *string    `json:"framed_ip"`
		CallingStation string     `json:"calling_station"`
		CalledStation  string     `json:"called_station"`
		StartTime      *time.Time `json:"start_time"`
		Duration       int64      `json:"duration_seconds"`
		InputBytes     int64      `json:"input_bytes"`
		OutputBytes    int64      `json:"output_bytes"`
		StopTime       *time.Time `json:"stop_time"`
		TermCause      string     `json:"term_cause"`
		Active         bool       `json:"active"`
	}

	sessions := []Session{}
	for rows.Next() {
		var s Session
		if err := rows.Scan(
			&s.SessionID, &s.Username, &s.NASIPAddress, &s.NASPortID,
			&s.FramedIP, &s.CallingStation, &s.CalledStation,
			&s.StartTime, &s.Duration, &s.InputBytes, &s.OutputBytes,
			&s.StopTime, &s.TermCause,
		); err != nil {
			continue
		}
		s.Active = s.StopTime == nil
		sessions = append(sessions, s)
	}

	c.JSON(http.StatusOK, gin.H{"data": sessions, "username": username})
}

// ImportRadiusUsers handles bulk CSV import of RADIUS users.
//
// Required columns: username, password. Optional: email, full_name, department, phone,
// device_limit, expiry (YYYY-MM-DD), validity_days (used when expiry is empty; blank or 0
// means never expires), plan_name, data_limit_mb, notes (or description). With plan_name the
// plan's limits are applied; its period is NOT billed or started (set expiry for that).
// Each row is its own transaction, so one bad row does not stop the rest.
func (h *Handler) ImportRadiusUsers(c *gin.Context) {
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		respondError(c, http.StatusBadRequest, "CSV file required (field: file)")
		return
	}
	defer file.Close()

	claims, _ := middleware.GetClaims(c)

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		respondError(c, http.StatusBadRequest, "failed to read CSV header")
		return
	}

	colIdx := map[string]int{}
	for i, col := range header {
		colIdx[strs.ToLower(strs.TrimSpace(strs.TrimPrefix(col, "\ufeff")))] = i
	}
	for _, need := range []string{"username", "password"} {
		if _, ok := colIdx[need]; !ok {
			respondError(c, http.StatusBadRequest, fmt.Sprintf("CSV must have a '%s' column", need))
			return
		}
	}

	type RowResult struct {
		Row      int    `json:"row"`
		Username string `json:"username"`
		Status   string `json:"status"`
		Message  string `json:"message"`
	}

	results := []RowResult{}
	created, skipped, failed := 0, 0, 0
	rowNum := 1
	passwordExpiry := time.Now().AddDate(0, 0, settings.Int("password_expiry_days", "", 90)).Format("2006-01-02")

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		rowNum++
		if err != nil {
			results = append(results, RowResult{rowNum, "", "error", "parse error: " + err.Error()})
			failed++
			continue
		}
		if rowNum > 10001 {
			results = append(results, RowResult{rowNum, "", "error", "import stops at 10000 rows"})
			failed++
			break
		}

		get := func(col string) string {
			idx, ok := colIdx[col]
			if !ok || idx >= len(record) {
				return ""
			}
			return strs.TrimSpace(record[idx])
		}

		username := get("username")
		password := get("password")
		switch {
		case username == "" || password == "":
			results = append(results, RowResult{rowNum, username, "skip", "empty username or password"})
			skipped++
			continue
		case len(password) < 6:
			results = append(results, RowResult{rowNum, username, "skip", "password too short (min 6)"})
			skipped++
			continue
		case strs.ContainsAny(username, " '\"\\"):
			results = append(results, RowResult{rowNum, username, "skip", "username may not contain spaces or quotes"})
			skipped++
			continue
		case !validEmail(get("email")):
			results = append(results, RowResult{rowNum, username, "skip", "invalid email"})
			skipped++
			continue
		}

		deviceLimit := 1
		if v := get("device_limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 500 {
				deviceLimit = n
			}
		}
		var expiry *time.Time
		if v := get("expiry"); v != "" {
			if expiry, err = parseDateParam(v); err != nil {
				results = append(results, RowResult{rowNum, username, "skip", "expiry must be YYYY-MM-DD"})
				skipped++
				continue
			}
		} else if v := get("validity_days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				t := time.Now().AddDate(0, 0, n)
				expiry = &t
			}
		}
		var plan *planRow
		if planName := get("plan_name"); planName != "" {
			var pid int
			if err := h.db.QueryRow(`SELECT id FROM user_plans WHERE LOWER(name)=LOWER($1)`, planName).Scan(&pid); err != nil {
				results = append(results, RowResult{rowNum, username, "skip", "unknown plan_name " + planName})
				skipped++
				continue
			}
			plan, _ = loadPlan(h.db, pid)
		}
		notes := get("notes")
		if notes == "" {
			notes = get("description")
		}

		hash, err := auth.HashPassword(password)
		if err != nil {
			results = append(results, RowResult{rowNum, username, "error", "hash failed"})
			failed++
			continue
		}

		rowErr := func() error {
			tx, err := h.db.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var newID int
			if err := tx.QueryRow(`
				INSERT INTO radius_users (username, email, password, full_name, department, phone, notes,
				    status, device_limit, account_expiry, password_expiry, created_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,'active',$8,$9,$10,$11) RETURNING id`,
				username, nullableString(get("email")), hash, nullableString(get("full_name")),
				nullableString(get("department")), nullableString(get("phone")), nullableString(notes),
				deviceLimit, expiry, passwordExpiry, claims.UserID).Scan(&newID); err != nil {
				return err
			}
			if err := setCheck(tx, username, "Cleartext-Password", ":=", password); err != nil {
				return err
			}
			if plan != nil {
				if err := applyPlanLimits(tx, newID, username, plan); err != nil {
					return err
				}
			}
			if v := get("data_limit_mb"); v != "" {
				if mb, err := strconv.ParseInt(v, 10, 64); err == nil && mb > 0 {
					if err := setCheck(tx, username, "Max-Octets", ":=", strconv.FormatInt(mb*1024*1024, 10)); err != nil {
						return err
					}
				}
			}
			if err := syncAccess(tx, username); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if rowErr != nil {
			if isUniqueViolation(rowErr) {
				results = append(results, RowResult{rowNum, username, "skip", "username already exists"})
				skipped++
				continue
			}
			results = append(results, RowResult{rowNum, username, "error", rowErr.Error()})
			failed++
			continue
		}
		results = append(results, RowResult{rowNum, username, "created", ""})
		created++
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Import complete: %d created, %d skipped, %d failed", created, skipped, failed),
		"created": created, "skipped": skipped, "failed": failed,
		"total": created + skipped + failed, "results": results,
	})
}

// ExportRadiusUsers exports all RADIUS users as a CSV file (no passwords). The columns match
// what ImportRadiusUsers reads, apart from the password.
func (h *Handler) ExportRadiusUsers(c *gin.Context) {
	rows, err := h.db.Query(`
		SELECT ru.username, COALESCE(ru.email,''), COALESCE(ru.full_name,''), COALESCE(ru.department,''),
		       COALESCE(ru.phone,''), ru.status, ru.device_limit, COALESCE(ru.account_expiry::text, ''),
		       COALESCE(up.name,''), ru.created_at
		FROM radius_users ru LEFT JOIN user_plans up ON up.id = ru.plan_id
		ORDER BY ru.username`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "export failed")
		return
	}
	defer rows.Close()

	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", "attachment; filename=radius_users_export.csv")

	writer := csv.NewWriter(c.Writer)
	writer.Write([]string{"username", "email", "full_name", "department", "phone", "status", "device_limit", "expiry", "plan_name", "created_at"})

	for rows.Next() {
		var username, email, fullName, department, phone, status, expiry, plan string
		var createdAt time.Time
		var deviceLimit int
		if rows.Scan(&username, &email, &fullName, &department, &phone, &status, &deviceLimit, &expiry, &plan, &createdAt) != nil {
			continue
		}
		writer.Write([]string{csvSafe(username), csvSafe(email), csvSafe(fullName), csvSafe(department), csvSafe(phone),
			status, strconv.Itoa(deviceLimit), expiry, csvSafe(plan), createdAt.Format(time.RFC3339)})
	}
	writer.Flush()
}

// csvSafe stops spreadsheet formula injection: a cell starting with = + - @ is prefixed
// with a quote so Excel/LibreOffice show it as text.
func csvSafe(s string) string {
	if s != "" && strs.ContainsAny(s[:1], "=+-@\t\r") {
		return "'" + s
	}
	return s
}
