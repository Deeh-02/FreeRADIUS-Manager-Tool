package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/freeradius-manager/backend/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// responseWriter wraps gin.ResponseWriter to capture the status code.
// The audit row is written after the handler runs, so it can record the outcome
// (including 4xx/5xx and role-denied attempts), not just the request.
type responseWriter struct {
	gin.ResponseWriter
	body   *bytes.Buffer
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	rw.body.Write(b)
	return rw.ResponseWriter.Write(b)
}

// AuditLogger records all write operations (POST, PUT, DELETE) to the audit_log table.
func AuditLogger(db *database.DB, log *logrus.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		method := c.Request.Method

		// Only audit mutating requests
		if method != http.MethodPost && method != http.MethodPut &&
			method != http.MethodDelete && method != http.MethodPatch {
			c.Next()
			return
		}

		// Read body for audit record
		var bodyBytes []byte
		if c.Request.Body != nil {
			bodyBytes, _ = io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		// Replace writer to capture status
		rw := &responseWriter{c.Writer, &bytes.Buffer{}, http.StatusOK}
		c.Writer = rw

		c.Next()

		claims, ok := GetClaims(c)
		if !ok {
			return
		}

		// Sanitize body: keys in sensitiveFields are redacted at any depth.
		sanitized := sanitizeBody(bodyBytes)

		action := method + " " + c.FullPath()
		if len(action) > 150 { // audit_log.action is VARCHAR(150); a lost row is worse than a cut one
			action = action[:150]
		}
		ipStr := c.ClientIP()

		details, _ := json.Marshal(map[string]interface{}{
			"method":    method,
			"path":      c.Request.URL.Path,
			"status":    rw.status,
			"body":      string(sanitized),
		})

		_, err := db.Exec(`
			INSERT INTO audit_log (user_id, action, details, ip_address, user_agent)
			VALUES ($1, $2, $3, $4::inet, $5)`,
			claims.UserID,
			action,
			string(details),
			ipStr,
			c.Request.UserAgent(),
		)
		if err != nil {
			log.WithError(err).Warn("Failed to write audit log entry")
		}
	}
}

// sensitiveFields are JSON keys whose values never reach audit_log, at any nesting depth.
var sensitiveFields = map[string]bool{
	"password": true, "new_password": true, "current_password": true, "secret": true,
	"mfa_secret": true, "mfa_code": true, "token": true, "refresh_token": true,
	"community": true, "key": true, "api_key": true, "setup_token": true,
	"default_secret": true, "smtp_pass": true,
}

// sanitizeBody removes sensitive fields from the JSON body for audit storage.
func sanitizeBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var v interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return []byte("[non-JSON body]")
	}
	out, _ := json.Marshal(redact(v))
	return out
}

func redact(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if sensitiveFields[strings.ToLower(k)] {
				t[k] = "***REDACTED***"
			} else {
				t[k] = redact(val)
			}
		}
		return t
	case []interface{}:
		for i := range t {
			t[i] = redact(t[i])
		}
		return t
	}
	return v
}
