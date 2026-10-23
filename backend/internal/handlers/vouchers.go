package handlers

import (
	"crypto/rand"
	"database/sql"
	"encoding/csv"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/gin-gonic/gin"
)

// 32 symbols x 12 positions (~60 bits) from crypto/rand; the alphabet drops look-alike
// characters so codes survive being read out or printed on scratch cards.
const voucherChars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O or 1/I

func generateVoucherCode() (string, error) {
	parts := make([]string, 3)
	for p := range parts {
		b := make([]byte, 4)
		for i := range b {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(voucherChars))))
			if err != nil {
				return "", err
			}
			b[i] = voucherChars[n.Int64()]
		}
		parts[p] = string(b)
	}
	return strings.Join(parts, "-"), nil
}

// GenerateVouchersRequest is the payload for batch voucher generation.
type GenerateVouchersRequest struct {
	Count        int     `json:"count" binding:"required,min=1,max=500"`
	BatchName    string  `json:"batch_name"`
	DataLimitMB  *int64  `json:"data_limit_mb" binding:"omitempty,min=1"`
	TimeLimitMin *int    `json:"time_limit_minutes" binding:"omitempty,min=1"`
	ValidDays    int     `json:"valid_days" binding:"omitempty,min=1,max=3650"`
	PlanID       *int    `json:"plan_id"`
	Price        float64 `json:"price" binding:"omitempty,min=0"`
	Currency     string  `json:"currency"`
	Devices      int     `json:"devices" binding:"omitempty,min=1,max=50"`
}

// ListVouchers returns paginated vouchers with optional filters.
func (h *Handler) ListVouchers(c *gin.Context) {
	offset, limit := paginationParams(c)
	status := c.Query("status")
	batch := c.Query("batch")

	where := "WHERE 1=1"
	args := []interface{}{}
	argN := 1

	if status != "" {
		where += fmt.Sprintf(" AND v.status = $%d", argN)
		args = append(args, status)
		argN++
	}
	if batch != "" {
		where += fmt.Sprintf(" AND v.batch_name ILIKE $%d", argN)
		args = append(args, "%"+batch+"%")
		argN++
	}

	var total int
	h.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM vouchers v %s", where), args...).Scan(&total)

	dataArgs := append(args, limit, offset)
	rows, err := h.db.Query(fmt.Sprintf(`
		SELECT v.id, v.code, v.batch_name, v.status, v.data_limit_mb, v.time_limit_minutes,
		       COALESCE(v.valid_days, 30), v.expires_at, v.redeemed_by, v.redeemed_at,
		       au.username, v.created_at, v.plan_id, up.name, v.price, v.currency
		FROM vouchers v
		LEFT JOIN app_users au ON au.id = v.created_by
		LEFT JOIN user_plans up ON up.id = v.plan_id
		%s
		ORDER BY v.created_at DESC
		LIMIT $%d OFFSET $%d`, where, argN, argN+1), dataArgs...)
	if err != nil {
		h.log.WithError(err).Error("list vouchers failed")
		respondError(c, http.StatusInternalServerError, "failed to fetch vouchers")
		return
	}
	defer rows.Close()

	type VoucherRow struct {
		ID           int        `json:"id"`
		Code         string     `json:"code"`
		BatchName    *string    `json:"batch_name"`
		Status       string     `json:"status"`
		DataLimitMB  *int64     `json:"data_limit_mb"`
		TimeLimitMin *int       `json:"time_limit_minutes"`
		ValidDays    int        `json:"valid_days"`
		ExpiresAt    *time.Time `json:"expires_at"`
		RedeemedBy   *string    `json:"redeemed_by"`
		RedeemedAt   *time.Time `json:"redeemed_at"`
		CreatedBy    *string    `json:"created_by"`
		CreatedAt    time.Time  `json:"created_at"`
		PlanID       *int       `json:"plan_id"`
		PlanName     *string    `json:"plan_name"`
		Price        float64    `json:"price"`
		Currency     string     `json:"currency"`
	}

	vouchers := []VoucherRow{}
	for rows.Next() {
		var v VoucherRow
		if err := rows.Scan(&v.ID, &v.Code, &v.BatchName, &v.Status, &v.DataLimitMB, &v.TimeLimitMin,
			&v.ValidDays, &v.ExpiresAt, &v.RedeemedBy, &v.RedeemedAt, &v.CreatedBy, &v.CreatedAt,
			&v.PlanID, &v.PlanName, &v.Price, &v.Currency); err != nil {
			h.log.WithError(err).Warn("scan voucher")
			continue
		}
		vouchers = append(vouchers, v)
	}

	c.JSON(http.StatusOK, gin.H{"data": vouchers, "total": total})
}

// GenerateVouchers creates a batch of voucher codes.
// A voucher is a RADIUS login in its own right: the code is both the username and the
// password (radcheck Cleartext-Password = code), so anyone holding the code can connect.
// What FreeRADIUS enforces for each code:
//   - Expiration at expires_at (generation time + valid_days), whether used or not
//   - Simultaneous-Use (devices, default 1; or the plan's device limit)
//   - Max-Octets (data limit) and Max-All-Session (total online time), via mods-available/quota
//   - the plan's speed profile, if a plan is given
// The scheduler's voucher_housekeeping task marks codes 'used' after their first session and
// 'expired' after expires_at. price is recorded for reporting; vouchers are sold offline.
func (h *Handler) GenerateVouchers(c *gin.Context) {
	var req GenerateVouchersRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.ValidDays == 0 {
		req.ValidDays = 30
	}
	if req.Currency == "" {
		req.Currency = defaultCurrency()
	}
	devices := req.Devices
	var plan *planRow
	if req.PlanID != nil {
		p, err := loadPlan(h.db, *req.PlanID)
		if err != nil {
			respondError(c, http.StatusBadRequest, "plan not found")
			return
		}
		plan = p
		if devices == 0 {
			devices = p.MaxDevices
		}
		if req.DataLimitMB == nil && p.DataLimitMB != nil && *p.DataLimitMB > 0 {
			req.DataLimitMB = p.DataLimitMB
		}
		if req.Price == 0 {
			req.Price = p.Price
			req.Currency = p.Currency
		}
	}
	if devices == 0 {
		devices = 1
	}

	claims, _ := middleware.GetClaims(c)
	expiresAt := time.Now().AddDate(0, 0, req.ValidDays)

	batchName := req.BatchName
	if batchName == "" {
		batchName = fmt.Sprintf("Batch-%s", time.Now().Format("20060102-1504"))
	}

	created := 0
	var lastErr error
	for i := 0; i < req.Count; i++ {
		for attempt := 0; attempt < 5; attempt++ {
			code, err := generateVoucherCode()
			if err != nil {
				lastErr = err
				break
			}
			err = h.inTx(func(tx *sql.Tx) error {
				if _, err := tx.Exec(`
					INSERT INTO vouchers (code, batch_name, data_limit_mb, time_limit_minutes, valid_days,
					                      expires_at, created_by, plan_id, price, currency)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
					code, batchName, req.DataLimitMB, req.TimeLimitMin, req.ValidDays, expiresAt,
					claims.UserID, req.PlanID, round2(req.Price), strings.ToUpper(req.Currency)); err != nil {
					return err
				}
				// A code must not collide with a customer's username either.
				var taken bool
				tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM radcheck WHERE username=$1)`, code).Scan(&taken)
				if taken {
					return fmt.Errorf("code in use")
				}
				for _, a := range [][2]string{
					{"Cleartext-Password", code},
					{"Expiration", expiresAt.Format(radiusDateLayout)},
					{"Simultaneous-Use", fmt.Sprintf("%d", devices)},
				} {
					if err := setCheck(tx, code, a[0], ":=", a[1]); err != nil {
						return err
					}
				}
				if req.DataLimitMB != nil {
					if err := setCheck(tx, code, "Max-Octets", ":=", fmt.Sprintf("%d", *req.DataLimitMB*1024*1024)); err != nil {
						return err
					}
				}
				if req.TimeLimitMin != nil {
					if err := setCheck(tx, code, "Max-All-Session", ":=", fmt.Sprintf("%d", *req.TimeLimitMin*60)); err != nil {
						return err
					}
				}
				if plan != nil && plan.BandwidthProfileID != nil {
					var up, down int
					var rl sql.NullString
					if err := tx.QueryRow(`SELECT upload_kbps, download_kbps, mikrotik_rate_limit FROM bandwidth_profiles WHERE id=$1`,
						*plan.BandwidthProfileID).Scan(&up, &down, &rl); err == nil {
						rate := kbpsToMikrotik(up, down)
						if rl.Valid && rl.String != "" {
							rate = rl.String
						}
						if err := setReply(tx, code, "Mikrotik-Rate-Limit", "=", rate); err != nil {
							return err
						}
					}
				}
				return nil
			})
			if err == nil {
				created++
				break
			}
			lastErr = err
		}
	}

	resp := gin.H{
		"message": fmt.Sprintf("Generated %d vouchers in batch '%s'", created, batchName),
		"count":   created,
		"batch":   batchName,
	}
	status := http.StatusCreated
	if created < req.Count {
		resp["message"] = fmt.Sprintf("Generated only %d of %d vouchers in batch '%s'", created, req.Count, batchName)
		if lastErr != nil {
			resp["error"] = lastErr.Error()
		}
		if created == 0 {
			status = http.StatusInternalServerError
		}
	}
	c.JSON(status, resp)
}

// GetVoucherBatches lists all voucher batches with summary counts.
func (h *Handler) GetVoucherBatches(c *gin.Context) {
	rows, err := h.db.Query(`
		SELECT batch_name,
		       COUNT(*) as total,
		       SUM(CASE WHEN status='active'   THEN 1 ELSE 0 END) as active,
		       SUM(CASE WHEN status='used'     THEN 1 ELSE 0 END) as used,
		       SUM(CASE WHEN status='disabled' THEN 1 ELSE 0 END) as disabled,
		       MIN(created_at) as created_at
		FROM vouchers
		WHERE batch_name IS NOT NULL
		GROUP BY batch_name
		ORDER BY MIN(created_at) DESC`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch batches")
		return
	}
	defer rows.Close()

	type Batch struct {
		Name      *string   `json:"name"`
		Total     int       `json:"total"`
		Active    int       `json:"active"`
		Used      int       `json:"used"`
		Disabled  int       `json:"disabled"`
		CreatedAt time.Time `json:"created_at"`
	}

	batches := []Batch{}
	for rows.Next() {
		var b Batch
		rows.Scan(&b.Name, &b.Total, &b.Active, &b.Used, &b.Disabled, &b.CreatedAt)
		batches = append(batches, b)
	}
	c.JSON(http.StatusOK, gin.H{"data": batches})
}

// DisableVoucher sets a voucher's status to disabled and blocks RADIUS auth.
func (h *Handler) DisableVoucher(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var code string
	if err := h.db.QueryRow(`SELECT code FROM vouchers WHERE id = $1`, id).Scan(&code); err != nil {
		respondError(c, http.StatusNotFound, "voucher not found")
		return
	}
	if err := h.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE vouchers SET status = 'disabled', updated_at = NOW() WHERE id = $1`, id); err != nil {
			return err
		}
		return setCheck(tx, code, "Auth-Type", ":=", "Reject")
	}); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to disable voucher")
		return
	}
	note := disconnectUserSessions(h.db, code, false).stillOnlineNote()
	c.JSON(http.StatusOK, gin.H{"message": "voucher disabled" + note})
}

// DeleteVoucher removes a voucher and its RADIUS entries.
func (h *Handler) DeleteVoucher(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var code string
	if err := h.db.QueryRow(`SELECT code FROM vouchers WHERE id = $1`, id).Scan(&code); err != nil {
		respondError(c, http.StatusNotFound, "voucher not found")
		return
	}
	if err := h.inTx(func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM radcheck WHERE username = $1`, `DELETE FROM radreply WHERE username = $1`} {
			if _, err := tx.Exec(q, code); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`DELETE FROM vouchers WHERE id = $1`, id)
		return err
	}); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete voucher")
		return
	}
	note := disconnectUserSessions(h.db, code, true).stillOnlineNote()
	c.JSON(http.StatusOK, gin.H{"message": "voucher deleted" + note})
}

// ExportVouchers exports vouchers as CSV.
func (h *Handler) ExportVouchers(c *gin.Context) {
	batch := c.Query("batch")
	where := "WHERE 1=1"
	args := []interface{}{}
	if batch != "" {
		where += " AND batch_name = $1"
		args = append(args, batch)
	}

	rows, err := h.db.Query(fmt.Sprintf(`
		SELECT code, batch_name, status, data_limit_mb, time_limit_minutes,
		       valid_days, expires_at, redeemed_by, created_at, price, currency
		FROM vouchers %s ORDER BY batch_name, created_at`, where), args...)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "export failed")
		return
	}
	defer rows.Close()

	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", `attachment; filename="vouchers_export.csv"`)

	w := csv.NewWriter(c.Writer)
	w.Write([]string{"code", "batch", "status", "data_limit_mb", "time_limit_min", "valid_days", "expires_at", "redeemed_by", "created_at", "price", "currency"})

	ns := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	ni64 := func(i *int64) string {
		if i == nil {
			return ""
		}
		return fmt.Sprintf("%d", *i)
	}
	ni := func(i *int) string {
		if i == nil {
			return ""
		}
		return fmt.Sprintf("%d", *i)
	}
	nt := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.Format("2006-01-02 15:04")
	}

	for rows.Next() {
		var code, status string
		var batch, redeemedBy *string
		var dataLimitMB *int64
		var timeLimitMins, validDays *int
		var expiresAt *time.Time
		var createdAt time.Time
		var price float64
		var currency string

		if rows.Scan(&code, &batch, &status, &dataLimitMB, &timeLimitMins, &validDays, &expiresAt, &redeemedBy, &createdAt, &price, &currency) != nil {
			continue
		}
		vd := 30
		if validDays != nil {
			vd = *validDays
		}
		w.Write([]string{
			code, ns(batch), status, ni64(dataLimitMB), ni(timeLimitMins),
			fmt.Sprintf("%d", vd), nt(expiresAt), ns(redeemedBy), createdAt.Format("2006-01-02 15:04"),
			fmt.Sprintf("%.2f", price), currency,
		})
	}
	w.Flush()
}
