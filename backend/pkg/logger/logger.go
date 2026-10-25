package logger

import (
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// New creates a new configured logger instance.
func New() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(os.Stdout)

	if os.Getenv("APP_ENV") == "production" {
		log.SetFormatter(&logrus.JSONFormatter{
			TimestampFormat: time.RFC3339,
		})
		log.SetLevel(logrus.InfoLevel)
	} else {
		log.SetFormatter(&logrus.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: "2006-01-02 15:04:05",
			ForceColors:     true,
		})
		log.SetLevel(logrus.DebugLevel)
	}

	return log
}

// GinLogger returns a Gin middleware that logs HTTP requests using logrus.
func GinLogger(log *logrus.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		c.Next()

		// Secrets in the path (the M-Pesa callback addresses end in one) are masked too:
		// anyone who reads the log could otherwise post fake payment confirmations.
		for _, p := range c.Params {
			switch p.Key {
			case "token", "secret", "key":
				if p.Value != "" {
					path = strings.Replace(path, "/"+p.Value, "/REDACTED", 1)
				}
			}
		}

		latency := time.Since(start)
		statusCode := c.Writer.Status()
		clientIP := c.ClientIP()
		method := c.Request.Method

		// Query strings are logged with credential-looking parameters masked. The API no
		// longer accepts ?token=, but an old client or a typo should not leak one into logs.
		if raw != "" {
			if q, err := url.ParseQuery(raw); err == nil {
				for k := range q {
					switch strings.ToLower(k) {
					case "token", "access_token", "refresh_token", "key", "api_key", "password", "secret":
						q.Set(k, "REDACTED")
					}
				}
				raw = q.Encode()
			}
			path = path + "?" + raw
		}

		entry := log.WithFields(logrus.Fields{
			"status":    statusCode,
			"method":    method,
			"path":      path,
			"ip":        clientIP,
			"latency":   latency.String(),
			"user_agent": c.Request.UserAgent(),
		})

		if statusCode >= 500 {
			entry.Error("Server error")
		} else if statusCode >= 400 {
			entry.Warn("Client error")
		} else {
			entry.Info("Request handled")
		}
	}
}
