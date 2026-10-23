package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// SendSMS sends a text message via the configured SMS gateway.
func (h *Handler) SendSMS(c *gin.Context) {
	var req struct {
		To       string `json:"to" binding:"required"`
		Message  string `json:"message" binding:"required"`
		Username string `json:"username"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	if !phoneRe.MatchString(req.To) {
		respondError(c, http.StatusBadRequest, "phone number must be digits, optionally starting with +")
		return
	}

	ref, err := sendSMSGateway(req.To, req.Message)
	status := "sent"
	if err != nil {
		status = "failed"
	}

	logSMS(h.db, req.To, req.Message, status, ref, req.Username)

	if err != nil {
		respondError(c, http.StatusBadRequest, "SMS gateway error: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":     "SMS sent",
		"to":          req.To,
		"gateway_ref": ref,
	})
}

// ListSMSLogs returns SMS send history.
func (h *Handler) ListSMSLogs(c *gin.Context) {
	offset, limit := paginationParams(c)
	rows, err := h.db.Query(`
		SELECT id, recipient, LEFT(message,160), status, gateway_ref, username, created_at
		FROM sms_logs ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch SMS logs")
		return
	}
	defer rows.Close()

	type SMSLog struct {
		ID         int        `json:"id"`
		Recipient  string     `json:"recipient"`
		Message    string     `json:"message"`
		Status     string     `json:"status"`
		GatewayRef *string    `json:"gateway_ref"`
		Username   *string    `json:"username"`
		CreatedAt  time.Time  `json:"created_at"`
	}
	logs := []SMSLog{}
	for rows.Next() {
		var l SMSLog
		rows.Scan(&l.ID, &l.Recipient, &l.Message, &l.Status, &l.GatewayRef, &l.Username, &l.CreatedAt)
		logs = append(logs, l)
	}
	var total int
	h.db.QueryRow(`SELECT COUNT(*) FROM sms_logs`).Scan(&total)
	c.JSON(http.StatusOK, gin.H{"data": logs, "total": total})
}

// NotifyUserExpiry sends SMS to users expiring within `days` days (if phone set), on demand.
// The expiry_reminders scheduled task does this automatically, once per paid period;
// pressing this button texts the same customers again.
func (h *Handler) NotifyUserExpiry(c *gin.Context) {
	var req struct {
		Days    int    `json:"days"`
		Message string `json:"message"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.Days == 0 {
		req.Days = 3
	}
	if req.Days < 0 || req.Days > 366 {
		respondError(c, http.StatusBadRequest, "days must be between 1 and 366")
		return
	}
	if req.Message == "" {
		req.Message = "Dear {username}, your account expires on {expiry}. Please renew to continue service."
	}

	rows, err := h.db.Query(`
		SELECT username, COALESCE(phone,''), COALESCE(account_expiry::text,'')
		FROM radius_users
		WHERE status='active'
		  AND phone IS NOT NULL AND phone != ''
		  AND account_expiry BETWEEN CURRENT_DATE AND CURRENT_DATE + $1::int`, req.Days)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to find expiring users")
		return
	}
	type target struct{ username, phone, expiry string }
	var targets []target
	for rows.Next() {
		var t target
		if rows.Scan(&t.username, &t.phone, &t.expiry) == nil {
			targets = append(targets, t)
		}
	}
	rows.Close()

	sent, failed := 0, 0
	for _, t := range targets {
		msg := strings.ReplaceAll(req.Message, "{username}", t.username)
		msg = strings.ReplaceAll(msg, "{expiry}", t.expiry)

		ref, err := sendSMSGateway(t.phone, msg)
		status := "sent"
		if err != nil {
			status = "failed"
			failed++
		} else {
			sent++
		}
		logSMS(h.db, t.phone, msg, status, ref, t.username)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Notifications: %d sent, %d failed", sent, failed),
		"sent":    sent,
		"failed":  failed,
	})
}

// SMSConfig returns current SMS gateway configuration status.
func (h *Handler) SMSConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"gateway":    os.Getenv("SMS_GATEWAY"),
		"configured": os.Getenv("SMS_GATEWAY") != "",
		"sender_id":  os.Getenv("SMS_SENDER_ID"),
	})
}

var phoneRe = regexp.MustCompile(`^\+?[0-9]{6,15}$`)

func logSMS(db dbtx, to, message, status, ref, username string) {
	db.Exec(`INSERT INTO sms_logs (recipient,message,status,gateway_ref,username)
		VALUES ($1,$2,$3,$4,$5)`, to, message, status, nullableString(ref), nullableString(username))
}

func smsConfigured() bool { return os.Getenv("SMS_GATEWAY") != "" }

// sendSMSGateway sends a message via an HTTP SMS gateway (Africa's Talking, Bulksms, a
// custom gateway...). Configured only through environment variables: SMS_GATEWAY (URL),
// SMS_API_KEY, SMS_SENDER_ID, SMS_BODY_TEMPLATE (with {to} {message} {api_key} {sender}),
// SMS_CONTENT_TYPE (default application/json), SMS_AUTH_HEADER (optional "Name: value",
// e.g. "apiKey: {api_key}" for Africa's Talking). Placeholder values are escaped for the
// body's content type (JSON string escaping, or URL encoding for form bodies). Any HTTP status
// below 400 counts as sent; gateway-specific error bodies are not parsed.
func sendSMSGateway(to, message string) (string, error) {
	gatewayURL := os.Getenv("SMS_GATEWAY")
	if gatewayURL == "" {
		return "", fmt.Errorf("SMS_GATEWAY not configured in .env")
	}

	apiKey := os.Getenv("SMS_API_KEY")
	senderID := os.Getenv("SMS_SENDER_ID")
	if senderID == "" {
		senderID = "RADIUS"
	}
	contentType := os.Getenv("SMS_CONTENT_TYPE")
	if contentType == "" {
		contentType = "application/json"
	}

	bodyTemplate := os.Getenv("SMS_BODY_TEMPLATE")
	if bodyTemplate == "" {
		bodyTemplate = `{"to":"{to}","message":"{message}","from":"{sender}"}`
	}

	esc := func(v string) string {
		switch {
		case strings.Contains(contentType, "json"):
			b, _ := json.Marshal(v)
			return string(b[1 : len(b)-1])
		case strings.Contains(contentType, "x-www-form-urlencoded"):
			return url.QueryEscape(v)
		}
		return v
	}
	body := strings.NewReplacer("{to}", esc(to), "{message}", esc(message),
		"{api_key}", esc(apiKey), "{sender}", esc(senderID)).Replace(bodyTemplate)

	req, err := http.NewRequest("POST", gatewayURL, bytes.NewBufferString(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if hdr := os.Getenv("SMS_AUTH_HEADER"); hdr != "" {
		if i := strings.Index(hdr, ":"); i > 0 {
			req.Header.Set(strings.TrimSpace(hdr[:i]), strings.TrimSpace(strings.ReplaceAll(hdr[i+1:], "{api_key}", apiKey)))
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gateway returned HTTP %d", resp.StatusCode)
	}

	return fmt.Sprintf("HTTP-%d", resp.StatusCode), nil
}
