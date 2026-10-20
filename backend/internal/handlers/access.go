package handlers

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/freeradius-manager/backend/internal/settings"
)

// ─────────────────────────────────────────────────────────────────────────────
// What FreeRADIUS enforces
// ─────────────────────────────────────────────────────────────────────────────
//
// FreeRADIUS reads radcheck/radreply only (never radius_users, plans or invoices), so every
// business rule that must stop or limit a customer on the network is copied into radcheck
// here. syncAccess is the single place that derives those rows from radius_users; every
// code path that changes status, account_expiry or device_limit calls it in the same
// transaction.
//
//   radius_users.status 'suspended' or 'expired'  → Auth-Type := Reject
//   radius_users.account_expiry (last paid day)   → Expiration := day after (+ grace days)
//   radius_users.device_limit                     → Simultaneous-Use
//
// Data caps (Max-Octets) and speed (radreply) are written by applyPlanLimits.

// dbtx is satisfied by *database.DB and *sql.Tx, so helpers work inside or outside a
// transaction.
type dbtx interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
	Query(query string, args ...interface{}) (*sql.Rows, error)
	QueryRow(query string, args ...interface{}) *sql.Row
}

// radiusDateLayout is the Expiration format FreeRADIUS parses ("Jan 02 2027 00:00:00").
// An ISO date ("2027-01-02") fails to parse and makes FreeRADIUS reject the user outright
// (verified against FreeRADIUS 3.2.5).
const radiusDateLayout = "Jan 02 2006 15:04:05"

// radiusExpiration converts a last-paid day into the moment access ends: midnight at the
// start of the following day, plus any grace days. FreeRADIUS interprets it in its own
// timezone (the TZ of the freeradius container).
func radiusExpiration(lastPaidDay time.Time, graceDays int) string {
	if graceDays < 0 {
		graceDays = 0
	}
	d := time.Date(lastPaidDay.Year(), lastPaidDay.Month(), lastPaidDay.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, 1+graceDays).Format(radiusDateLayout)
}

func graceDays() int {
	return settings.Int("billing_grace_days", "", 0)
}

// setCheck upserts one radcheck row (radcheck has a unique index on username+attribute).
func setCheck(q dbtx, username, attribute, op, value string) error {
	_, err := q.Exec(`
		INSERT INTO radcheck (username, attribute, op, value) VALUES ($1, $2, $3, $4)
		ON CONFLICT (username, attribute) DO UPDATE SET op = EXCLUDED.op, value = EXCLUDED.value`,
		username, attribute, op, value)
	return err
}

func deleteCheck(q dbtx, username string, attributes ...string) error {
	for _, a := range attributes {
		if _, err := q.Exec(`DELETE FROM radcheck WHERE username = $1 AND attribute = $2`, username, a); err != nil {
			return err
		}
	}
	return nil
}

// setReply replaces a radreply attribute. radreply has no unique index (FreeRADIUS allows
// repeated reply attributes), so ON CONFLICT cannot be used: delete, then insert.
func setReply(q dbtx, username, attribute, op, value string) error {
	if _, err := q.Exec(`DELETE FROM radreply WHERE username = $1 AND attribute = $2`, username, attribute); err != nil {
		return err
	}
	_, err := q.Exec(`INSERT INTO radreply (username, attribute, op, value) VALUES ($1, $2, $3, $4)`,
		username, attribute, op, value)
	return err
}

func deleteReply(q dbtx, username string, attributes ...string) error {
	for _, a := range attributes {
		if _, err := q.Exec(`DELETE FROM radreply WHERE username = $1 AND attribute = $2`, username, a); err != nil {
			return err
		}
	}
	return nil
}

// syncAccess rewrites the enforcement rows for one customer from radius_users. A username
// with no radius_users row (a voucher) is left alone.
func syncAccess(q dbtx, username string) error {
	var status string
	var expiry sql.NullTime
	var deviceLimit int
	err := q.QueryRow(`SELECT status, account_expiry, device_limit FROM radius_users WHERE username = $1`, username).
		Scan(&status, &expiry, &deviceLimit)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read user %s: %w", username, err)
	}

	if status == "suspended" || status == "expired" {
		err = setCheck(q, username, "Auth-Type", ":=", "Reject")
	} else {
		_, err = q.Exec(`DELETE FROM radcheck WHERE username = $1 AND attribute = 'Auth-Type' AND value = 'Reject'`, username)
	}
	if err != nil {
		return err
	}

	if expiry.Valid {
		err = setCheck(q, username, "Expiration", ":=", radiusExpiration(expiry.Time, graceDays()))
	} else {
		err = deleteCheck(q, username, "Expiration")
	}
	if err != nil {
		return err
	}

	if deviceLimit < 1 {
		deviceLimit = 1
	}
	return setCheck(q, username, "Simultaneous-Use", ":=", strconv.Itoa(deviceLimit))
}

// planRow is the part of a plan that is enforced on the network or billed.
type planRow struct {
	ID                 int
	Name               string
	Price              float64
	Currency           string
	DataLimitMB        *int64
	ValidityDays       int
	BandwidthProfileID *int
	MaxDevices         int
	IsActive           bool
}

func loadPlan(q dbtx, planID int) (*planRow, error) {
	var p planRow
	err := q.QueryRow(`
		SELECT id, name, price, currency, data_limit_mb, validity_days, bandwidth_profile_id, max_devices, is_active
		FROM user_plans WHERE id = $1`, planID).
		Scan(&p.ID, &p.Name, &p.Price, &p.Currency, &p.DataLimitMB, &p.ValidityDays,
			&p.BandwidthProfileID, &p.MaxDevices, &p.IsActive)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// applyPlanLimits links the plan to the customer and writes its limits: device limit,
// data cap (Max-Octets, enforced by the datacounter module) and speed profile (radreply).
// It does not change the paid-until date; see grantPlanPeriod.
func applyPlanLimits(q dbtx, userID int, username string, p *planRow) error {
	if _, err := q.Exec(`UPDATE radius_users SET plan_id = $1, device_limit = $2, updated_at = NOW() WHERE id = $3`,
		p.ID, p.MaxDevices, userID); err != nil {
		return err
	}
	if p.DataLimitMB != nil && *p.DataLimitMB > 0 {
		if err := setCheck(q, username, "Max-Octets", ":=", strconv.FormatInt(*p.DataLimitMB*1024*1024, 10)); err != nil {
			return err
		}
	} else if err := deleteCheck(q, username, "Max-Octets"); err != nil {
		return err
	}
	if p.BandwidthProfileID != nil {
		if err := applyBandwidthProfileTx(q, userID, username, *p.BandwidthProfileID); err != nil {
			return err
		}
	}
	return syncAccess(q, username)
}

// grantPlanPeriod adds one plan period to the customer's paid-until date and re-activates
// an expired account. The period extends from the current paid-until date when that is
// still in the future (paying early loses nothing), otherwise from today. The data-cap
// counter restarts from today. Returns the new paid-until date and the number of days the
// date actually moved (what revokePlanPeriod takes back if the payment is reversed).
func grantPlanPeriod(q dbtx, userID int, p *planRow) (time.Time, int, error) {
	days := p.ValidityDays
	if days <= 0 {
		days = 30
	}
	// Read the old date first (locking the row) in its own statement. Doing it in a CTE of
	// the UPDATE looks equivalent but is not: the CTE is evaluated after the row was already
	// updated, so the "old" date came back empty and every early renewal recorded one day too
	// many (plus the days still left), which a later reversal then wrongly took back.
	var old sql.NullTime
	if err := q.QueryRow(`SELECT account_expiry FROM radius_users WHERE id = $1 FOR UPDATE`, userID).Scan(&old); err != nil {
		return time.Time{}, 0, err
	}
	var oldArg interface{}
	if old.Valid {
		oldArg = old.Time.Format("2006-01-02")
	}
	var username string
	var newExpiry time.Time
	var moved int
	err := q.QueryRow(`
		UPDATE radius_users SET
		    account_expiry = GREATEST(COALESCE(account_expiry, CURRENT_DATE), CURRENT_DATE) + $1::int,
		    period_start   = CURRENT_DATE,
		    status         = CASE WHEN status = 'expired' THEN 'active' ELSE status END,
		    updated_at     = NOW()
		WHERE id = $2
		RETURNING username, account_expiry, account_expiry - COALESCE($3::date, CURRENT_DATE - 1)`,
		days, userID, oldArg).Scan(&username, &newExpiry, &moved)
	if err != nil {
		return time.Time{}, 0, err
	}
	return newExpiry, moved, syncAccess(q, username)
}

// blockUntilPaid sets an unpaid customer's paid-until date to yesterday (status expired)
// unless they still have paid time left. Grace days are subtracted as well, so a customer who
// has never paid cannot be rescued by the grace period: it is for people who paid before.
func blockUntilPaid(q dbtx, userID int) error {
	var username string
	err := q.QueryRow(`
		UPDATE radius_users SET account_expiry = CURRENT_DATE - 1 - $2::int,
		       status = CASE WHEN status = 'active' THEN 'expired' ELSE status END, updated_at = NOW()
		WHERE id = $1 AND (account_expiry IS NULL OR account_expiry + $2::int < CURRENT_DATE)
		RETURNING username`, userID, graceDays()).Scan(&username)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return syncAccess(q, username)
}

// revokePlanPeriod takes back days granted by an invoice whose payment was reversed, and
// marks the account expired if that leaves it past its paid-until date.
func revokePlanPeriod(q dbtx, userID, days int) error {
	var username string
	err := q.QueryRow(`
		UPDATE radius_users SET
		    account_expiry = account_expiry - $1::int,
		    status = CASE WHEN status = 'active' AND account_expiry - $1::int + $3::int < CURRENT_DATE
		                  THEN 'expired' ELSE status END,
		    updated_at = NOW()
		WHERE id = $2 AND account_expiry IS NOT NULL
		RETURNING username`, days, userID, graceDays()).Scan(&username)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return syncAccess(q, username)
}

// applyBandwidthProfileTx writes the speed attributes for a profile and records the link.
// Mikrotik-Rate-Limit is understood by MikroTik; the WISPr attributes (bits per second) by
// many hotspot gateways. Other vendors need their own attributes (use a template).
func applyBandwidthProfileTx(q dbtx, userID int, username string, profileID int) error {
	var up, down int
	var rl sql.NullString
	if err := q.QueryRow(`SELECT upload_kbps, download_kbps, mikrotik_rate_limit FROM bandwidth_profiles WHERE id = $1`, profileID).
		Scan(&up, &down, &rl); err != nil {
		return fmt.Errorf("bandwidth profile %d: %w", profileID, err)
	}
	rate := kbpsToMikrotik(up, down)
	if rl.Valid && rl.String != "" {
		rate = rl.String
	}
	if _, err := q.Exec(`UPDATE radius_users SET bandwidth_profile_id = $1 WHERE id = $2`, profileID, userID); err != nil {
		return err
	}
	if err := setReply(q, username, "Mikrotik-Rate-Limit", "=", rate); err != nil {
		return err
	}
	if err := setReply(q, username, "WISPr-Bandwidth-Max-Up", "=", strconv.Itoa(up*1000)); err != nil {
		return err
	}
	return setReply(q, username, "WISPr-Bandwidth-Max-Down", "=", strconv.Itoa(down*1000))
}

// deleteRadiusUserTx removes a customer and everything FreeRADIUS knows about them.
func deleteRadiusUserTx(q dbtx, userID int, username string) error {
	for _, stmt := range []string{
		`DELETE FROM radcheck WHERE username = $1`,
		`DELETE FROM radreply WHERE username = $1`,
		`DELETE FROM radusergroup WHERE username = $1`,
		`UPDATE ip_pool_assignments SET username = NULL, user_id = NULL, leased_at = NULL WHERE username = $1`,
		`DELETE FROM portal_sessions WHERE username = $1`,
	} {
		if _, err := q.Exec(stmt, username); err != nil {
			return err
		}
	}
	_, err := q.Exec(`DELETE FROM radius_users WHERE id = $1`, userID)
	return err
}
