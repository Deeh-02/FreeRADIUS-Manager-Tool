package handlers

import (
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/freeradius-manager/backend/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// AlertRule holds an alert notification rule.
type AlertRule struct {
	ID              int        `json:"id"`
	Name            string     `json:"name"`
	EventType       string     `json:"event_type"`
	NotifyEmail     bool       `json:"notify_email"`
	EmailAddress    *string    `json:"email_address"`
	IsActive        bool       `json:"is_active"`
	Threshold       int        `json:"threshold"`
	CooldownMinutes int        `json:"cooldown_minutes"`
	LastTriggered   *time.Time `json:"last_triggered"`
	CreatedAt       time.Time  `json:"created_at"`
}

// alertEvents are the conditions checkAlerts evaluates (every 5 minutes).
var alertEvents = map[string]string{
	"data_limit_80pct":  "a customer has used 80% of their data cap this period",
	"data_limit_100pct": "a customer has reached their data cap",
	"account_expiry_3d": "customers expire within 3 days",
	"account_expiry_7d": "customers expire within 7 days",
	"login_failure":     "failed RADIUS logins in 5 minutes reach the threshold (default 20)",
	"nas_down":          "a NAS stops answering the monitor",
	"new_user":          "customers were created in the last 5 minutes",
	"invoice_overdue":   "invoices are overdue",
	"backup_failed":     "the last database backup failed",
}

// ListAlertRules returns all alert rules.
func (h *Handler) ListAlertRules(c *gin.Context) {
	rows, err := h.db.Query(`
		SELECT id, name, event_type, notify_email, email_address, is_active, threshold, cooldown_minutes,
		       last_triggered, created_at
		FROM alert_rules ORDER BY created_at ASC`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch alert rules")
		return
	}
	defer rows.Close()

	rules := []AlertRule{}
	for rows.Next() {
		var r AlertRule
		if rows.Scan(&r.ID, &r.Name, &r.EventType, &r.NotifyEmail, &r.EmailAddress, &r.IsActive,
			&r.Threshold, &r.CooldownMinutes, &r.LastTriggered, &r.CreatedAt) == nil {
			rules = append(rules, r)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"data":            rules,
		"events":          alertEvents,
		"smtp_configured": os.Getenv("SMTP_HOST") != "",
		"smtp_from":       os.Getenv("SMTP_FROM"),
	})
}

// CreateAlertRule creates a new alert rule.
func (h *Handler) CreateAlertRule(c *gin.Context) {
	var req struct {
		Name            string `json:"name" binding:"required"`
		EventType       string `json:"event_type" binding:"required"`
		NotifyEmail     bool   `json:"notify_email"`
		EmailAddress    string `json:"email_address" binding:"omitempty,email"`
		IsActive        *bool  `json:"is_active"`
		Threshold       int    `json:"threshold" binding:"omitempty,min=0"`
		CooldownMinutes *int   `json:"cooldown_minutes" binding:"omitempty,min=0"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := alertEvents[req.EventType]; !ok {
		respondError(c, http.StatusBadRequest, "unknown event_type")
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	cooldown := 360
	if req.CooldownMinutes != nil {
		cooldown = *req.CooldownMinutes
	}
	var id int
	err := h.db.QueryRow(`
		INSERT INTO alert_rules (name,event_type,notify_email,email_address,is_active,threshold,cooldown_minutes)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		req.Name, req.EventType, req.NotifyEmail, nullableString(req.EmailAddress), isActive, req.Threshold, cooldown,
	).Scan(&id)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to create alert rule")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id, "message": "alert rule created"})
}

// UpdateAlertRule updates an existing rule.
func (h *Handler) UpdateAlertRule(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name            string  `json:"name"`
		NotifyEmail     *bool   `json:"notify_email"`
		EmailAddress    *string `json:"email_address"`
		IsActive        *bool   `json:"is_active"`
		Threshold       *int    `json:"threshold" binding:"omitempty,min=0"`
		CooldownMinutes *int    `json:"cooldown_minutes" binding:"omitempty,min=0"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.EmailAddress != nil && !validEmail(*req.EmailAddress) {
		respondError(c, http.StatusBadRequest, "invalid email address")
		return
	}
	// Only fields present in the request change (toggling Pause/Enable sends is_active only).
	if _, err := h.db.Exec(`UPDATE alert_rules SET name=COALESCE(NULLIF($1,''),name),
		notify_email=COALESCE($2,notify_email),
		email_address=CASE WHEN $3::text IS NULL THEN email_address ELSE NULLIF($3,'') END,
		is_active=COALESCE($4,is_active),
		threshold=COALESCE($5,threshold), cooldown_minutes=COALESCE($6,cooldown_minutes)
		WHERE id=$7`,
		req.Name, req.NotifyEmail, req.EmailAddress, req.IsActive, req.Threshold, req.CooldownMinutes, id); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update alert rule")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "alert rule updated"})
}

// DeleteAlertRule removes a rule.
func (h *Handler) DeleteAlertRule(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	res, err := h.db.Exec(`DELETE FROM alert_rules WHERE id=$1`, id)
	if !writeOK(c, res, err, "alert rule") {
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "alert rule deleted"})
}

// SendTestEmail sends a test email to verify SMTP config.
func (h *Handler) SendTestEmail(c *gin.Context) {
	var req struct {
		To string `json:"to" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if !validEmail(req.To) {
		respondError(c, http.StatusBadRequest, "invalid email address")
		return
	}
	err := sendEmail(req.To, "RADIUS Manager — Test Email",
		"This is a test email from RADIUS Manager.\n\nSMTP is configured correctly.")
	if err != nil {
		respondError(c, http.StatusBadRequest, "failed to send email: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "test email sent to " + req.To})
}

// sendEmail sends a plain-text email using the SMTP settings from environment. Port 465 is
// implicit TLS; any other port upgrades to STARTTLS when the server offers it. The whole
// exchange is bounded by smtpTimeout: emails are sent from the scheduler, and a server that
// never answers (465 spoken to as plain SMTP, a provider blocking outbound mail) used to
// hold it, and with it expiry, renewal invoices and backups, for as long as it liked.
func sendEmail(to, subject, body string) error {
	host := os.Getenv("SMTP_HOST")
	if host == "" {
		return fmt.Errorf("SMTP_HOST not configured")
	}
	port := os.Getenv("SMTP_PORT")
	if port == "" {
		port = "587"
	}
	user := os.Getenv("SMTP_USER")
	pass := os.Getenv("SMTP_PASS")
	from := os.Getenv("SMTP_FROM")
	if from == "" {
		from = "noreply@radius-manager.local"
	}
	// A newline in a header value would let the caller add headers (or recipients).
	for _, v := range []string{to, subject, from} {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("invalid characters in email header")
		}
	}

	domain := from
	if i := strings.LastIndex(from, "@"); i >= 0 {
		domain = from[i+1:]
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%d.%s>\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s",
		from, to, mime.QEncoding.Encode("utf-8", subject), time.Now().Format(time.RFC1123Z),
		time.Now().UnixNano(), domain, strings.ReplaceAll(body, "\n", "\r\n"))

	var auth smtp.Auth
	if user != "" {
		auth = smtp.PlainAuth("", user, pass, host)
	}
	return smtpSend(host, port, auth, from, to, []byte(msg))
}

var smtpTimeout = 30 * time.Second

// smtpSend is smtp.SendMail with a connect timeout, a deadline for the whole conversation and
// implicit TLS on port 465.
func smtpSend(host, port string, auth smtp.Auth, from, to string, msg []byte) error {
	addr := net.JoinHostPort(host, port)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tlsConf := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	var err error
	if port == "465" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConf)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(smtpTimeout))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if port != "465" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsConf); err != nil {
				return err
			}
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return fmt.Errorf("smtp: server doesn't support AUTH")
		}
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// StartAlertWorker launches a background goroutine that checks alert conditions
// every 5 minutes and sends email notifications.
func StartAlertWorker(db *database.DB, log *logrus.Logger) {
	go func() {
		log.Info("Alert worker started — checking every 5 minutes")
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		time.Sleep(1 * time.Minute)
		checkAlerts(db, log)

		for range ticker.C {
			checkAlerts(db, log)
		}
	}()
}

// checkAlerts evaluates each active rule and emails the result to the rule's address (or
// SMTP_FROM if none). Alerts go to staff, not to customers. A rule that fired is silent for
// its cooldown_minutes (default 6 hours) even if the condition stays true. The whole worker
// is a no-op when SMTP_HOST is empty.
func checkAlerts(db *database.DB, log *logrus.Logger) {
	if os.Getenv("SMTP_HOST") == "" {
		return
	}
	adminEmail := os.Getenv("SMTP_FROM")

	rows, err := db.Query(`SELECT id, event_type, COALESCE(email_address,''), name, threshold
		FROM alert_rules
		WHERE is_active = TRUE AND notify_email = TRUE
		  AND (last_triggered IS NULL OR last_triggered < NOW() - make_interval(mins => cooldown_minutes))`)
	if err != nil {
		return
	}
	type rule struct {
		id        int
		event     string
		email     string
		name      string
		threshold int
	}
	rules := []rule{}
	for rows.Next() {
		var r rule
		if rows.Scan(&r.id, &r.event, &r.email, &r.name, &r.threshold) == nil {
			if r.email == "" {
				r.email = adminEmail
			}
			rules = append(rules, r)
		}
	}
	rows.Close()

	listUsers := func(query string, args ...interface{}) []string {
		var out []string
		rs, err := db.Query(query, args...)
		if err != nil {
			return out
		}
		defer rs.Close()
		for rs.Next() {
			var u string
			if rs.Scan(&u) == nil {
				out = append(out, u)
			}
		}
		return out
	}
	// Usage this period vs. the Max-Octets cap, same rule as the datacounter module.
	overCap := func(pct int) []string {
		return listUsers(`
			SELECT rc.username FROM radcheck rc
			LEFT JOIN radius_users ru ON ru.username = rc.username
			WHERE rc.attribute = 'Max-Octets' AND rc.value ~ '^[0-9]+$'
			  AND (SELECT COALESCE(SUM(acctinputoctets + acctoutputoctets), 0) FROM radacct ra
			       WHERE ra.username = rc.username
			         AND ra.radacctid > COALESCE(ru.period_acct_floor, 0))
			      >= rc.value::numeric * $1 / 100
			ORDER BY rc.username LIMIT 100`, pct)
	}

	for _, r := range rules {
		var details string
		var count int
		switch r.event {
		case "account_expiry_3d", "account_expiry_7d":
			days := 3
			if r.event == "account_expiry_7d" {
				days = 7
			}
			db.QueryRow(`SELECT COUNT(*) FROM radius_users
				WHERE status='active' AND account_expiry BETWEEN CURRENT_DATE AND CURRENT_DATE + $1::int`, days).Scan(&count)
			if count > 0 {
				details = fmt.Sprintf("%d customer account(s) expire within %d days.", count, days)
			}
		case "nas_down":
			down := listUsers(`SELECT COALESCE(NULLIF(shortname,''), nasname) FROM nas WHERE status='active' AND ping_status='down'`)
			if len(down) > 0 {
				details = fmt.Sprintf("%d NAS device(s) are not answering: %s", len(down), strings.Join(down, ", "))
			}
		case "data_limit_100pct", "data_limit_80pct":
			pct := 100
			if r.event == "data_limit_80pct" {
				pct = 80
			}
			if users := overCap(pct); len(users) > 0 {
				details = fmt.Sprintf("%d customer(s) at or above %d%% of their data cap: %s", len(users), pct, strings.Join(users, ", "))
			}
		case "login_failure":
			threshold := r.threshold
			if threshold <= 0 {
				threshold = 20
			}
			db.QueryRow(`SELECT COUNT(*) FROM radpostauth WHERE reply = 'Access-Reject'
				AND authdate >= NOW() - INTERVAL '5 minutes'`).Scan(&count)
			if count >= threshold {
				details = fmt.Sprintf("%d failed RADIUS logins in the last 5 minutes (threshold %d).", count, threshold)
			}
		case "new_user":
			if users := listUsers(`SELECT username FROM radius_users WHERE created_at >= NOW() - INTERVAL '5 minutes' ORDER BY username`); len(users) > 0 {
				details = fmt.Sprintf("New customer(s): %s", strings.Join(users, ", "))
			}
		case "invoice_overdue":
			db.QueryRow(`SELECT COUNT(*) FROM invoices WHERE status = 'overdue'`).Scan(&count)
			if count > 0 {
				details = fmt.Sprintf("%d invoice(s) are overdue.", count)
			}
		case "backup_failed":
			backupMu.Lock()
			e := lastBackupErr
			backupMu.Unlock()
			if e != "" {
				details = "The last database backup failed: " + e
			}
		}

		if details == "" || r.email == "" {
			continue
		}
		err := sendEmail(r.email,
			fmt.Sprintf("[RADIUS Manager Alert] %s", r.name),
			fmt.Sprintf("Alert: %s\n\n%s\n\nTime: %s", r.name, details, time.Now().Format(time.RFC1123)))
		if err != nil {
			log.WithError(err).Warnf("failed to send alert email for rule %s", r.name)
			continue
		}
		db.Exec(`UPDATE alert_rules SET last_triggered=NOW() WHERE id=$1`, r.id)
		log.Infof("Alert sent: %s → %s", r.name, r.email)
	}
}
