package handlers

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────────
// Payments
// ─────────────────────────────────────────────────────────────────────────────
//
// A payment is a record of money received by staff (cash, bank transfer, mobile money,
// card). There is no payment-gateway integration: a gateway callback would call
// recordPaymentTx the same way. Payments are never deleted, only reversed.

type Payment struct {
	ID             int        `json:"id"`
	InvoiceID      *int       `json:"invoice_id"`
	InvoiceNumber  *string    `json:"invoice_number"`
	Username       *string    `json:"username"`
	Amount         float64    `json:"amount"`
	Currency       string     `json:"currency"`
	PaymentMethod  string     `json:"payment_method"`
	Gateway        *string    `json:"gateway"`
	GatewayRef     *string    `json:"gateway_ref"`
	Status         string     `json:"status"`
	Notes          *string    `json:"notes"`
	ReceiptNumber  string     `json:"receipt_number"`
	ProcessedBy    *string    `json:"processed_by"`
	ReversedAt     *time.Time `json:"reversed_at"`
	ReversalReason *string    `json:"reversal_reason"`
	CreatedAt      time.Time  `json:"created_at"`
}

const paymentColumns = `
	p.id, p.invoice_id, i.invoice_number, p.username,
	p.amount, p.currency, p.payment_method, p.gateway,
	p.gateway_ref, p.status, p.notes, p.receipt_number,
	u.username, p.reversed_at, p.reversal_reason, p.created_at`

func scanPayment(scan func(...interface{}) error, p *Payment) error {
	return scan(&p.ID, &p.InvoiceID, &p.InvoiceNumber, &p.Username,
		&p.Amount, &p.Currency, &p.PaymentMethod, &p.Gateway,
		&p.GatewayRef, &p.Status, &p.Notes, &p.ReceiptNumber,
		&p.ProcessedBy, &p.ReversedAt, &p.ReversalReason, &p.CreatedAt)
}

func listPaymentsWhere(h *Handler, where string, args ...interface{}) []Payment {
	out := []Payment{}
	rows, err := h.db.Query(fmt.Sprintf(`SELECT %s FROM payments p
		LEFT JOIN invoices i ON i.id = p.invoice_id
		LEFT JOIN app_users u ON u.id = p.processed_by
		WHERE %s ORDER BY p.created_at DESC`, paymentColumns, where), args...)
	if err != nil {
		h.log.WithError(err).Warn("list payments")
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p Payment
		if scanPayment(rows.Scan, &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// ListPayments returns paginated payments with optional filters.
func (h *Handler) ListPayments(c *gin.Context) {
	offset, limit := paginationParams(c)
	username := c.Query("username")
	method := c.Query("method")
	status := c.Query("status")

	rows, err := h.db.Query(fmt.Sprintf(`
		SELECT %s
		FROM payments p
		LEFT JOIN invoices i ON i.id = p.invoice_id
		LEFT JOIN app_users u ON u.id = p.processed_by
		WHERE ($1='' OR p.username ILIKE '%%'||$1||'%%')
		  AND ($2='' OR p.payment_method=$2)
		  AND ($3='' OR p.status=$3)
		ORDER BY p.created_at DESC
		LIMIT $4 OFFSET $5`, paymentColumns),
		username, method, status, limit, offset)
	if err != nil {
		h.log.WithError(err).Error("list payments")
		respondError(c, http.StatusInternalServerError, "failed to fetch payments")
		return
	}
	defer rows.Close()

	payments := []Payment{}
	for rows.Next() {
		var p Payment
		if err := scanPayment(rows.Scan, &p); err != nil {
			h.log.WithError(err).Warn("scan payment")
			continue
		}
		payments = append(payments, p)
	}

	var total int
	h.db.QueryRow(`SELECT COUNT(*) FROM payments p
		WHERE ($1='' OR p.username ILIKE '%'||$1||'%') AND ($2='' OR p.payment_method=$2) AND ($3='' OR p.status=$3)`,
		username, method, status).Scan(&total)

	c.JSON(http.StatusOK, gin.H{
		"data":   payments,
		"total":  total,
		"totals": currencyTotals(h, `SELECT currency, SUM(amount), COUNT(*) FROM payments
			WHERE status='confirmed' AND ($1='' OR username ILIKE '%'||$1||'%') GROUP BY currency ORDER BY currency`, username),
	})
}

// CreatePayment records money received. With invoice_id the invoice is validated (open,
// same currency, not overpaid); when it becomes fully paid it is marked paid and the
// customer's plan period is added, all in one transaction.
func (h *Handler) CreatePayment(c *gin.Context) {
	var req struct {
		InvoiceID     *int    `json:"invoice_id"`
		Username      string  `json:"username"`
		Amount        float64 `json:"amount" binding:"required,gt=0"`
		Currency      string  `json:"currency"`
		PaymentMethod string  `json:"payment_method" binding:"omitempty,oneof=cash bank_transfer mobile_money card online voucher other"`
		Gateway       string  `json:"gateway"`
		GatewayRef    string  `json:"gateway_ref"`
		Notes         string  `json:"notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.InvoiceID == nil && strings.TrimSpace(req.Username) == "" {
		respondError(c, http.StatusBadRequest, "choose an invoice or enter the customer's username")
		return
	}
	claims, _ := middleware.GetClaims(c)

	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback()
	out, err := recordPaymentTx(tx, paymentInput{
		InvoiceID: req.InvoiceID, Username: strings.TrimSpace(req.Username), Amount: req.Amount,
		Currency: req.Currency, PaymentMethod: req.PaymentMethod, Gateway: req.Gateway,
		GatewayRef: req.GatewayRef, Notes: req.Notes, ProcessedBy: &claims.UserID,
	})
	if err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to record payment")
		return
	}

	payload := gin.H{"payment_id": out.PaymentID, "username": out.Username, "amount": req.Amount,
		"currency": req.Currency, "receipt": out.Receipt, "invoice_id": req.InvoiceID}
	go h.dispatchWebhook("payment.received", payload)
	msg := "payment recorded"
	if out.InvoicePaid {
		go h.dispatchWebhook("invoice.paid", payload)
		msg = "payment recorded; invoice paid in full"
		if out.PeriodGranted && out.NewExpiry != nil {
			msg += fmt.Sprintf("; %s is paid until %s", out.Username, out.NewExpiry.Format("2006-01-02"))
		}
	} else if req.InvoiceID != nil {
		msg = "partial payment recorded; invoice still open"
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":             out.PaymentID,
		"receipt_number": out.Receipt,
		"invoice_paid":   out.InvoicePaid,
		"account_expiry": out.NewExpiry,
		"message":        msg,
	})
}

// ReversePayment marks a payment reversed (refund, bounced transfer, entry error). The row
// is kept; its invoice re-opens and the plan period it granted is taken back.
// DELETE /payments/:id is an alias kept for older clients.
func (h *Handler) ReversePayment(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	c.ShouldBindJSON(&req)
	claims, _ := middleware.GetClaims(c)

	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback()
	msg, err := reversePaymentTx(tx, id, &claims.UserID, req.Reason)
	if err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to reverse payment")
		return
	}
	go h.dispatchWebhook("payment.reversed", gin.H{"payment_id": id, "reason": req.Reason})
	c.JSON(http.StatusOK, gin.H{"message": msg})
}

// GetPaymentReceipt returns an HTML receipt. The UI fetches it with the Authorization
// header and opens the result, so no token ever appears in a URL.
func (h *Handler) GetPaymentReceipt(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}

	var p Payment
	err = scanPayment(h.db.QueryRow(fmt.Sprintf(`SELECT %s FROM payments p
		LEFT JOIN invoices i ON i.id = p.invoice_id
		LEFT JOIN app_users u ON u.id = p.processed_by
		WHERE p.id=$1`, paymentColumns), id).Scan, &p)
	if err != nil {
		respondError(c, http.StatusNotFound, "payment not found")
		return
	}

	var orgName, orgEmail string
	h.db.QueryRow(`SELECT COALESCE(value,'') FROM system_settings WHERE key='org_name'`).Scan(&orgName)
	h.db.QueryRow(`SELECT COALESCE(value,'') FROM system_settings WHERE key='org_email'`).Scan(&orgEmail)
	if orgName == "" {
		orgName = "Internet Service"
	}

	html, err := buildReceiptHTML(p, orgName, orgEmail)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to render receipt")
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
	c.String(http.StatusOK, html)
}

// PaymentSummary returns revenue analytics, always split by currency.
func (h *Handler) PaymentSummary(c *gin.Context) {
	type PeriodStat struct {
		Period   string  `json:"period"`
		Currency string  `json:"currency"`
		Total    float64 `json:"total"`
		Count    int     `json:"count"`
	}
	monthly := []PeriodStat{}
	rows, err := h.db.Query(`
		SELECT TO_CHAR(created_at,'YYYY-MM') AS period, currency, SUM(amount), COUNT(*)
		FROM payments
		WHERE status='confirmed' AND created_at >= DATE_TRUNC('month', NOW()) - INTERVAL '11 months'
		GROUP BY period, currency ORDER BY period, currency`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var s PeriodStat
			if rows.Scan(&s.Period, &s.Currency, &s.Total, &s.Count) == nil {
				monthly = append(monthly, s)
			}
		}
	}

	type MethodStat struct {
		Method   string  `json:"method"`
		Currency string  `json:"currency"`
		Total    float64 `json:"total"`
		Count    int     `json:"count"`
	}
	methods := []MethodStat{}
	mrows, err := h.db.Query(`
		SELECT payment_method, currency, SUM(amount), COUNT(*)
		FROM payments WHERE status='confirmed'
		GROUP BY payment_method, currency ORDER BY SUM(amount) DESC`)
	if err == nil {
		defer mrows.Close()
		for mrows.Next() {
			var s MethodStat
			if mrows.Scan(&s.Method, &s.Currency, &s.Total, &s.Count) == nil {
				methods = append(methods, s)
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"monthly":     monthly,
		"by_method":   methods,
		"all_time":    currencyTotals(h, `SELECT currency, SUM(amount), COUNT(*) FROM payments WHERE status='confirmed' GROUP BY currency ORDER BY currency`),
		"this_month":  currencyTotals(h, `SELECT currency, SUM(amount), COUNT(*) FROM payments WHERE status='confirmed' AND created_at >= DATE_TRUNC('month',NOW()) GROUP BY currency ORDER BY currency`),
		"outstanding": currencyTotals(h, `SELECT i.currency, SUM(i.amount - COALESCE((SELECT SUM(p.amount) FROM payments p WHERE p.invoice_id=i.id AND p.status='confirmed'),0)), COUNT(*) FROM invoices i WHERE i.status IN ('pending','overdue') GROUP BY i.currency ORDER BY i.currency`),
	})
}

var receiptTemplate = template.Must(template.New("receipt").Parse(`<!DOCTYPE html>
<html><head><meta charset="UTF-8">
<title>Receipt {{.Receipt}}</title>
<style>
  body{font-family:-apple-system,sans-serif;max-width:480px;margin:40px auto;padding:20px;color:#1e293b}
  .header{text-align:center;padding:24px 0;border-bottom:2px solid #e2e8f0;margin-bottom:24px}
  .logo{font-size:24px;font-weight:800;color:#3b82f6}
  .receipt-no{font-size:13px;color:#64748b;margin-top:4px}
  table{width:100%;border-collapse:collapse}
  td{padding:10px 4px;border-bottom:1px solid #f1f5f9;font-size:14px}
  td.v{text-align:right}
  .total{font-size:20px;font-weight:700;color:#16a34a;text-align:right}
  .footer{text-align:center;margin-top:32px;padding-top:16px;border-top:1px solid #e2e8f0;color:#94a3b8;font-size:12px}
  .badge{display:inline-block;padding:4px 12px;border-radius:20px;font-size:12px;font-weight:600}
  .ok{background:#dcfce7;color:#16a34a}.rev{background:#fee2e2;color:#dc2626}
  @media print{body{margin:0}button{display:none}}
</style>
</head><body>
<div class="header">
  <div class="logo">{{.Org}}</div>
  <div class="receipt-no">PAYMENT RECEIPT</div>
  {{if .OrgEmail}}<div style="font-size:13px;color:#94a3b8;margin-top:4px">{{.OrgEmail}}</div>{{end}}
</div>
<table>
  <tr><td style="color:#6b7280;">Receipt No.</td><td class="v" style="font-weight:700;font-family:monospace">{{.Receipt}}</td></tr>
  <tr><td style="color:#6b7280;">Date</td><td class="v">{{.Date}}</td></tr>
  <tr><td style="color:#6b7280;">Customer</td><td class="v">{{.Customer}}</td></tr>
  <tr><td style="color:#6b7280;">Invoice</td><td class="v">{{.Invoice}}</td></tr>
  <tr><td style="color:#6b7280;">Payment Method</td><td class="v">{{.Method}}</td></tr>
  {{if .Reference}}<tr><td style="color:#6b7280;">Reference</td><td class="v">{{.Reference}}</td></tr>{{end}}
  {{if .Notes}}<tr><td style="color:#6b7280;">Notes</td><td class="v">{{.Notes}}</td></tr>{{end}}
  <tr><td style="font-weight:600;padding-top:16px;">Amount Paid</td><td class="total">{{.Currency}} {{.Amount}}</td></tr>
</table>
<div style="text-align:center;margin-top:24px;">
  {{if .Reversed}}<span class="badge rev">REVERSED {{.ReversedAt}}</span>{{else}}<span class="badge ok">✓ Payment Confirmed</span>{{end}}
</div>
<div class="footer">
  <p>{{.Org}}</p>
  <p style="margin-top:4px">Thank you for your payment!</p>
  <p style="margin-top:8px"><button onclick="window.print()" style="background:#3b82f6;color:#fff;border:none;padding:8px 20px;border-radius:8px;cursor:pointer;">Print</button></p>
</div>
</body></html>`))

// buildReceiptHTML renders the receipt; html/template escapes every field.
func buildReceiptHTML(p Payment, orgName, orgEmail string) (string, error) {
	deref := func(s *string, def string) string {
		if s == nil || *s == "" {
			return def
		}
		return *s
	}
	method := strings.ReplaceAll(p.PaymentMethod, "_", " ")
	if method != "" {
		method = strings.ToUpper(method[:1]) + method[1:]
	}
	data := map[string]interface{}{
		"Org": orgName, "OrgEmail": orgEmail, "Receipt": p.ReceiptNumber,
		"Date": p.CreatedAt.Format("Jan 2, 2006 15:04"), "Customer": deref(p.Username, "—"),
		"Invoice": deref(p.InvoiceNumber, "—"), "Method": method, "Reference": deref(p.GatewayRef, ""),
		"Notes": deref(p.Notes, ""), "Currency": p.Currency, "Amount": fmt.Sprintf("%.2f", p.Amount),
		"Reversed": p.Status == "reversed", "ReversedAt": "",
	}
	if p.ReversedAt != nil {
		data["ReversedAt"] = p.ReversedAt.Format("Jan 2, 2006")
	}
	var buf bytes.Buffer
	if err := receiptTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
