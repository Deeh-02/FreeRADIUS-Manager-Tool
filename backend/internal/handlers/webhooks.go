package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"

	"github.com/freeradius-manager/backend/internal/database"
	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────────
// Webhooks
// ─────────────────────────────────────────────────────────────────────────────
//
// Events sent (an empty event list on a webhook means "all"):
//   user.created, user.suspended, user.expired, invoice.created, invoice.paid,
//   payment.received, payment.reversed, ticket.created, nas.down, nas.up, test
// Each delivery is a signed JSON POST (X-RADIUS-Signature: sha256=<hmac of the body>),
// retried up to 3 times with back-off; every attempt is logged in webhook_logs.

// WebhookEvents is the list the UI offers.
var WebhookEvents = []string{"user.created", "user.suspended", "user.expired", "invoice.created",
	"invoice.paid", "payment.received", "payment.reversed", "ticket.created", "nas.down", "nas.up", "test"}

type Webhook struct {
	ID            int        `json:"id"`
	Name          string     `json:"name"`
	URL           string     `json:"url"`
	Secret        *string    `json:"secret,omitempty"`
	HasSecret     bool       `json:"has_secret"`
	Events        []string   `json:"events"`
	IsActive      bool       `json:"is_active"`
	LastTriggered *time.Time `json:"last_triggered"`
	FailCount     int        `json:"fail_count"`
	CreatedAt     time.Time  `json:"created_at"`
}

func validEvents(events []string) error {
	known := map[string]bool{}
	for _, e := range WebhookEvents {
		known[e] = true
	}
	for _, e := range events {
		if !known[e] {
			return fmt.Errorf("unknown event %q", e)
		}
	}
	return nil
}

// ListWebhooks returns all webhook configurations (secrets are never returned).
func (h *Handler) ListWebhooks(c *gin.Context) {
	rows, err := h.db.Query(`SELECT id, name, url, secret IS NOT NULL AND secret <> '', events,
		is_active, last_triggered, fail_count, created_at
		FROM webhooks ORDER BY is_active DESC, name`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch webhooks")
		return
	}
	defer rows.Close()

	hooks := []Webhook{}
	for rows.Next() {
		var wh Webhook
		var eventsRaw []byte
		if err := rows.Scan(&wh.ID, &wh.Name, &wh.URL, &wh.HasSecret, &eventsRaw,
			&wh.IsActive, &wh.LastTriggered, &wh.FailCount, &wh.CreatedAt); err != nil {
			continue
		}
		json.Unmarshal(eventsRaw, &wh.Events)
		if wh.Events == nil {
			wh.Events = []string{}
		}
		hooks = append(hooks, wh)
	}
	c.JSON(http.StatusOK, gin.H{"data": hooks, "events": WebhookEvents})
}

// CreateWebhook registers a new webhook.
func (h *Handler) CreateWebhook(c *gin.Context) {
	var req struct {
		Name     string   `json:"name" binding:"required,min=2"`
		URL      string   `json:"url" binding:"required,url"`
		Secret   string   `json:"secret"`
		Events   []string `json:"events"`
		IsActive *bool    `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := checkWebhookURL(req.URL); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := validEvents(req.Events); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	if req.Events == nil {
		req.Events = []string{}
	}
	eventsJSON, _ := json.Marshal(req.Events)

	var id int
	if err := h.db.QueryRow(`INSERT INTO webhooks (name,url,secret,events,is_active) VALUES ($1,$2,$3,$4::jsonb,$5) RETURNING id`,
		req.Name, req.URL, nullableString(req.Secret), string(eventsJSON), isActive).Scan(&id); err != nil {
		h.log.WithError(err).Error("create webhook")
		respondError(c, http.StatusInternalServerError, "failed to create webhook")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id, "message": "webhook created"})
}

// UpdateWebhook updates a webhook's configuration. Omitted fields are unchanged; send
// "events": [] for all events.
func (h *Handler) UpdateWebhook(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name     string    `json:"name"`
		URL      string    `json:"url"`
		Secret   string    `json:"secret"`
		Events   *[]string `json:"events"`
		IsActive *bool     `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.URL != "" {
		if err := checkWebhookURL(req.URL); err != nil {
			respondError(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	var events interface{}
	if req.Events != nil {
		if err := validEvents(*req.Events); err != nil {
			respondError(c, http.StatusBadRequest, err.Error())
			return
		}
		b, _ := json.Marshal(*req.Events)
		if *req.Events == nil {
			b = []byte("[]")
		}
		events = string(b)
	}

	if _, err := h.db.Exec(`UPDATE webhooks SET
		name=COALESCE(NULLIF($1,''),name),
		url=COALESCE(NULLIF($2,''),url),
		secret=COALESCE(NULLIF($3,''),secret),
		events=COALESCE($4::jsonb, events),
		is_active=COALESCE($5, is_active),
		fail_count=CASE WHEN $5 IS TRUE THEN 0 ELSE fail_count END
		WHERE id=$6`,
		req.Name, req.URL, req.Secret, events, req.IsActive, id); err != nil {
		h.log.WithError(err).Error("update webhook")
		respondError(c, http.StatusInternalServerError, "failed to update webhook")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "webhook updated"})
}

// DeleteWebhook removes a webhook and its logs.
func (h *Handler) DeleteWebhook(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	if _, err := h.db.Exec(`DELETE FROM webhooks WHERE id=$1`, id); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete webhook")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "webhook deleted"})
}

// TestWebhook sends a test payload to the webhook URL (one attempt, no retry).
func (h *Handler) TestWebhook(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var url, secret string
	if err := h.db.QueryRow(`SELECT url, COALESCE(secret,'') FROM webhooks WHERE id=$1`, id).Scan(&url, &secret); err != nil {
		respondError(c, http.StatusNotFound, "webhook not found")
		return
	}

	payload := gin.H{"message": "This is a test delivery from RADIUS Manager"}
	statusCode, deliveryErr := deliverWebhook(url, secret, "test", payload)
	success := deliveryErr == nil
	h.db.Exec(`INSERT INTO webhook_logs (webhook_id,event,payload,status_code,success) VALUES ($1,'test',$2::jsonb,$3,$4)`,
		id, mustJSON(payload), statusCode, success)

	if !success {
		respondError(c, http.StatusBadGateway, fmt.Sprintf("delivery failed: %v", deliveryErr))
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "test delivered", "status_code": statusCode})
}

// ListWebhookLogs returns recent delivery logs for a webhook.
func (h *Handler) ListWebhookLogs(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	rows, err := h.db.Query(`SELECT id,event,status_code,success,created_at
		FROM webhook_logs WHERE webhook_id=$1 ORDER BY created_at DESC LIMIT 50`, id)
	type Log struct {
		ID         int       `json:"id"`
		Event      string    `json:"event"`
		StatusCode int       `json:"status_code"`
		Success    bool      `json:"success"`
		CreatedAt  time.Time `json:"created_at"`
	}
	logs := []Log{}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var l Log
			if rows.Scan(&l.ID, &l.Event, &l.StatusCode, &l.Success, &l.CreatedAt) == nil {
				logs = append(logs, l)
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": logs})
}

// dispatchWebhook delivers an event to all matching active webhooks (call with `go`).
func (h *Handler) dispatchWebhook(event string, payload gin.H) {
	dispatchWebhookDB(h.db, event, payload)
}

// dispatchWebhookDB is dispatchWebhook for background workers that have no Handler.
func dispatchWebhookDB(db *database.DB, event string, payload gin.H) {
	rows, err := db.Query(`SELECT id, url, COALESCE(secret,'') FROM webhooks
		WHERE is_active = TRUE AND (events = '[]'::jsonb OR events ? $1)`, event)
	if err != nil {
		return
	}
	type hook struct {
		id          int
		url, secret string
	}
	hooks := []hook{}
	for rows.Next() {
		var wh hook
		if rows.Scan(&wh.id, &wh.url, &wh.secret) == nil {
			hooks = append(hooks, wh)
		}
	}
	rows.Close()

	for _, wh := range hooks {
		go func(wh hook) {
			var statusCode int
			var deliveryErr error
			for attempt := 0; attempt < 3; attempt++ {
				if attempt > 0 {
					time.Sleep(time.Duration(attempt*attempt*5) * time.Second)
				}
				statusCode, deliveryErr = deliverWebhook(wh.url, wh.secret, event, payload)
				db.Exec(`INSERT INTO webhook_logs (webhook_id,event,payload,status_code,success) VALUES ($1,$2,$3::jsonb,$4,$5)`,
					wh.id, event, mustJSON(payload), statusCode, deliveryErr == nil)
				// 4xx other than 408/429 will not get better by retrying.
				if deliveryErr == nil || (statusCode >= 400 && statusCode < 500 && statusCode != 408 && statusCode != 429) {
					break
				}
			}
			db.Exec(`UPDATE webhooks SET last_triggered=NOW(),
				fail_count=CASE WHEN $1 THEN 0 ELSE fail_count+1 END WHERE id=$2`,
				deliveryErr == nil, wh.id)
		}(wh)
	}
}

// checkWebhookURL refuses non-http(s) URLs and, unless WEBHOOK_ALLOW_PRIVATE=true, hosts that
// resolve to loopback, private or link-local addresses (so a webhook cannot be used to probe
// the internal network or cloud metadata endpoints). The same check runs again at dial time.
func checkWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("webhook URL must be an http(s) URL")
	}
	if os.Getenv("WEBHOOK_ALLOW_PRIVATE") == "true" {
		return nil
	}
	ips, err := net.LookupIP(u.Hostname())
	if err != nil {
		return fmt.Errorf("cannot resolve %s", u.Hostname())
	}
	for _, ip := range ips {
		if isInternalIP(ip) {
			return fmt.Errorf("%s resolves to an internal address; set WEBHOOK_ALLOW_PRIVATE=true to allow it", u.Hostname())
		}
	}
	return nil
}

func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

var webhookClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
			// Re-checked on the resolved address actually dialled (defeats DNS rebinding).
			Control: func(network, address string, _ syscall.RawConn) error {
				if os.Getenv("WEBHOOK_ALLOW_PRIVATE") == "true" {
					return nil
				}
				host, _, _ := net.SplitHostPort(address)
				if ip := net.ParseIP(host); ip != nil && isInternalIP(ip) {
					return errors.New("webhook destination is an internal address")
				}
				return nil
			},
		}).DialContext,
		MaxIdleConns:    10,
		IdleConnTimeout: 30 * time.Second,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// deliverWebhook sends a signed JSON POST to the given URL.
// The body is signed with HMAC-SHA256 of the raw JSON using the webhook's secret and sent
// as X-RADIUS-Signature: sha256=<hex>; receivers should recompute it and also reject old
// "timestamp" values (it is inside the signed body).
func deliverWebhook(target, secret, event string, payload interface{}) (int, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"event":     event,
		"timestamp": time.Now().Unix(),
		"data":      payload,
	})

	req, err := http.NewRequestWithContext(context.Background(), "POST", target, bytes.NewBuffer(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "RADIUS-Manager-Webhook/"+AppVersion)
	req.Header.Set("X-RADIUS-Event", event)

	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-RADIUS-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := webhookClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
