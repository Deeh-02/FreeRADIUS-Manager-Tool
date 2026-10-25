package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────────
// Promotions / Discount Codes
// ─────────────────────────────────────────────────────────────────────────────
//
// A code is used by passing promo_code when assigning a plan or creating an invoice: the
// discount is taken off the invoice, the invoice records the code, and one use is counted
// atomically (billing_core.go redeemPromotion). Validate only previews the discount.
// Fixed-amount discounts are in the invoice's currency.

type Promotion struct {
	ID            int        `json:"id"`
	Code          string     `json:"code"`
	Description   *string    `json:"description"`
	DiscountType  string     `json:"discount_type"`
	DiscountValue float64    `json:"discount_value"`
	PlanID        *int       `json:"plan_id"`
	PlanName      *string    `json:"plan_name"`
	MaxUses       int        `json:"max_uses"`
	UsesCount     int        `json:"uses_count"`
	ValidFrom     *time.Time `json:"valid_from"`
	ValidUntil    *time.Time `json:"valid_until"`
	IsActive      bool       `json:"is_active"`
	CreatedAt     time.Time  `json:"created_at"`
}

// ListPromotions returns all promotions.
func (h *Handler) ListPromotions(c *gin.Context) {
	rows, err := h.db.Query(`
		SELECT p.id, p.code, p.description, p.discount_type, p.discount_value,
		       p.plan_id, up.name, p.max_uses, p.uses_count,
		       p.valid_from, p.valid_until, p.is_active, p.created_at
		FROM promotions p
		LEFT JOIN user_plans up ON up.id = p.plan_id
		ORDER BY p.is_active DESC, p.created_at DESC`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch promotions")
		return
	}
	defer rows.Close()

	promos := []Promotion{}
	for rows.Next() {
		var p Promotion
		rows.Scan(&p.ID, &p.Code, &p.Description, &p.DiscountType, &p.DiscountValue,
			&p.PlanID, &p.PlanName, &p.MaxUses, &p.UsesCount,
			&p.ValidFrom, &p.ValidUntil, &p.IsActive, &p.CreatedAt)
		promos = append(promos, p)
	}
	c.JSON(http.StatusOK, gin.H{"data": promos})
}

// CreatePromotion creates a new discount code.
func (h *Handler) CreatePromotion(c *gin.Context) {
	var req struct {
		Code          string     `json:"code" binding:"required,min=3,max=50"`
		Description   string     `json:"description"`
		DiscountType  string     `json:"discount_type"`
		DiscountValue float64    `json:"discount_value" binding:"required,gt=0"`
		PlanID        *int       `json:"plan_id"`
		MaxUses       int        `json:"max_uses"`
		ValidFrom     *time.Time `json:"valid_from"`
		ValidUntil    *time.Time `json:"valid_until"`
		IsActive      *bool      `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.Code = strings.ToUpper(strings.TrimSpace(req.Code))
	if req.DiscountType == "" {
		req.DiscountType = "percent"
	}
	if req.DiscountType != "percent" && req.DiscountType != "fixed" {
		respondError(c, http.StatusBadRequest, "discount_type must be percent or fixed")
		return
	}
	if req.DiscountType == "percent" && req.DiscountValue > 100 {
		respondError(c, http.StatusBadRequest, "percent discount cannot exceed 100")
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	var id int
	err := h.db.QueryRow(`INSERT INTO promotions
		(code,description,discount_type,discount_value,plan_id,max_uses,valid_from,valid_until,is_active)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		req.Code, nullableString(req.Description), req.DiscountType, req.DiscountValue,
		req.PlanID, req.MaxUses, req.ValidFrom, req.ValidUntil, isActive).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			respondError(c, http.StatusConflict, "promo code already exists")
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to create promotion")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id, "message": "promotion created"})
}

// UpdatePromotion updates a promotion's settings. Only the fields present in the request
// change; send "clear_plan": true / "clear_dates": true to remove a restriction.
func (h *Handler) UpdatePromotion(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Description   *string    `json:"description"`
		DiscountType  *string    `json:"discount_type" binding:"omitempty,oneof=percent fixed"`
		DiscountValue *float64   `json:"discount_value" binding:"omitempty,gt=0"`
		PlanID        *int       `json:"plan_id"`
		ClearPlan     bool       `json:"clear_plan"`
		MaxUses       *int       `json:"max_uses" binding:"omitempty,min=0"`
		ValidFrom     *time.Time `json:"valid_from"`
		ValidUntil    *time.Time `json:"valid_until"`
		ClearDates    bool       `json:"clear_dates"`
		IsActive      *bool      `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.DiscountValue != nil && *req.DiscountValue > 100 {
		dt := ""
		h.db.QueryRow(`SELECT discount_type FROM promotions WHERE id=$1`, id).Scan(&dt)
		if (req.DiscountType != nil && *req.DiscountType == "percent") || (req.DiscountType == nil && dt == "percent") {
			respondError(c, http.StatusBadRequest, "percent discount cannot exceed 100")
			return
		}
	}
	res, err := h.db.Exec(`UPDATE promotions SET
		description    = COALESCE($1, description),
		discount_type  = COALESCE($2, discount_type),
		discount_value = COALESCE($3, discount_value),
		plan_id        = CASE WHEN $4 THEN NULL ELSE COALESCE($5, plan_id) END,
		max_uses       = COALESCE($6, max_uses),
		valid_from     = CASE WHEN $7 THEN NULL ELSE COALESCE($8, valid_from) END,
		valid_until    = CASE WHEN $7 THEN NULL ELSE COALESCE($9, valid_until) END,
		is_active      = COALESCE($10, is_active)
		WHERE id = $11`,
		req.Description, req.DiscountType, req.DiscountValue, req.ClearPlan, req.PlanID, req.MaxUses,
		req.ClearDates, req.ValidFrom, req.ValidUntil, req.IsActive, id)
	if isBadValue(err) {
		respondError(c, http.StatusBadRequest, "invalid value (unknown plan_id, or a number out of range)")
		return
	}
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update promotion")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(c, http.StatusNotFound, "promotion not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "promotion updated"})
}

// DeletePromotion removes a promotion.
func (h *Handler) DeletePromotion(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	res, err := h.db.Exec(`DELETE FROM promotions WHERE id=$1`, id)
	if !writeOK(c, res, err, "promotion") {
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "promotion deleted"})
}

// ValidatePromoCode checks if a code is valid (for plan_id, if given) and previews the
// discount. It does not consume a use.
func (h *Handler) ValidatePromoCode(c *gin.Context) {
	var req struct {
		Code          string  `json:"code" binding:"required"`
		OriginalPrice float64 `json:"original_price"`
		PlanID        *int    `json:"plan_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.Code = strings.ToUpper(strings.TrimSpace(req.Code))

	var p Promotion
	err := h.db.QueryRow(`
		SELECT id, code, description, discount_type, discount_value,
		       plan_id, NULL, max_uses, uses_count,
		       valid_from, valid_until, is_active, created_at
		FROM promotions WHERE code=$1`, req.Code).
		Scan(&p.ID, &p.Code, &p.Description, &p.DiscountType, &p.DiscountValue,
			&p.PlanID, &p.PlanName, &p.MaxUses, &p.UsesCount,
			&p.ValidFrom, &p.ValidUntil, &p.IsActive, &p.CreatedAt)
	if err != nil {
		respondError(c, http.StatusNotFound, "invalid or unknown promo code")
		return
	}

	now := time.Now()
	if !p.IsActive {
		respondError(c, http.StatusBadRequest, "promotion is no longer active")
		return
	}
	if p.MaxUses > 0 && p.UsesCount >= p.MaxUses {
		respondError(c, http.StatusBadRequest, "promotion usage limit reached")
		return
	}
	if p.ValidFrom != nil && now.Before(*p.ValidFrom) {
		respondError(c, http.StatusBadRequest, fmt.Sprintf("promotion starts on %s", p.ValidFrom.Format("Jan 2, 2006")))
		return
	}
	if p.ValidUntil != nil && now.After(*p.ValidUntil) {
		respondError(c, http.StatusBadRequest, "promotion has expired")
		return
	}
	if p.PlanID != nil && (req.PlanID == nil || *req.PlanID != *p.PlanID) {
		respondError(c, http.StatusBadRequest, "promotion does not apply to this plan")
		return
	}

	var discountAmount, finalPrice float64
	if req.OriginalPrice > 0 {
		discountAmount = promoDiscount(p.DiscountType, p.DiscountValue, req.OriginalPrice)
		finalPrice = round2(req.OriginalPrice - discountAmount)
	}

	c.JSON(http.StatusOK, gin.H{
		"valid":           true,
		"promotion":       p,
		"discount_amount": discountAmount,
		"final_price":     finalPrice,
	})
}

// ApplyPromoCode consumes one use of a code outside the invoice flow (for example a
// discount given at the counter). It runs the same checks as Validate, atomically.
func (h *Handler) ApplyPromoCode(c *gin.Context) {
	var req struct {
		Code   string `json:"code" binding:"required"`
		PlanID *int   `json:"plan_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := redeemPromotion(h.db, req.Code, req.PlanID, 0); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "promo code applied"})
}
