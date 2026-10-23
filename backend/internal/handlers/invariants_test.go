package handlers

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/freeradius-manager/backend/internal/database"
)

// checkMoney verifies the rules the books must obey at every moment, whatever happened
// before. It returns one line per violation.
//
//	M1 confirmed payments on an invoice never exceed its amount
//	M2 a paid invoice is fully paid; an open invoice is not
//	M3 a paid invoice for a customer's plan has had its period applied exactly once
//	   (service_applied), and an unpaid prepaid one has not (unless started on credit)
//	M4 receipt and invoice numbers are unique
//	M5 the revenue the Billing page shows (confirmed payments linked to invoices) plus
//	   standalone payments equals the Payments page total, per currency
func checkMoney(db *database.DB) []string {
	var bad []string
	add := func(format string, a ...interface{}) { bad = append(bad, fmt.Sprintf(format, a...)) }

	rows, err := db.Query(`
		SELECT i.id, i.invoice_number, i.amount, i.status, i.service_applied, i.applied_days, i.plan_id, i.user_id,
		       COALESCE((SELECT SUM(p.amount) FROM payments p WHERE p.invoice_id = i.id AND p.status = 'confirmed'), 0)
		FROM invoices i`)
	if err != nil {
		return []string{"query invoices: " + err.Error()}
	}
	defer rows.Close()
	for rows.Next() {
		var id, applied int
		var num, status string
		var amount, paid float64
		var svc bool
		var plan, user *int
		if err := rows.Scan(&id, &num, &amount, &status, &svc, &applied, &plan, &user, &paid); err != nil {
			return []string{"scan invoice: " + err.Error()}
		}
		if paid > amount+0.005 {
			add("M1 %s: paid %.2f exceeds invoice amount %.2f", num, paid, amount)
		}
		if status == "paid" && paid < amount-0.005 {
			add("M2 %s: marked paid but only %.2f of %.2f received", num, paid, amount)
		}
		if (status == "pending" || status == "overdue") && amount > 0 && paid >= amount-0.005 {
			add("M2 %s: fully paid (%.2f) but still %s", num, paid, status)
		}
		if status == "paid" && plan != nil && user != nil && !svc {
			add("M3 %s: paid plan invoice never granted its period", num)
		}
		if svc && applied <= 0 && status == "paid" && plan != nil && user != nil {
			add("M3 %s: period applied but applied_days=%d", num, applied)
		}
		if !svc && applied != 0 {
			add("M3 %s: applied_days=%d but service_applied is false", num, applied)
		}
	}

	var dupI, dupR int
	db.QueryRow(`SELECT COUNT(*) FROM (SELECT invoice_number FROM invoices GROUP BY 1 HAVING COUNT(*)>1) x`).Scan(&dupI)
	db.QueryRow(`SELECT COUNT(*) FROM (SELECT receipt_number FROM payments GROUP BY 1 HAVING COUNT(*)>1) x`).Scan(&dupR)
	if dupI+dupR > 0 {
		add("M4 duplicate numbers: %d invoice, %d receipt", dupI, dupR)
	}

	// M5: totals per currency, computed two independent ways.
	crow, err := db.Query(`SELECT currency,
		SUM(amount) FILTER (WHERE status='confirmed'),
		SUM(amount) FILTER (WHERE status='confirmed' AND invoice_id IS NOT NULL),
		SUM(amount) FILTER (WHERE status='confirmed' AND invoice_id IS NULL)
		FROM payments GROUP BY currency`)
	if err == nil {
		defer crow.Close()
		for crow.Next() {
			var cur string
			var all, linked, standalone *float64
			crow.Scan(&cur, &all, &linked, &standalone)
			a, l, s := deref0(all), deref0(linked), deref0(standalone)
			if diff := a - l - s; diff > 0.005 || diff < -0.005 {
				add("M5 %s: total %.2f != linked %.2f + standalone %.2f", cur, a, l, s)
			}
		}
	}
	return bad
}

func deref0(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// radiusDecision imitates what FreeRADIUS decides from a customer's radcheck rows alone at
// the database's idea of today: Auth-Type := Reject refuses; Expiration refuses on and after
// the stated midnight; a user with no rows at all is unknown and refused.
func radiusDecision(today time.Time, hasRows, reject bool, expiration *string) (allowed bool, why string) {
	switch {
	case !hasRows:
		return false, "no radcheck rows"
	case reject:
		return false, "Auth-Type Reject"
	case expiration != nil:
		exp, err := time.Parse(radiusDateLayout, *expiration)
		if err != nil {
			return false, "unparsable Expiration " + *expiration
		}
		// naive local midnight vs today's local date
		t0 := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
		if !t0.Before(exp) {
			return false, "Expiration " + *expiration + " reached"
		}
	}
	return true, ""
}

// accessFact is what the books say about one customer, next to what radcheck holds.
type accessFact struct {
	status                      string
	noExpiry, paidUp, neverPaid bool
	want                        bool // the books say: let them online
	allowed                     bool // radcheck alone (radiusDecision) says
	why                         string
}

// expectedAccess works out, for every customer, whether the books say they may get online:
// a customer paid up to today (counting grace days) and not suspended must get online; anyone
// else must not.
func expectedAccess(db *database.DB) (map[string]*accessFact, error) {
	var today time.Time
	db.QueryRow(`SELECT CURRENT_DATE`).Scan(&today)
	rows, err := db.Query(`SELECT ru.username, ru.status, ru.account_expiry IS NULL,
		COALESCE(ru.account_expiry + COALESCE(ru.grace_days, $1::int) >= CURRENT_DATE, FALSE),
		EXISTS (SELECT 1 FROM radcheck r WHERE r.username = ru.username),
		EXISTS (SELECT 1 FROM radcheck r WHERE r.username = ru.username AND r.attribute = 'Auth-Type' AND r.value = 'Reject'),
		(SELECT r.value FROM radcheck r WHERE r.username = ru.username AND r.attribute = 'Expiration' LIMIT 1),
		-- never been given a period: no invoice ever granted one, and not on a free plan. Such a
		-- customer waits, blocked, for the first payment; the paid-until date they carry is only
		-- a marker, so grace days must never count for them.
		ru.trial_started IS NULL AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.user_id = ru.id AND i.service_applied)
		  AND NOT EXISTS (SELECT 1 FROM user_plans up WHERE up.id = ru.plan_id AND up.price <= 0)
		FROM radius_users ru`, graceDays())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*accessFact{}
	for rows.Next() {
		var name string
		var f accessFact
		var hasRows, reject bool
		var expiration *string
		if err := rows.Scan(&name, &f.status, &f.noExpiry, &f.paidUp, &hasRows, &reject, &expiration, &f.neverPaid); err != nil {
			return nil, err
		}
		f.allowed, f.why = radiusDecision(today, hasRows, reject, expiration)
		switch {
		case f.status == "suspended":
			f.want = false
		case f.noExpiry:
			f.want = f.status == "active"
		case f.neverPaid && f.status == "expired":
			f.want = false
		default:
			f.want = f.paidUp
		}
		out[name] = &f
	}
	return out, rows.Err()
}

// checkEnforcement compares what FreeRADIUS would do (judged from radcheck) with what the
// books say should happen, for every customer.
func checkEnforcement(db *database.DB) []string {
	facts, err := expectedAccess(db)
	if err != nil {
		return []string{"expected access: " + err.Error()}
	}
	var bad []string
	for name, f := range facts {
		if f.allowed != f.want {
			bad = append(bad, fmt.Sprintf("E1 %s: status=%s paid_up=%v no_expiry=%v but RADIUS allows=%v (%s)",
				name, f.status, f.paidUp, f.noExpiry, f.allowed, f.why))
		}
	}
	sort.Strings(bad)
	return bad
}

func failIf(t *testing.T, problems []string) {
	t.Helper()
	if len(problems) > 0 {
		max := len(problems)
		if max > 15 {
			max = 15
		}
		t.Fatalf("%d invariant violation(s):\n  %s", len(problems), strings.Join(problems[:max], "\n  "))
	}
}
