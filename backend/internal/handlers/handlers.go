package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/freeradius-manager/backend/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	db  *database.DB
	log *logrus.Logger
}

// New creates a Handler with injected dependencies.
func New(db *database.DB, log *logrus.Logger) *Handler {
	return &Handler{db: db, log: log}
}

// AppVersion is the single version string (API, setup wizard, webhooks, UI footer).
const AppVersion = "1.1.0"

// HealthCheck returns service health status. The database decides the HTTP status (503 when
// it is down, which is what the container healthcheck acts on); FreeRADIUS is probed with
// Status-Server and reported, but a RADIUS outage does not fail the API's health. Pass
// ?full=1 to include the RADIUS probe (it adds up to 2s).
func HealthCheck(db *database.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		services := gin.H{}
		overall := "ok"
		code := http.StatusOK
		if err := db.Ping(); err != nil {
			services["database"] = "error: " + err.Error()
			overall, code = "degraded", http.StatusServiceUnavailable
		} else {
			services["database"] = "ok"
		}
		if c.Query("full") != "" {
			if err := radiusAlive(); err != nil {
				services["radius"] = "error: " + err.Error()
				overall = "degraded"
			} else {
				services["radius"] = "ok"
			}
		}

		c.JSON(code, gin.H{
			"status":    overall,
			"version":   AppVersion,
			"timestamp": time.Now().UTC(),
			"services":  services,
		})
	}
}

// Version returns the application version.
func Version() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"version": AppVersion,
			"name":    "FreeRADIUS Manager",
		})
	}
}

// respondError sends a JSON error response.
func respondError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"error": message})
}

// paginationParams extracts page and limit from query string.
// Defaults to page 1 / 20 rows; limit is capped at 100 and silently falls back to 20 if
// larger. Many list endpoints also run a separate COUNT(*) and ignore scan errors, so a
// failing query shows up as an empty page, not an error.
func paginationParams(c *gin.Context) (offset, limit int) {
	page := 1
	limit = 20

	if p := c.Query("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}
	if l := c.Query("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 100 {
			limit = v
		}
	}

	offset = (page - 1) * limit
	return
}

// mustInt is a helper to parse URL param :id.
func mustInt(c *gin.Context, param string) (int, error) {
	raw := c.Param(param)
	return strconv.Atoi(raw)
}

