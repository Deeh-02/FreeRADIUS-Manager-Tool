package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

// APIKey represents an external integration key.
type APIKey struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	KeyPrefix   string     `json:"key_prefix"`
	Permissions []string   `json:"permissions"`
	CreatedBy   *string    `json:"created_by"`
	LastUsed    *time.Time `json:"last_used"`
	ExpiresAt   *time.Time `json:"expires_at"`
	IsActive    bool       `json:"is_active"`
	CreatedAt   time.Time  `json:"created_at"`
}

// ListAPIKeys returns all API keys (never returns the full key).
func (h *Handler) ListAPIKeys(c *gin.Context) {
	rows, err := h.db.Query(`
		SELECT ak.id, ak.name, ak.key_prefix, ak.permissions, au.username,
		       ak.last_used, ak.expires_at, ak.is_active, ak.created_at
		FROM api_keys ak
		LEFT JOIN app_users au ON au.id = ak.created_by
		ORDER BY ak.created_at DESC`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch API keys")
		return
	}
	defer rows.Close()

	keys := []APIKey{}
	for rows.Next() {
		var k APIKey
		if rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, pq.Array(&k.Permissions),
			&k.CreatedBy, &k.LastUsed, &k.ExpiresAt, &k.IsActive, &k.CreatedAt) == nil {
			keys = append(keys, k)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": keys})
}

// CreateAPIKey generates a new API key. Returns the full key ONCE.
func (h *Handler) CreateAPIKey(c *gin.Context) {
	var req struct {
		Name        string   `json:"name" binding:"required,min=2,max=100"`
		Permissions []string `json:"permissions"`
		ExpiresAt   *string  `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Permissions) == 0 {
		req.Permissions = []string{"read"}
	}
	for _, p := range req.Permissions {
		if p != "read" && p != "write" {
			respondError(c, http.StatusBadRequest, "permissions may only be read and write")
			return
		}
	}

	claims, _ := middleware.GetClaims(c)

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to generate key")
		return
	}
	fullKey := "rmk_" + hex.EncodeToString(raw) // prefix + 64 hex chars

	hash := sha256.Sum256([]byte(fullKey))
	keyHash := hex.EncodeToString(hash[:])
	keyPrefix := fullKey[:12] // "rmk_" + first 8 hex chars

	var expiresAt interface{}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse("2006-01-02", (*req.ExpiresAt)[:min(10, len(*req.ExpiresAt))])
		if err != nil {
			respondError(c, http.StatusBadRequest, "expires_at must be YYYY-MM-DD")
			return
		}
		expiresAt = t
	}

	var id int
	err := h.db.QueryRow(`
		INSERT INTO api_keys (name, key_hash, key_prefix, permissions, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		req.Name, keyHash, keyPrefix, pq.Array(req.Permissions), claims.UserID, expiresAt,
	).Scan(&id)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to create API key")
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":          id,
		"name":        req.Name,
		"key":         fullKey, // shown ONCE, never again
		"key_prefix":  keyPrefix,
		"permissions": req.Permissions,
		"message":     "API key created — copy it now, it will not be shown again",
	})
}

// RevokeAPIKey marks a key as inactive.
func (h *Handler) RevokeAPIKey(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	res, err := h.db.Exec(`UPDATE api_keys SET is_active=false WHERE id=$1`, id)
	if !writeOK(c, res, err, "API key") {
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "API key revoked"})
}

// DeleteAPIKey permanently removes a key.
func (h *Handler) DeleteAPIKey(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	res, err := h.db.Exec(`DELETE FROM api_keys WHERE id=$1`, id)
	if !writeOK(c, res, err, "API key") {
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "API key deleted"})
}

// Keys are checked by middleware.RequireAuth ("Authorization: ApiKey <key>"): "read" keys
// may only GET, "write" keys act with the admin role. Keys act as the admin who created
// them and stop working if that account is disabled.

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// APIKeyStats returns usage statistics.
func (h *Handler) APIKeyStats(c *gin.Context) {
	var total, active, expired int
	h.db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE is_active),
		COUNT(*) FILTER (WHERE expires_at < NOW()) FROM api_keys`).
		Scan(&total, &active, &expired)
	c.JSON(http.StatusOK, gin.H{
		"total": total, "active": active, "expired": expired,
		"docs_url": fmt.Sprintf("%s/api/v1", c.Request.Host),
		"usage":    "send the header  Authorization: ApiKey <key>",
	})
}
