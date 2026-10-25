package handlers

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/gin-gonic/gin"
)

// Invoice holds a billing record. Amounts are NUMERIC(12,2) in the database; Go carries
// them as float64 for JSON only, and money arithmetic is done in SQL or rounded to cents
// (billing_core.go). username/plan_name are copied onto the invoice so history survives
// deleting the user or plan.
type Invoice struct {
	ID             int        `json:"id"`
	InvoiceNumber  string     `json:"invoice_number"`
	UserID         *int       `json:"user_id"`
	Username       string     `json:"username"`
	PlanID         *int       `json:"plan_id"`
	PlanName       *string    `json:"plan_name"`
	Amount         float64    `json:"amount"`
	AmountPaid     float64    `json:"amount_paid"`
	Discount       float64    `json:"discount_amount"`
	PromoCode      *string    `json:"promo_code"`
	Currency       string     `json:"currency"`
	Status         string     `json:"status"`
	DueDate        *string    `json:"due_date"`
	PaidAt         *time.Time `json:"paid_at"`
	ServiceApplied bool       `json:"service_applied"`
	PeriodEnd      *string    `json:"period_end"`
	Notes          *string    `json:"notes"`
	CreatedBy      *string    `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
}

const invoiceColumns = `
	i.id, i.invoice_number, i.user_id, i.username, i.plan_id, i.plan_name,
	i.amount, COALESCE((SELECT SUM(p.amount) FROM payments p WHERE p.invoice_id = i.id AND p.status = 'confirmed'), 0),
	i.discount_amount, i.promo_code, i.currency, i.status, i.due_date::text, i.paid_at,
	i.service_applied, i.period_end::text, i.notes, au.username, i.created_at`

func scanInvoice(scan func(...interface{}) error, inv *Invoice) error {
	return scan(&inv.ID, &inv.InvoiceNumber, &inv.UserID, &inv.Username, &inv.PlanID, &inv.PlanName,
		&inv.Amount, &inv.AmountPaid, &inv.Discount, &inv.PromoCode, &inv.Currency, &inv.Status,
		&inv.DueDate, &inv.PaidAt, &inv.ServiceApplied, &inv.PeriodEnd, &inv.Notes, &inv.CreatedBy, &inv.CreatedAt)
}

// CurrencyTotal is a money figure for one currency (totals never mix currencies).
type CurrencyTotal struct {
	Currency string  `json:"currency"`
	Amount   float64 `json:"amount"`
	Count    int     `json:"count"`
}

// ListInvoices returns paginated invoices with filters, and a summary over all invoices.
func (h *Handler) ListInvoices(c *gin.Context) {
	offset, limit := paginationParams(c)
	status := c.Query("status")
	username := c.Query("username")

	where := "WHERE 1=1"
	args := []interface{}{}
	argN := 1

	if status == "open" {
		where += " AND i.status IN ('pending','overdue')"
	} else if status != "" {
		where += fmt.Sprintf(" AND i.status=$%d", argN)
		args = append(args, status)
		argN++
	}
	if username != "" {
		where += fmt.Sprintf(" AND i.username ILIKE $%d", argN)
		args = append(args, "%"+username+"%")
		argN++
	}

	var total int
	h.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM invoices i %s", where), args...).Scan(&total)

	dataArgs := append(args, limit, offset)
	rows, err := h.db.Query(fmt.Sprintf(`
		SELECT %s
		FROM invoices i
		LEFT JOIN app_users au ON au.id = i.created_by
		%s
		ORDER BY i.created_at DESC
		LIMIT $%d OFFSET $%d`, invoiceColumns, where, argN, argN+1), dataArgs...)
	if err != nil {
		h.log.WithError(err).Error("list invoices")
		respondError(c, http.StatusInternalServerError, "failed to fetch invoices")
		return
	}
	defer rows.Close()

	invoices := []Invoice{}
	for rows.Next() {
		var inv Invoice
		if err := scanInvoice(rows.Scan, &inv); err != nil {
			h.log.WithError(err).Warn("scan invoice")
			continue
		}
		invoices = append(invoices, inv)
	}

	// Summary over all invoices (not the filtered page). "Collected" is the sum of confirmed
	// payments linked to invoices, per currency, so it matches the Payments page.
	type Summary struct {
		Collected   []CurrencyTotal `json:"collected"`
		Outstanding []CurrencyTotal `json:"outstanding"`
		Pending     int             `json:"pending"`
		Paid        int             `json:"paid"`
		Overdue     int             `json:"overdue"`
	}
	sum := Summary{Collected: []CurrencyTotal{}, Outstanding: []CurrencyTotal{}}
	h.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE status='pending'),
	               COUNT(*) FILTER (WHERE status='paid'),
	               COUNT(*) FILTER (WHERE status='overdue')
	               FROM invoices`).Scan(&sum.Pending, &sum.Paid, &sum.Overdue)
	sum.Collected = currencyTotals(h, `SELECT p.currency, SUM(p.amount), COUNT(*) FROM payments p
		WHERE p.status='confirmed' AND p.invoice_id IS NOT NULL GROUP BY p.currency ORDER BY p.currency`)
	sum.Outstanding = currencyTotals(h, `SELECT i.currency,
		SUM(i.amount - COALESCE((SELECT SUM(p.amount) FROM payments p WHERE p.invoice_id=i.id AND p.status='confirmed'),0)),
		COUNT(*) FROM invoices i WHERE i.status IN ('pending','overdue') GROUP BY i.currency ORDER BY i.currency`)

	c.JSON(http.StatusOK, gin.H{"data": invoices, "total": total, "summary": sum})
}

func currencyTotals(h *Handler, query string, args ...interface{}) []CurrencyTotal {
	out := []CurrencyTotal{}
	rows, err := h.db.Query(query, args...)
	if err != nil {
		h.log.WithError(err).Warn("currency totals")
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t CurrencyTotal
		if rows.Scan(&t.Currency, &t.Amount, &t.Count) == nil {
			out = append(out, t)
		}
	}
	return out
}

// GetInvoice returns one invoice with its payments.
func (h *Handler) GetInvoice(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var inv Invoice
	err = scanInvoice(h.db.QueryRow(fmt.Sprintf(`SELECT %s FROM invoices i
		LEFT JOIN app_users au ON au.id = i.created_by WHERE i.id = $1`, invoiceColumns), id).Scan, &inv)
	if err != nil {
		respondError(c, http.StatusNotFound, "invoice not found")
		return
	}
	payments := listPaymentsWhere(h, "p.invoice_id = $1", id)
	c.JSON(http.StatusOK, gin.H{"invoice": inv, "payments": payments})
}

// CreateInvoice creates a manual invoice. With plan_id, paying it adds that plan's period to
// the customer. promo_code applies a promotion (validated and counted).
func (h *Handler) CreateInvoice(c *gin.Context) {
	var req struct {
		Username  string  `json:"username" binding:"required"`
		PlanID    *int    `json:"plan_id"`
		Amount    float64 `json:"amount"`
		Currency  string  `json:"currency"`
		DueDate   string  `json:"due_date"`
		Notes     string  `json:"notes"`
		PromoCode string  `json:"promo_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	claims, _ := middleware.GetClaims(c)

	in := newInvoice{Username: strings.TrimSpace(req.Username), Amount: req.Amount, Currency: req.Currency,
		Notes: req.Notes, PromoCode: req.PromoCode, CreatedBy: &claims.UserID}

	var uid int
	if err := h.db.QueryRow(`SELECT id FROM radius_users WHERE username=$1`, in.Username).Scan(&uid); err == nil {
		in.UserID = &uid
	}
	if req.PlanID != nil {
		p, err := loadPlan(h.db, *req.PlanID)
		if err != nil {
			respondError(c, http.StatusBadRequest, "plan not found")
			return
		}
		if in.UserID == nil {
			respondError(c, http.StatusBadRequest, "a plan invoice needs an existing RADIUS user")
			return
		}
		in.PlanID, in.PlanName, in.PlanDays = &p.ID, &p.Name, &p.ValidityDays
		if in.Amount <= 0 {
			in.Amount = p.Price
		}
		if in.Currency == "" {
			in.Currency = p.Currency
		}
	}
	if in.Amount <= 0 {
		respondError(c, http.StatusBadRequest, "amount must be greater than zero")
		return
	}
	if req.DueDate != "" {
		d, err := time.Parse("2006-01-02", req.DueDate)
		if err != nil {
			respondError(c, http.StatusBadRequest, "due_date must be YYYY-MM-DD")
			return
		}
		in.DueDate = &d
	}

	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback()
	inv, err := createInvoiceTx(tx, in)
	if err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to create invoice")
		return
	}
	go h.dispatchWebhook("invoice.created", gin.H{"invoice_id": inv.ID, "invoice_number": inv.Number,
		"username": in.Username, "amount": inv.Amount})
	c.JSON(http.StatusCreated, gin.H{"id": inv.ID, "invoice_number": inv.Number, "amount": inv.Amount,
		"discount_amount": inv.Discount, "message": "invoice created"})
}

// UpdateInvoice edits an open invoice: due date, notes, amount (only while nothing has been
// paid), or status overdue/cancelled/pending. An invoice becomes "paid" only by recording
// payments against it (Payments → Record Payment, or Billing → Record Payment), so
// revenue, receipts and the customer's paid-until date always agree.
func (h *Handler) UpdateInvoice(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Status  string  `json:"status"`
		Amount  float64 `json:"amount"`
		DueDate string  `json:"due_date"`
		Notes   *string `json:"notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	switch req.Status {
	case "", "pending", "overdue", "cancelled":
	case "paid":
		respondError(c, http.StatusBadRequest, "record a payment against the invoice to mark it paid")
		return
	default:
		respondError(c, http.StatusBadRequest, "status must be pending, overdue or cancelled")
		return
	}

	var dueDate interface{}
	if req.DueDate != "" {
		if _, err := time.Parse("2006-01-02", req.DueDate); err != nil {
			respondError(c, http.StatusBadRequest, "due_date must be YYYY-MM-DD")
			return
		}
		dueDate = req.DueDate
	}
	var notes interface{}
	if req.Notes != nil {
		notes = *req.Notes
	}

	// Check and change inside one transaction with the invoice row locked, the same lock a
	// payment takes. Otherwise a payment landing between the check and the update could be
	// recorded on an invoice that is then cancelled or re-priced.
	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback()
	var cur string
	if err := tx.QueryRow(`SELECT status FROM invoices WHERE id=$1 FOR UPDATE`, id).Scan(&cur); err != nil {
		respondError(c, http.StatusNotFound, "invoice not found")
		return
	}
	var paid float64
	tx.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM payments WHERE invoice_id=$1 AND status='confirmed'`, id).Scan(&paid)
	if cur == "paid" || cur == "cancelled" {
		respondError(c, http.StatusConflict, "paid and cancelled invoices cannot be edited; reverse the payment first")
		return
	}
	if paid > 0 && (req.Amount > 0 || req.Status == "cancelled") {
		respondError(c, http.StatusConflict, "this invoice has payments; reverse them before changing the amount or cancelling")
		return
	}
	if req.Amount > 0 && round2(req.Amount) <= 0 {
		respondError(c, http.StatusBadRequest, "amount must be at least 0.01")
		return
	}
	if _, err := tx.Exec(`UPDATE invoices SET status=COALESCE(NULLIF($1,''),status),
		amount=CASE WHEN $2::numeric > 0 THEN $2::numeric ELSE amount END,
		due_date=COALESCE($3::date,due_date),
		notes=COALESCE($4,notes),
		updated_at=NOW() WHERE id=$5`,
		req.Status, round2(req.Amount), dueDate, notes, id); err != nil {
		h.log.WithError(err).Error("update invoice")
		respondError(c, http.StatusInternalServerError, "failed to update invoice")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update invoice")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "invoice updated"})
}

// DeleteInvoice removes an invoice that has never been paid. Invoices with payments are
// kept for the record: cancel them (after reversing payments) instead.
func (h *Handler) DeleteInvoice(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	// Lock the invoice first: a payment locks it too (recordPaymentTx), so one being recorded
	// right now either finishes before the count below (and is seen) or waits for the delete.
	// Without the lock the delete could land just after it and detach it (ON DELETE SET NULL).
	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete invoice")
		return
	}
	defer tx.Rollback()
	var one, payments int
	err = tx.QueryRow(`SELECT 1 FROM invoices WHERE id=$1 FOR UPDATE`, id).Scan(&one)
	if err == sql.ErrNoRows {
		respondError(c, http.StatusNotFound, "invoice not found")
		return
	}
	if err == nil {
		err = tx.QueryRow(`SELECT COUNT(*) FROM payments WHERE invoice_id=$1`, id).Scan(&payments)
	}
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete invoice")
		return
	}
	if payments > 0 {
		respondError(c, http.StatusConflict, "invoice has payments recorded; cancel it instead of deleting")
		return
	}
	if _, err := tx.Exec(`DELETE FROM invoices WHERE id=$1`, id); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete invoice")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete invoice")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "invoice deleted"})
}
