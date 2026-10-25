package handlers

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/gin-gonic/gin"
)

// UserPlan holds a subscription plan definition.
type UserPlan struct {
	ID                 int       `json:"id"`
	Name               string    `json:"name"`
	Description        *string   `json:"description"`
	Price              float64   `json:"price"`
	Currency           string    `json:"currency"`
	DataLimitMB        *int64    `json:"data_limit_mb"`
	ValidityDays       int       `json:"validity_days"`
	BandwidthProfileID *int      `json:"bandwidth_profile_id"`
	BandwidthName      *string   `json:"bandwidth_name"`
	MaxDevices         int       `json:"max_devices"`
	IsActive           bool      `json:"is_active"`
	UserCount          int       `json:"user_count"`
	CreatedAt          time.Time `json:"created_at"`
}

// ListPlans returns all user plans.
func (h *Handler) ListPlans(c *gin.Context) {
	rows, err := h.db.Query(`
		SELECT up.id, up.name, up.description, up.price, up.currency,
		       up.data_limit_mb, up.validity_days, up.bandwidth_profile_id,
		       bp.name, up.max_devices, up.is_active, up.created_at,
		       COUNT(ru.id) as user_count
		FROM user_plans up
		LEFT JOIN bandwidth_profiles bp ON bp.id = up.bandwidth_profile_id
		LEFT JOIN radius_users ru ON ru.plan_id = up.id
		GROUP BY up.id, bp.name
		ORDER BY up.price ASC`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch plans")
		return
	}
	defer rows.Close()

	plans := []UserPlan{}
	for rows.Next() {
		var p UserPlan
		rows.Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Currency,
			&p.DataLimitMB, &p.ValidityDays, &p.BandwidthProfileID,
			&p.BandwidthName, &p.MaxDevices, &p.IsActive, &p.CreatedAt, &p.UserCount)
		plans = append(plans, p)
	}
	c.JSON(http.StatusOK, gin.H{"data": plans})
}

// CreatePlan creates a new user plan.
func (h *Handler) CreatePlan(c *gin.Context) {
	var req struct {
		Name               string  `json:"name" binding:"required,min=2,max=100"`
		Description        string  `json:"description"`
		Price              float64 `json:"price"`
		Currency           string  `json:"currency"`
		DataLimitMB        *int64  `json:"data_limit_mb"`
		ValidityDays       int     `json:"validity_days"`
		BandwidthProfileID *int    `json:"bandwidth_profile_id"`
		MaxDevices         int     `json:"max_devices"`
		IsActive           *bool   `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.ValidityDays == 0 {
		req.ValidityDays = 30
	}
	if req.MaxDevices == 0 {
		req.MaxDevices = 1
	}
	if req.Currency == "" {
		req.Currency = defaultCurrency()
	}
	if msg := planNumbersProblem(req.Price, req.ValidityDays, req.MaxDevices, req.DataLimitMB); msg != "" {
		respondError(c, http.StatusBadRequest, msg)
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	var id int
	err := h.db.QueryRow(`
		INSERT INTO user_plans (name,description,price,currency,data_limit_mb,validity_days,
		    bandwidth_profile_id,max_devices,is_active)
		VALUES ($1,$2,$3,UPPER($4),$5,$6,$7,$8,$9) RETURNING id`,
		req.Name, nullableString(req.Description), req.Price, req.Currency,
		req.DataLimitMB, req.ValidityDays, req.BandwidthProfileID, req.MaxDevices, isActive,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			respondError(c, http.StatusConflict, "plan name already exists")
			return
		}
		if isBadValue(err) {
			respondError(c, http.StatusBadRequest, "invalid value (currency over 10 letters, or an unknown bandwidth_profile_id)")
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to create plan")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id, "message": "plan created"})
}

// planNumbersProblem checks a plan's numbers against sense and the columns (price is
// NUMERIC(12,2)). A negative validity used to be accepted on create, which would give paying
// customers an expiry in the past.
func planNumbersProblem(price float64, validityDays, maxDevices int, dataLimitMB *int64) string {
	switch {
	case price < 0:
		return "price cannot be negative"
	case price >= 1e10 || math.IsNaN(price):
		return "price is too large"
	case validityDays < 1 || validityDays > 3660:
		return "validity_days must be between 1 and 3660"
	case maxDevices < 1 || maxDevices > 1000:
		return "max_devices must be between 1 and 1000"
	case dataLimitMB != nil && (*dataLimitMB < 0 || *dataLimitMB > 1<<40):
		return "data_limit_mb must be 0 (no cap) or a positive number of MB"
	}
	return ""
}

// UpdatePlan updates a plan's details.
func (h *Handler) UpdatePlan(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name               string  `json:"name"`
		Description        string  `json:"description"`
		Price              float64 `json:"price"`
		Currency           string  `json:"currency"`
		DataLimitMB        *int64  `json:"data_limit_mb"`
		ValidityDays       int     `json:"validity_days"`
		BandwidthProfileID *int    `json:"bandwidth_profile_id"`
		MaxDevices         int     `json:"max_devices"`
		IsActive           *bool   `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	if req.ValidityDays <= 0 {
		req.ValidityDays = 30
	}
	if req.MaxDevices <= 0 {
		req.MaxDevices = 1
	}
	if msg := planNumbersProblem(req.Price, req.ValidityDays, req.MaxDevices, req.DataLimitMB); msg != "" {
		respondError(c, http.StatusBadRequest, msg)
		return
	}
	if req.Currency == "" {
		req.Currency = defaultCurrency()
	}
	// The speed profile before the edit: customers who got it from this plan lose it if the
	// plan no longer sets one. Read under the row lock the UPDATE takes, in one transaction.
	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update plan")
		return
	}
	defer tx.Rollback()
	var prevProfile sql.NullInt64
	if err := tx.QueryRow(`SELECT bandwidth_profile_id FROM user_plans WHERE id = $1 FOR UPDATE`, id).Scan(&prevProfile); err == sql.ErrNoRows {
		respondError(c, http.StatusNotFound, "plan not found")
		return
	} else if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update plan")
		return
	}
	res, err := tx.Exec(`
		UPDATE user_plans SET name=COALESCE(NULLIF($1,''), name), description=$2, price=$3, currency=UPPER($4),
		    data_limit_mb=$5, validity_days=$6, bandwidth_profile_id=$7,
		    max_devices=$8, is_active=$9, updated_at=NOW()
		WHERE id=$10`,
		req.Name, nullableString(req.Description), req.Price, req.Currency,
		req.DataLimitMB, req.ValidityDays, req.BandwidthProfileID,
		req.MaxDevices, isActive, id)
	if err != nil {
		if isUniqueViolation(err) {
			respondError(c, http.StatusConflict, "plan name already exists")
			return
		}
		if isBadValue(err) {
			respondError(c, http.StatusBadRequest, "invalid value (currency over 10 letters, or an unknown bandwidth_profile_id)")
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to update plan")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(c, http.StatusNotFound, "plan not found")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update plan")
		return
	}
	// Existing customers on the plan get the new device limit, data cap and speed.
	go h.reapplyPlan(id, prevProfile)
	c.JSON(http.StatusOK, gin.H{"message": "plan updated; limits are being re-applied to its users"})
}

// DeletePlan removes a plan. It is refused while customers are on it (deactivate it
// instead); invoices keep their copy of the plan name.
func (h *Handler) DeletePlan(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var users int
	h.db.QueryRow(`SELECT COUNT(*) FROM radius_users WHERE plan_id=$1`, id).Scan(&users)
	if users > 0 {
		respondError(c, http.StatusConflict, fmt.Sprintf("%d customer(s) are on this plan; move them or deactivate the plan instead", users))
		return
	}
	if _, err := h.db.Exec(`DELETE FROM user_plans WHERE id=$1`, id); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete plan")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "plan deleted"})
}

// AssignPlan assigns a plan to a RADIUS user, applies its limits and bills it.
// Body: plan_id (null removes the plan link), start_now (give the period before payment),
// promo_code. Billing rules are in billing_core.go (assignPlanTx); everything happens in
// one transaction.
func (h *Handler) AssignPlan(c *gin.Context) {
	userID, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid user ID")
		return
	}
	var body struct {
		PlanID    *int   `json:"plan_id"`
		StartNow  bool   `json:"start_now"`
		PromoCode string `json:"promo_code"`
		// TrialDays overrides the system free-trial length for this customer (0 = no trial).
		TrialDays *int `json:"trial_days" binding:"omitempty,min=0,max=90"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	var username string
	if err := h.db.QueryRow(`SELECT username FROM radius_users WHERE id=$1`, userID).Scan(&username); err != nil {
		respondError(c, http.StatusNotFound, "user not found")
		return
	}

	if body.PlanID == nil {
		if _, err := h.db.Exec(`UPDATE radius_users SET plan_id=NULL, updated_at=NOW() WHERE id=$1`, userID); err != nil {
			respondError(c, http.StatusInternalServerError, "failed to update user")
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "plan removed from user (limits already applied are kept)"})
		return
	}

	claims, _ := middleware.GetClaims(c)
	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback()

	res, err := assignPlanTx(tx, userID, username, *body.PlanID, body.StartNow, body.PromoCode, &claims.UserID, body.TrialDays)
	if err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to save plan assignment")
		return
	}
	if res.InvoiceID != 0 {
		go h.dispatchWebhook("invoice.created", gin.H{"invoice_id": res.InvoiceID, "invoice_number": res.Invoice,
			"username": username, "amount": res.Amount})
	}
	c.JSON(http.StatusOK, gin.H{
		"message":        res.Message,
		"invoice_id":     res.InvoiceID,
		"invoice_number": res.Invoice,
		"account_expiry": res.Expiry,
	})
}

// reapplyPlan re-writes a plan's limits for every customer on it (after the plan changed).
// Each customer is locked and re-checked in their own transaction: one moved to another plan
// while this runs keeps the new plan instead of being put back on this one.
func (h *Handler) reapplyPlan(planID int, prevProfile sql.NullInt64) error {
	rows, err := h.db.Query(`SELECT id, username FROM radius_users WHERE plan_id = $1`, planID)
	if err != nil {
		return err
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
			var onPlan bool
			if err := tx.QueryRow(`SELECT COALESCE(plan_id = $2, FALSE) FROM radius_users WHERE id = $1 FOR UPDATE`,
				u.id, planID).Scan(&onPlan); err != nil || !onPlan {
				return nil // deleted or moved meanwhile
			}
			p, err := loadPlan(tx, planID) // the newest version, if it was edited again meanwhile
			if err != nil {
				return err
			}
			return writePlanLimits(tx, u.id, u.name, p, prevProfile)
		})
		if err != nil {
			h.log.WithError(err).Warnf("re-apply plan %d to %s", planID, u.name)
		}
	}
	return nil
}
