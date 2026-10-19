package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/freeradius-manager/backend/internal/settings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Billing rules (one place, used by the handlers and the scheduler)
// ─────────────────────────────────────────────────────────────────────────────
//
// Model: prepaid by default.
//   - Assigning a paid plan raises an invoice. The plan period is added to the customer's
//     paid-until date when that invoice is fully paid (recordPayment).
//   - "Start now" (start_now on assignment) adds the period immediately and marks the
//     invoice service_applied, so paying it later does not add a second period. With the
//     suspend_overdue setting, such a customer is blocked when the invoice goes overdue.
//   - A free plan (price 0), or a promotion that makes the invoice 0, adds the period at once.
//   - Reversing a payment re-opens the invoice and takes the days back.
// Access itself is enforced by FreeRADIUS through syncAccess (access.go).

var (
	errInvoiceNotOpen   = errors.New("invoice is not open (already paid or cancelled)")
	errCurrencyMismatch = errors.New("payment currency does not match the invoice")
	errOverpayment      = errors.New("payment is larger than the amount still owed on the invoice")
)

// round2 rounds money to cents. Amounts are NUMERIC(12,2) in the database; Go only computes
// discounts, which are rounded here before they are stored.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

func defaultCurrency() string {
	return strings.ToUpper(settings.String("default_currency", "DEFAULT_CURRENCY", "USD"))
}

// nextDocNumber returns PREFIX-YYYY-000001 from a Postgres sequence (no collisions, no
// reuse; a rolled-back transaction leaves a gap, which is normal for sequences).
func nextDocNumber(q dbtx, prefix, sequence string) (string, error) {
	var n int64
	if err := q.QueryRow(`SELECT nextval($1::regclass)`, sequence).Scan(&n); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d-%06d", prefix, time.Now().Year(), n), nil
}

// promoDiscount computes the discount a promotion gives on an amount.
func promoDiscount(discountType string, value, amount float64) float64 {
	var d float64
	if discountType == "percent" {
		d = amount * value / 100
	} else {
		d = value
	}
	if d > amount {
		d = amount
	}
	if d < 0 {
		d = 0
	}
	return round2(d)
}

// redeemPromotion validates a code for a plan and consumes one use atomically, so the
// usage limit holds even under concurrent use. Returns the discount on amount.
func redeemPromotion(q dbtx, code string, planID *int, amount float64) (float64, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	var dtype string
	var value float64
	err := q.QueryRow(`
		UPDATE promotions SET uses_count = uses_count + 1
		WHERE code = $1 AND is_active
		  AND (max_uses = 0 OR uses_count < max_uses)
		  AND (valid_from IS NULL OR valid_from <= NOW())
		  AND (valid_until IS NULL OR valid_until >= NOW())
		  AND (plan_id IS NULL OR plan_id = $2)
		RETURNING discount_type, discount_value`, code, planID).Scan(&dtype, &value)
	if err == sql.ErrNoRows {
		return 0, promoRejectReason(q, code, planID)
	}
	if err != nil {
		return 0, err
	}
	return promoDiscount(dtype, value, amount), nil
}

// promoRejectReason explains why a code cannot be used (for the error message).
func promoRejectReason(q dbtx, code string, planID *int) error {
	var active bool
	var maxUses, uses int
	var from, until sql.NullTime
	var promoPlan sql.NullInt64
	err := q.QueryRow(`SELECT is_active, max_uses, uses_count, valid_from, valid_until, plan_id
		FROM promotions WHERE code = $1`, code).Scan(&active, &maxUses, &uses, &from, &until, &promoPlan)
	switch {
	case err != nil:
		return errors.New("invalid or unknown promo code")
	case !active:
		return errors.New("promotion is no longer active")
	case maxUses > 0 && uses >= maxUses:
		return errors.New("promotion usage limit reached")
	case from.Valid && time.Now().Before(from.Time):
		return fmt.Errorf("promotion starts on %s", from.Time.Format("Jan 2, 2006"))
	case until.Valid && time.Now().After(until.Time):
		return errors.New("promotion has expired")
	case promoPlan.Valid && (planID == nil || int64(*planID) != promoPlan.Int64):
		return errors.New("promotion does not apply to this plan")
	}
	return errors.New("promotion cannot be applied")
}

type newInvoice struct {
	UserID         *int
	Username       string
	PlanID         *int
	PlanName       *string
	Amount         float64 // before discount
	Currency       string
	DueDate        *time.Time
	Notes          string
	PromoCode      string
	CreatedBy      *int
	ServiceApplied bool
	AppliedDays    int
	PeriodEnd      *time.Time
}

type createdInvoice struct {
	ID       int
	Number   string
	Amount   float64 // after discount
	Discount float64
}

// createInvoiceTx inserts an invoice, applying a promotion code if one is given.
func createInvoiceTx(q dbtx, in newInvoice) (*createdInvoice, error) {
	if in.Currency == "" {
		in.Currency = defaultCurrency()
	}
	in.Currency = strings.ToUpper(in.Currency)
	if in.Amount < 0 {
		return nil, errors.New("amount cannot be negative")
	}
	out := &createdInvoice{Amount: round2(in.Amount)}
	var promo interface{}
	if strings.TrimSpace(in.PromoCode) != "" {
		d, err := redeemPromotion(q, in.PromoCode, in.PlanID, out.Amount)
		if err != nil {
			return nil, err
		}
		out.Discount = d
		out.Amount = round2(out.Amount - d)
		promo = strings.ToUpper(strings.TrimSpace(in.PromoCode))
	}
	if in.DueDate == nil {
		due := time.Now().AddDate(0, 0, settings.Int("invoice_due_days", "", 7))
		in.DueDate = &due
	}
	number, err := nextDocNumber(q, "INV", "invoice_number_seq")
	if err != nil {
		return nil, err
	}
	out.Number = number
	err = q.QueryRow(`
		INSERT INTO invoices (invoice_number, user_id, username, plan_id, plan_name, amount, currency,
		                      due_date, notes, created_by, promo_code, discount_amount,
		                      service_applied, applied_days, period_end)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
		number, in.UserID, in.Username, in.PlanID, in.PlanName, out.Amount, in.Currency,
		in.DueDate.Format("2006-01-02"), nullableString(in.Notes), in.CreatedBy, promo, out.Discount,
		in.ServiceApplied, in.AppliedDays, in.PeriodEnd).Scan(&out.ID)
	if err != nil {
		return nil, err
	}
	return out, nil
}

type paymentInput struct {
	InvoiceID     *int
	Username      string
	Amount        float64
	Currency      string
	PaymentMethod string
	Gateway       string
	GatewayRef    string
	Notes         string
	ProcessedBy   *int
}

type paymentOutcome struct {
	PaymentID     int
	Receipt       string
	InvoicePaid   bool
	PeriodGranted bool
	NewExpiry     *time.Time
	Username      string
}

// recordPaymentTx records money received. With an invoice it validates the invoice, and
// when the invoice becomes fully paid it marks it paid and adds the plan period (once).
func recordPaymentTx(q dbtx, in paymentInput) (*paymentOutcome, error) {
	if in.Amount <= 0 {
		return nil, errors.New("amount must be greater than zero")
	}
	in.Amount = round2(in.Amount)
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.PaymentMethod == "" {
		in.PaymentMethod = "cash"
	}
	out := &paymentOutcome{Username: in.Username}

	var inv struct {
		amount         float64
		currency       string
		status         string
		username       string
		userID         sql.NullInt64
		planID         sql.NullInt64
		serviceApplied bool
		paid           float64
	}
	if in.InvoiceID != nil {
		err := q.QueryRow(`SELECT amount, currency, status, username, user_id, plan_id, service_applied
			FROM invoices WHERE id = $1 FOR UPDATE`, *in.InvoiceID).
			Scan(&inv.amount, &inv.currency, &inv.status, &inv.username, &inv.userID, &inv.planID, &inv.serviceApplied)
		if err == sql.ErrNoRows {
			return nil, errors.New("invoice not found")
		}
		if err != nil {
			return nil, err
		}
		if inv.status != "pending" && inv.status != "overdue" {
			return nil, errInvoiceNotOpen
		}
		if in.Currency == "" {
			in.Currency = inv.currency
		}
		if in.Currency != inv.currency {
			return nil, errCurrencyMismatch
		}
		if in.Username == "" {
			in.Username = inv.username
		}
		if err := q.QueryRow(`SELECT COALESCE(SUM(amount), 0) FROM payments
			WHERE invoice_id = $1 AND status = 'confirmed'`, *in.InvoiceID).Scan(&inv.paid); err != nil {
			return nil, err
		}
		if in.Amount > round2(inv.amount-inv.paid)+0.005 {
			return nil, errOverpayment
		}
	}
	if in.Currency == "" {
		in.Currency = defaultCurrency()
	}
	out.Username = in.Username

	receipt, err := nextDocNumber(q, "RCP", "receipt_number_seq")
	if err != nil {
		return nil, err
	}
	out.Receipt = receipt
	err = q.QueryRow(`
		INSERT INTO payments (invoice_id, username, amount, currency, payment_method,
		                      gateway, gateway_ref, notes, receipt_number, status, processed_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'confirmed',$10) RETURNING id`,
		in.InvoiceID, nullableString(in.Username), in.Amount, in.Currency, in.PaymentMethod,
		nullableString(in.Gateway), nullableString(in.GatewayRef), nullableString(in.Notes),
		receipt, in.ProcessedBy).Scan(&out.PaymentID)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, errors.New("a payment with this gateway reference is already recorded")
		}
		return nil, err
	}

	if in.InvoiceID == nil || inv.paid+in.Amount < inv.amount-0.005 {
		return out, nil // standalone payment, or a partial payment on an open invoice
	}

	out.InvoicePaid = true
	if _, err := q.Exec(`UPDATE invoices SET status = 'paid', paid_at = NOW(), updated_at = NOW() WHERE id = $1`,
		*in.InvoiceID); err != nil {
		return nil, err
	}
	if !inv.userID.Valid {
		return out, nil
	}
	uid := int(inv.userID.Int64)
	if !inv.serviceApplied && inv.planID.Valid {
		p, err := loadPlan(q, int(inv.planID.Int64))
		if err != nil {
			return nil, fmt.Errorf("plan of invoice: %w", err)
		}
		exp, days, err := grantPlanPeriod(q, uid, p)
		if err != nil {
			return nil, err
		}
		if _, err := q.Exec(`UPDATE invoices SET service_applied = TRUE, applied_days = $1, period_end = $2 WHERE id = $3`,
			days, exp, *in.InvoiceID); err != nil {
			return nil, err
		}
		out.PeriodGranted, out.NewExpiry = true, &exp
		return out, nil
	}
	// Service was given on credit (or the invoice has no plan): paying lifts an overdue
	// block if the paid-until date is still current.
	var username string
	err = q.QueryRow(`UPDATE radius_users SET status = 'active', updated_at = NOW()
		WHERE id = $1 AND status = 'expired' AND account_expiry IS NOT NULL
		  AND account_expiry + $2::int >= CURRENT_DATE
		RETURNING username`, uid, graceDays()).Scan(&username)
	if err == nil {
		return out, syncAccess(q, username)
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	return out, nil
}

// reversePaymentTx marks a payment reversed (it is kept for the record). If that leaves its
// invoice unpaid, the invoice re-opens and the period it granted is taken back.
func reversePaymentTx(q dbtx, paymentID int, by *int, reason string) (string, error) {
	var status, username string
	var invoiceID sql.NullInt64
	err := q.QueryRow(`SELECT status, COALESCE(username, ''), invoice_id FROM payments WHERE id = $1 FOR UPDATE`, paymentID).
		Scan(&status, &username, &invoiceID)
	if err == sql.ErrNoRows {
		return "", errors.New("payment not found")
	}
	if err != nil {
		return "", err
	}
	if status != "confirmed" {
		return "", errors.New("payment is already reversed")
	}
	if _, err := q.Exec(`UPDATE payments SET status = 'reversed', reversed_at = NOW(), reversed_by = $1,
		reversal_reason = $2 WHERE id = $3`, by, nullableString(reason), paymentID); err != nil {
		return "", err
	}
	if !invoiceID.Valid {
		return "payment reversed", nil
	}

	var amount, paid float64
	var invStatus string
	var userID sql.NullInt64
	var serviceApplied bool
	var appliedDays int
	if err := q.QueryRow(`SELECT amount, status, user_id, service_applied, applied_days FROM invoices WHERE id = $1 FOR UPDATE`,
		invoiceID.Int64).Scan(&amount, &invStatus, &userID, &serviceApplied, &appliedDays); err != nil {
		return "", err
	}
	q.QueryRow(`SELECT COALESCE(SUM(amount), 0) FROM payments WHERE invoice_id = $1 AND status = 'confirmed'`,
		invoiceID.Int64).Scan(&paid)
	if invStatus != "paid" || paid >= amount-0.005 {
		return "payment reversed", nil
	}
	if _, err := q.Exec(`UPDATE invoices SET
		status = CASE WHEN due_date IS NOT NULL AND due_date < CURRENT_DATE THEN 'overdue' ELSE 'pending' END,
		paid_at = NULL, updated_at = NOW() WHERE id = $1`, invoiceID.Int64); err != nil {
		return "", err
	}
	msg := "payment reversed; invoice re-opened"
	if serviceApplied && appliedDays > 0 && userID.Valid {
		if err := revokePlanPeriod(q, int(userID.Int64), appliedDays); err != nil {
			return "", err
		}
		if _, err := q.Exec(`UPDATE invoices SET service_applied = FALSE, applied_days = 0, period_end = NULL WHERE id = $1`,
			invoiceID.Int64); err != nil {
			return "", err
		}
		msg += fmt.Sprintf("; %d day(s) removed from %s's paid-until date", appliedDays, username)
	}
	return msg, nil
}

type assignResult struct {
	Message   string
	InvoiceID int
	Invoice   string
	Amount    float64
	Expiry    *time.Time
}

// assignPlanTx links a plan to a customer, applies its limits and handles billing:
// free plans start at once; paid plans get an invoice (unless one is already open for
// the same plan) and start when paid, or at once with startNow.
func assignPlanTx(q dbtx, userID int, username string, planID int, startNow bool, promoCode string, by *int) (*assignResult, error) {
	p, err := loadPlan(q, planID)
	if err == sql.ErrNoRows {
		return nil, errors.New("plan not found")
	}
	if err != nil {
		return nil, err
	}
	if !p.IsActive {
		return nil, errors.New("plan is inactive")
	}
	if err := applyPlanLimits(q, userID, username, p); err != nil {
		return nil, err
	}
	res := &assignResult{}

	if p.Price <= 0 {
		exp, _, err := grantPlanPeriod(q, userID, p)
		if err != nil {
			return nil, err
		}
		res.Expiry = &exp
		res.Message = fmt.Sprintf("free plan '%s' assigned to %s; paid until %s", p.Name, username, exp.Format("2006-01-02"))
		return res, nil
	}

	// Prepaid: a customer with no current paid period gets no access until the invoice is
	// paid (paid-until is set to yesterday, so FreeRADIUS rejects them). Someone switching
	// plans mid-period keeps the time they already paid for.
	if !startNow {
		if err := blockUntilPaid(q, userID); err != nil {
			return nil, err
		}
	}

	var openID int
	var openNumber string
	err = q.QueryRow(`SELECT id, invoice_number FROM invoices
		WHERE user_id = $1 AND plan_id = $2 AND status IN ('pending','overdue') ORDER BY id DESC LIMIT 1`,
		userID, p.ID).Scan(&openID, &openNumber)
	if err == nil {
		res.InvoiceID, res.Invoice = openID, openNumber
		res.Message = fmt.Sprintf("plan '%s' assigned to %s; invoice %s is already open for it", p.Name, username, openNumber)
		return res, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	in := newInvoice{
		UserID: &userID, Username: username, PlanID: &p.ID, PlanName: &p.Name,
		Amount: p.Price, Currency: p.Currency, PromoCode: promoCode, CreatedBy: by,
	}
	if startNow {
		exp, days, err := grantPlanPeriod(q, userID, p)
		if err != nil {
			return nil, err
		}
		in.ServiceApplied, in.AppliedDays, in.PeriodEnd = true, days, &exp
		res.Expiry = &exp
	}
	inv, err := createInvoiceTx(q, in)
	if err != nil {
		return nil, err
	}
	res.InvoiceID, res.Invoice, res.Amount = inv.ID, inv.Number, inv.Amount

	if inv.Amount <= 0 { // 100% promotion: settle it now
		if _, err := q.Exec(`UPDATE invoices SET status = 'paid', paid_at = NOW() WHERE id = $1`, inv.ID); err != nil {
			return nil, err
		}
		if !startNow {
			exp, days, err := grantPlanPeriod(q, userID, p)
			if err != nil {
				return nil, err
			}
			q.Exec(`UPDATE invoices SET service_applied = TRUE, applied_days = $1, period_end = $2 WHERE id = $3`, days, exp, inv.ID)
			res.Expiry = &exp
		}
		res.Message = fmt.Sprintf("plan '%s' assigned to %s; invoice %s fully discounted", p.Name, username, inv.Number)
		return res, nil
	}
	if startNow {
		res.Message = fmt.Sprintf("plan '%s' started for %s (paid until %s); invoice %s raised for %s %.2f",
			p.Name, username, res.Expiry.Format("2006-01-02"), inv.Number, p.Currency, inv.Amount)
	} else {
		res.Message = fmt.Sprintf("plan '%s' assigned to %s; invoice %s raised for %s %.2f — service starts when it is paid",
			p.Name, username, inv.Number, p.Currency, inv.Amount)
	}
	return res, nil
}
