package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────────
// RADIUS Attribute Templates
// ─────────────────────────────────────────────────────────────────────────────

type AttributeEntry struct {
	Attribute string `json:"attribute"`
	Op        string `json:"op"`
	Value     string `json:"value"`
	Table     string `json:"table"` // "check" or "reply"
}

type RadiusTemplate struct {
	ID          int              `json:"id"`
	Name        string           `json:"name"`
	Description *string          `json:"description"`
	Attributes  []AttributeEntry `json:"attributes"`
	IsActive    bool             `json:"is_active"`
	CreatedAt   time.Time        `json:"created_at"`
}

// ListTemplates returns all RADIUS attribute templates.
func (h *Handler) ListTemplates(c *gin.Context) {
	rows, err := h.db.Query(`SELECT id, name, description, attributes, is_active, created_at
		FROM radius_templates ORDER BY is_active DESC, name`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch templates")
		return
	}
	defer rows.Close()

	templates := []RadiusTemplate{}
	for rows.Next() {
		var t RadiusTemplate
		var attrsRaw []byte
		rows.Scan(&t.ID, &t.Name, &t.Description, &attrsRaw, &t.IsActive, &t.CreatedAt)
		json.Unmarshal(attrsRaw, &t.Attributes)
		if t.Attributes == nil {
			t.Attributes = []AttributeEntry{}
		}
		templates = append(templates, t)
	}
	c.JSON(http.StatusOK, gin.H{"data": templates})
}

// CreateTemplate creates a new RADIUS attribute template.
func (h *Handler) CreateTemplate(c *gin.Context) {
	var req struct {
		Name        string           `json:"name" binding:"required,min=2,max=100"`
		Description string           `json:"description"`
		Attributes  []AttributeEntry `json:"attributes"`
		IsActive    *bool            `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	if req.Attributes == nil {
		req.Attributes = []AttributeEntry{}
	}
	if err := validateTemplateAttrs(req.Attributes); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	attrsJSON, _ := json.Marshal(req.Attributes)

	var id int
	err := h.db.QueryRow(`INSERT INTO radius_templates (name,description,attributes,is_active)
		VALUES ($1,$2,$3,$4) RETURNING id`,
		req.Name, nullableString(req.Description), string(attrsJSON), isActive).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			respondError(c, http.StatusConflict, "template name already exists")
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to create template")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id, "message": "template created"})
}

// UpdateTemplate updates template details and attributes.
func (h *Handler) UpdateTemplate(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name        string           `json:"name"`
		Description string           `json:"description"`
		Attributes  []AttributeEntry `json:"attributes"`
		IsActive    *bool            `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateTemplateAttrs(req.Attributes); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	attrsJSON, _ := json.Marshal(req.Attributes)

	if _, err := h.db.Exec(`UPDATE radius_templates SET
		name=COALESCE(NULLIF($1,''),name),
		description=COALESCE(NULLIF($2,''),description),
		attributes=CASE WHEN $3='null' THEN attributes ELSE $3::jsonb END,
		is_active=COALESCE($4,is_active)
		WHERE id=$5`,
		req.Name, req.Description, string(attrsJSON), req.IsActive, id); err != nil {
		if isUniqueViolation(err) {
			respondError(c, http.StatusConflict, "template name already exists")
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to update template")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "template updated"})
}

// DeleteTemplate removes a template.
func (h *Handler) DeleteTemplate(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	h.db.Exec(`DELETE FROM radius_templates WHERE id=$1`, id)
	c.JSON(http.StatusOK, gin.H{"message": "template deleted"})
}

// allowedTemplateOps are the FreeRADIUS operators a template may use.
var allowedTemplateOps = map[string]bool{":=": true, "=": true, "==": true, "+=": true, "!=": true,
	">": true, ">=": true, "<": true, "<=": true}

// protectedCheckAttrs may not be set by a template: they would change the password or the
// billing/suspension state that syncAccess manages.
// Keys are lower case: FreeRADIUS matches attribute names without regard to case.
var protectedCheckAttrs = map[string]bool{"cleartext-password": true, "auth-type": true,
	"expiration": true, "simultaneous-use": true, "crypt-password": true, "md5-password": true,
	"nt-password": true, "sha-password": true}

func validateTemplateAttrs(attrs []AttributeEntry) error {
	for _, a := range attrs {
		if a.Attribute == "" || len(a.Attribute) > 64 || len(a.Value) > 253 {
			return fmt.Errorf("attribute names must be 1-64 characters and values at most 253")
		}
		// Checked whatever the operator: an empty one is applied as := (applyTemplateTx).
		if a.Table == "check" && protectedCheckAttrs[strings.ToLower(strings.TrimSpace(a.Attribute))] {
			return fmt.Errorf("%s is managed by the system and cannot be set by a template", a.Attribute)
		}
		if a.Op != "" && !allowedTemplateOps[a.Op] {
			return fmt.Errorf("operator %q is not allowed", a.Op)
		}
	}
	return nil
}

// applyTemplateTx writes a template's attributes for one user: reply attributes (the
// default) replace any existing value of the same name; check attributes are upserted.
func applyTemplateTx(q dbtx, username string, attrs []AttributeEntry) error {
	if err := validateTemplateAttrs(attrs); err != nil {
		return err
	}
	for _, attr := range attrs {
		op := attr.Op
		if attr.Table == "check" {
			if op == "" {
				op = ":="
			}
			if err := setCheck(q, username, attr.Attribute, op, attr.Value); err != nil {
				return err
			}
		} else {
			if op == "" {
				op = "="
			}
			if err := setReply(q, username, attr.Attribute, op, attr.Value); err != nil {
				return err
			}
		}
	}
	return nil
}

// ApplyTemplate applies a template's attributes to one or more RADIUS users.
// System-managed check items (password, Auth-Type, Expiration, Simultaneous-Use) are
// refused; template routes are admin/super_admin only in main.go.
func (h *Handler) ApplyTemplate(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Usernames []string `json:"usernames" binding:"required,min=1"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	var attrsRaw []byte
	err = h.db.QueryRow(`SELECT attributes FROM radius_templates WHERE id=$1 AND is_active=true`, id).Scan(&attrsRaw)
	if err != nil {
		respondError(c, http.StatusNotFound, "template not found or inactive")
		return
	}
	var attrs []AttributeEntry
	json.Unmarshal(attrsRaw, &attrs)

	applied, failed := 0, 0
	errors := []string{}
	for _, username := range req.Usernames {
		var exists bool
		h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM radius_users WHERE username=$1)`, username).Scan(&exists)
		if !exists {
			failed++
			errors = append(errors, username+": user not found")
			continue
		}
		if err := h.inTx(func(tx *sql.Tx) error { return applyTemplateTx(tx, username, attrs) }); err != nil {
			failed++
			errors = append(errors, username+": "+err.Error())
			continue
		}
		applied++
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "template applied",
		"applied": applied,
		"failed":  failed,
		"errors":  errors,
	})
}

// CloneTemplate duplicates an existing template.
func (h *Handler) CloneTemplate(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	var newID int
	err = h.db.QueryRow(`INSERT INTO radius_templates (name,description,attributes,is_active)
		SELECT $1, description, attributes, is_active FROM radius_templates WHERE id=$2
		RETURNING id`, req.Name, id).Scan(&newID)
	if err == sql.ErrNoRows {
		respondError(c, http.StatusNotFound, "template not found")
		return
	}
	if isUniqueViolation(err) {
		respondError(c, http.StatusConflict, "a template with this name already exists")
		return
	}
	if isBadValue(err) {
		respondError(c, http.StatusBadRequest, "invalid name")
		return
	}
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to clone template")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": newID, "message": "template cloned"})
}
