package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────────
// Bulk User Operations
// ─────────────────────────────────────────────────────────────────────────────

// BulkOperation performs an action on multiple RADIUS users. Each username is processed in
// its own transaction with the same helpers the single-user endpoints use, so a bulk
// suspend/expiry/plan change has exactly the same effect on the network as doing it one by
// one. Errors are reported per user.
//
// Actions: suspend, activate, delete, change_plan {plan_id, start_now}, set_expiry {expiry:
// YYYY-MM-DD or ""}, apply_template {template_id}, reset_attributes (removes speed, fixed IP,
// data and time caps; the device limit and expiry are re-written from the user record).
func (h *Handler) BulkOperation(c *gin.Context) {
	var req struct {
		Usernames []string               `json:"usernames" binding:"required,min=1,max=5000"`
		Action    string                 `json:"action"    binding:"required"`
		Params    map[string]interface{} `json:"params"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	claims, _ := middleware.GetClaims(c)
	var performedBy *int
	if claims != nil {
		id := claims.UserID
		performedBy = &id
	}

	intParam := func(name string) int {
		if req.Params == nil {
			return 0
		}
		if fv, ok := req.Params[name].(float64); ok {
			return int(fv)
		}
		return 0
	}
	strParam := func(name string) (string, bool) {
		if req.Params == nil {
			return "", false
		}
		v, ok := req.Params[name].(string)
		return v, ok
	}

	var templateAttrs []AttributeEntry
	if req.Action == "apply_template" {
		var attrsRaw []byte
		if err := h.db.QueryRow(`SELECT attributes FROM radius_templates WHERE id=$1 AND is_active`, intParam("template_id")).Scan(&attrsRaw); err != nil {
			respondError(c, http.StatusBadRequest, "template not found or inactive")
			return
		}
		json.Unmarshal(attrsRaw, &templateAttrs)
	}

	type Result struct {
		Username string `json:"username"`
		Success  bool   `json:"success"`
		Message  string `json:"message"`
	}

	results := []Result{}
	successCount, failCount := 0, 0

	for _, username := range req.Usernames {
		var userID int
		var msg string
		opErr := h.db.QueryRow(`SELECT id FROM radius_users WHERE username=$1`, username).Scan(&userID)
		if opErr == sql.ErrNoRows {
			opErr = fmt.Errorf("user not found")
		}

		if opErr == nil {
			switch req.Action {
			case "suspend", "activate":
				target := map[string]string{"suspend": "suspended", "activate": "active"}[req.Action]
				var st string
				_, st, opErr = setUserStatus(h.db, userID, target)
				msg = st
				if opErr == nil && target == "suspended" {
					disconnectUserSessions(h.db, username, false)
				}

			case "delete":
				disconnectUserSessions(h.db, username, true)
				opErr = h.inTx(func(tx *sql.Tx) error { return deleteRadiusUserTx(tx, userID, username) })
				msg = "deleted"

			case "change_plan":
				planID := intParam("plan_id")
				startNow, _ := req.Params["start_now"].(bool)
				if planID == 0 {
					opErr = fmt.Errorf("plan_id required in params")
					break
				}
				opErr = h.inTx(func(tx *sql.Tx) error {
					res, err := assignPlanTx(tx, userID, username, planID, startNow, "", performedBy, nil)
					if err == nil {
						msg = res.Message
					}
					return err
				})

			case "set_expiry":
				expiry, ok := strParam("expiry")
				if !ok {
					opErr = fmt.Errorf("expiry required in params (YYYY-MM-DD, or empty for never)")
					break
				}
				var d *time.Time
				if d, opErr = parseDateParam(expiry); opErr != nil {
					break
				}
				opErr = h.inTx(func(tx *sql.Tx) error {
					if _, err := tx.Exec(`UPDATE radius_users SET account_expiry=$1,
						status = CASE WHEN status='expired' AND ($1::date IS NULL OR $1::date + COALESCE(grace_days, $3::int) >= CURRENT_DATE) THEN 'active' ELSE status END,
						updated_at=NOW() WHERE id=$2`, d, userID, graceDays()); err != nil {
						return err
					}
					return syncAccess(tx, username)
				})
				if expiry == "" {
					msg = "expiry cleared (never expires)"
				} else {
					msg = "expiry set to " + expiry
				}

			case "apply_template":
				opErr = h.inTx(func(tx *sql.Tx) error { return applyTemplateTx(tx, username, templateAttrs) })
				msg = fmt.Sprintf("template #%d applied", intParam("template_id"))

			case "reset_attributes":
				opErr = h.inTx(func(tx *sql.Tx) error {
					if err := deleteReply(tx, username, "Mikrotik-Rate-Limit", "WISPr-Bandwidth-Max-Up",
						"WISPr-Bandwidth-Max-Down", "Framed-IP-Address", "Session-Timeout"); err != nil {
						return err
					}
					if err := deleteCheck(tx, username, "Max-Octets", "Max-All-Session", "Session-Timeout"); err != nil {
						return err
					}
					if _, err := tx.Exec(`UPDATE radius_users SET bandwidth_profile_id=NULL WHERE id=$1`, userID); err != nil {
						return err
					}
					if _, err := tx.Exec(`UPDATE ip_pool_assignments SET username=NULL, user_id=NULL, leased_at=NULL WHERE username=$1`, username); err != nil {
						return err
					}
					return syncAccess(tx, username)
				})
				msg = "RADIUS attributes reset"

			default:
				opErr = fmt.Errorf("unknown action: %s", req.Action)
			}
		}

		success := opErr == nil
		if success {
			successCount++
			if msg == "" {
				msg = "ok"
			}
		} else {
			failCount++
			msg = opErr.Error()
		}
		results = append(results, Result{Username: username, Success: success, Message: msg})
	}

	// Log bulk operation
	paramsJSON, _ := json.Marshal(req.Params)
	if _, err := h.db.Exec(`INSERT INTO bulk_operations (operation,target_count,success_count,fail_count,params,performed_by)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6)`,
		req.Action, len(req.Usernames), successCount, failCount, string(paramsJSON), performedBy); err != nil {
		h.log.WithError(err).Warn("record bulk operation")
	}

	c.JSON(http.StatusOK, gin.H{
		"action":        req.Action,
		"total":         len(req.Usernames),
		"success_count": successCount,
		"fail_count":    failCount,
		"results":       results,
	})
}

// ListBulkOpHistory returns recent bulk operation history.
func (h *Handler) ListBulkOpHistory(c *gin.Context) {
	type BulkOp struct {
		ID           int       `json:"id"`
		Operation    string    `json:"operation"`
		TargetCount  int       `json:"target_count"`
		SuccessCount int       `json:"success_count"`
		FailCount    int       `json:"fail_count"`
		PerformedBy  *string   `json:"performed_by"`
		CreatedAt    time.Time `json:"created_at"`
	}
	rows, err := h.db.Query(`
		SELECT bo.id, bo.operation, bo.target_count, bo.success_count, bo.fail_count,
		       u.username, bo.created_at
		FROM bulk_operations bo
		LEFT JOIN app_users u ON u.id = bo.performed_by
		ORDER BY bo.created_at DESC LIMIT 50`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch history")
		return
	}

	ops := []BulkOp{}
	{
		defer rows.Close()
		for rows.Next() {
			var op BulkOp
			rows.Scan(&op.ID, &op.Operation, &op.TargetCount, &op.SuccessCount, &op.FailCount, &op.PerformedBy, &op.CreatedAt)
			ops = append(ops, op)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": ops})
}

// inTx runs fn in a transaction, committing only if it returns nil.
func (h *Handler) inTx(fn func(*sql.Tx) error) error {
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
