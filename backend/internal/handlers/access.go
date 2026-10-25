package handlers

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
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

// graceDays is the system-wide default: days of service after the paid-until date.
func graceDays() int {
	return settings.Int("billing_grace_days", "", 0)
}

// effectiveGrace is a customer's own grace days if staff set them, else the system default.
// SQL that compares dates writes the same rule as COALESCE(grace_days, $default).
func effectiveGrace(own sql.NullInt64) int {
	if own.Valid {
		return int(own.Int64)
	}
	return graceDays()
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
	var ownGrace sql.NullInt64
	err := q.QueryRow(`SELECT status, account_expiry, device_limit, grace_days FROM radius_users WHERE username = $1`, username).
		Scan(&status, &expiry, &deviceLimit, &ownGrace)
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
		err = setCheck(q, username, "Expiration", ":=", radiusExpiration(expiry.Time, effectiveGrace(ownGrace)))
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
	// The speed profile of the plan the customer is leaving (see writePlanLimits).
	var prev sql.NullInt64
	if err := q.QueryRow(`SELECT up.bandwidth_profile_id FROM radius_users ru
		JOIN user_plans up ON up.id = ru.plan_id WHERE ru.id = $1`, userID).Scan(&prev); err != nil && err != sql.ErrNoRows {
		return err
	}
	return writePlanLimits(q, userID, username, p, prev)
}

// writePlanLimits is applyPlanLimits with the speed profile the customer got from their
// previous plan (or from this plan before it was edited). When the plan sets no speed, that
// profile is taken away; a profile staff gave the customer by hand is left alone.
func writePlanLimits(q dbtx, userID int, username string, p *planRow, prevPlanProfile sql.NullInt64) error {
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
	} else if prevPlanProfile.Valid {
		var fromPlan bool
		if err := q.QueryRow(`SELECT COALESCE(bandwidth_profile_id = $2, FALSE) FROM radius_users WHERE id = $1`,
			userID, prevPlanProfile.Int64).Scan(&fromPlan); err != nil {
			return err
		}
		if fromPlan {
			if err := clearBandwidthTx(q, userID, username); err != nil {
				return err
			}
		}
	}
	return syncAccess(q, username)
}

// clearBandwidthTx removes a customer's speed limit: the link and the attributes that
// applyBandwidthProfileTx writes.
func clearBandwidthTx(q dbtx, userID int, username string) error {
	if _, err := q.Exec(`UPDATE radius_users SET bandwidth_profile_id = NULL WHERE id = $1`, userID); err != nil {
		return err
	}
	return deleteReply(q, username, "Mikrotik-Rate-Limit", "WISPr-Bandwidth-Max-Up", "WISPr-Bandwidth-Max-Down")
}

// grantPlanPeriod adds one plan period to the customer's paid-until date and re-activates
// an expired account. The period extends from the current paid-until date when that is
// still in the future (paying early loses nothing), otherwise from today. The data-cap
// counter restarts now: only sessions recorded after this moment count (period_acct_floor is
// the newest radacct row at the time; row numbers, unlike router timestamps, cannot be off),
// so a customer who ran out of data and pays again gets online straight away. A session that
// was already open keeps going uncounted until it ends, which Session-Timeout makes happen by
// the old paid-until midnight at the latest. Returns the new paid-until date and the number of days the
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
		    period_acct_floor = (SELECT COALESCE(MAX(radacctid), 0) FROM radacct),
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
		UPDATE radius_users SET account_expiry = CURRENT_DATE - 1 - COALESCE(grace_days, $2::int),
		       status = CASE WHEN status = 'active' THEN 'expired' ELSE status END, updated_at = NOW()
		WHERE id = $1 AND (account_expiry IS NULL OR account_expiry + COALESCE(grace_days, $2::int) < CURRENT_DATE)
		RETURNING username`, userID, graceDays()).Scan(&username)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return syncAccess(q, username)
}

// startTrial gives a brand-new customer their one free trial: the trial_days setting, or the
// length staff chose for this customer when assigning the plan (override; 0 means none): the
// paid-until date becomes today + trial_days, so FreeRADIUS lets them on until then. It does
// nothing, and returns nil, when there is no trial configured, the customer already had a
// trial, has ever been given a period, or still has paid time left. The first invoice is due
// when the trial ends.
func startTrial(q dbtx, userID int, override *int) (*time.Time, error) {
	days := settings.Int("trial_days", "", 0)
	if override != nil { // staff chose the length for this customer (0 = no trial)
		days = *override
	}
	if days <= 0 {
		return nil, nil
	}
	var username string
	var end time.Time
	err := q.QueryRow(`
		UPDATE radius_users SET account_expiry = CURRENT_DATE + $2::int, trial_started = CURRENT_DATE,
		       status = CASE WHEN status = 'expired' THEN 'active' ELSE status END, updated_at = NOW()
		WHERE id = $1 AND trial_started IS NULL
		  AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.user_id = radius_users.id AND i.service_applied)
		  AND (account_expiry IS NULL OR account_expiry + COALESCE(grace_days, $3::int) < CURRENT_DATE)
		RETURNING username, account_expiry`, userID, days, graceDays()).Scan(&username, &end)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &end, syncAccess(q, username)
}

// revokePlanPeriod takes back days granted by an invoice whose payment was reversed, and
// marks the account expired if that leaves it past its paid-until date.
func revokePlanPeriod(q dbtx, userID, days int) error {
	var username string
	err := q.QueryRow(`
		UPDATE radius_users SET
		    account_expiry = account_expiry - $1::int,
		    status = CASE WHEN status = 'active' AND account_expiry - $1::int + COALESCE(grace_days, $3::int) < CURRENT_DATE
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

// unsettledInvoicesError is returned when a customer cannot be deleted because money is still
// unresolved on one of their invoices.
type unsettledInvoicesError struct{ msg string }

func (e *unsettledInvoicesError) Error() string { return e.msg }

// checkSettled refuses to delete a customer while an open invoice has money on it (payments
// received but not applied to service) or had service started on credit (money owed). Staff
// must deal with those on purpose: reverse the payment to refund it, collect the rest, or
// cancel a credit invoice to write it off. Unpaid prepaid invoices are not a problem; they are
// cancelled with the customer.
func checkSettled(q dbtx, userID int, username string) error {
	rows, err := q.Query(`
		SELECT i.invoice_number, i.currency, i.amount, i.service_applied,
		       COALESCE((SELECT SUM(p.amount) FROM payments p WHERE p.invoice_id = i.id AND p.status = 'confirmed'), 0)
		FROM invoices i
		WHERE i.user_id = $1 AND i.status IN ('pending','overdue')
		  AND (i.service_applied OR EXISTS (SELECT 1 FROM payments p WHERE p.invoice_id = i.id AND p.status = 'confirmed'))
		ORDER BY i.id`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var num, cur string
		var amount, paid float64
		var credit bool
		if err := rows.Scan(&num, &cur, &amount, &credit, &paid); err != nil {
			return err
		}
		switch {
		case credit && paid > 0:
			problems = append(problems, fmt.Sprintf("%s: service was started on credit and only %s %.2f of %.2f was received; collect the rest, or reverse the payment and cancel the invoice", num, cur, paid, amount))
		case credit:
			problems = append(problems, fmt.Sprintf("%s: service was started on credit and %s %.2f is unpaid; collect it, or cancel the invoice to write it off", num, cur, amount))
		default:
			problems = append(problems, fmt.Sprintf("%s: %s %.2f of %.2f received and not applied to service; refund it (reverse the payment) or collect the rest", num, cur, paid, amount))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &unsettledInvoicesError{msg: fmt.Sprintf("cannot delete %s until money on these invoices is settled (you can suspend the customer instead): %s",
		username, strings.Join(problems, "; "))}
}

// deleteRadiusUserTx removes a customer and everything FreeRADIUS knows about them.
func deleteRadiusUserTx(q dbtx, userID int, username string) error {
	if err := lockUser(q, userID); err != nil {
		return err
	}
	if err := checkSettled(q, userID, username); err != nil {
		return err
	}
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
	// Prepaid invoices nobody has paid anything on are withdrawn with the customer. Invoices
	// with money on them, or whose service was started on credit, stay open and unlinked for
	// staff to settle (refund by reversing the payment, or collect the debt).
	if _, err := q.Exec(`UPDATE invoices SET status = 'cancelled', updated_at = NOW(),
			notes = COALESCE(notes || E'\n', '') || 'Cancelled automatically: customer deleted'
		WHERE user_id = $1 AND status IN ('pending','overdue') AND NOT service_applied
		  AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.invoice_id = invoices.id AND p.status = 'confirmed')`, userID); err != nil {
		return err
	}
	_, err := q.Exec(`DELETE FROM radius_users WHERE id = $1`, userID)
	return err
}
