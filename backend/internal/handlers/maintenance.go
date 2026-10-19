package handlers

import (
	"database/sql"
	"fmt"
	"io"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Command-line maintenance: `radius-manager access-report` and `radius-manager resync-access`
// ─────────────────────────────────────────────────────────────────────────────
//
// FreeRADIUS only enforces radcheck rows. Before version 1.1 a customer's status and expiry
// were labels in radius_users that nothing enforced, so an upgraded database can hold
// customers marked "expired" or "suspended", or past their expiry date, who still get online.
// resync-access writes the enforcement rows (syncAccess) for every customer; access-report
// shows beforehand who will be refused, so the operator can extend anyone who has paid.
// deploy-update.sh runs both.

type accessReportRow struct {
	Username string
	Status   string
	Expiry   sql.NullTime
	Reason   string
}

// AccessReport writes who will be refused once enforcement rows are synced, and returns
// how many.
func AccessReport(db *sql.DB, w io.Writer) (int, error) {
	grace := graceDays()
	rows, err := db.Query(`
		SELECT username, status, account_expiry,
		       CASE WHEN status IN ('suspended','expired') THEN 'status is ' || status
		            ELSE 'paid-until date has passed' END
		FROM radius_users
		WHERE status IN ('suspended','expired')
		   OR (account_expiry IS NOT NULL AND account_expiry + $1::int < CURRENT_DATE)
		ORDER BY account_expiry NULLS FIRST, username`, grace)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var list []accessReportRow
	for rows.Next() {
		var r accessReportRow
		if err := rows.Scan(&r.Username, &r.Status, &r.Expiry, &r.Reason); err != nil {
			return 0, err
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var total int
	db.QueryRow(`SELECT COUNT(*) FROM radius_users`).Scan(&total)
	fmt.Fprintf(w, "Customers: %d. Refused after sync: %d (grace days: %d).\n", total, len(list), grace)
	if len(list) == 0 {
		return 0, nil
	}
	fmt.Fprintf(w, "%-32s %-10s %-12s %s\n", "USERNAME", "STATUS", "PAID UNTIL", "REASON")
	const maxShown = 500
	for i, r := range list {
		if i == maxShown {
			fmt.Fprintf(w, "... and %d more\n", len(list)-maxShown)
			break
		}
		exp := "-"
		if r.Expiry.Valid {
			exp = r.Expiry.Time.Format("2006-01-02")
		}
		fmt.Fprintf(w, "%-32s %-10s %-12s %s\n", r.Username, r.Status, exp, r.Reason)
	}
	fmt.Fprintln(w, "To keep someone online, extend their paid-until date or record their payment before syncing.")
	return len(list), nil
}

// ResyncAllAccess marks active customers past their paid-until date (plus grace) as expired,
// then rewrites every customer's enforcement rows. Each customer is its own transaction, so
// one bad row does not stop the rest. It does not disconnect sessions that are already open;
// the router drops them at their next re-authentication, or use Disconnect in the UI.
func ResyncAllAccess(db *sql.DB, w io.Writer) (synced, failed int, err error) {
	res, err := db.Exec(`
		UPDATE radius_users SET status='expired', updated_at=NOW()
		WHERE status='active' AND account_expiry IS NOT NULL
		  AND account_expiry + $1::int < CURRENT_DATE`, graceDays())
	if err != nil {
		return 0, 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Fprintf(w, "Marked %d customer(s) past their paid-until date as expired.\n", n)
	}

	rows, err := db.Query(`SELECT username FROM radius_users ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}
	var names []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			names = append(names, u)
		}
	}
	rows.Close()

	start := time.Now()
	for _, u := range names {
		tx, err := db.Begin()
		if err == nil {
			if err = syncAccess(tx, u); err == nil {
				err = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
		if err != nil {
			failed++
			fmt.Fprintf(w, "  %s: %v\n", u, err)
			continue
		}
		synced++
	}
	fmt.Fprintf(w, "Synced %d customer(s), %d failed, in %s.\n", synced, failed, time.Since(start).Round(time.Millisecond))
	return synced, failed, nil
}
