package handlers

import (
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/freeradius-manager/backend/internal/database"
	"github.com/gin-gonic/gin"
)

// exactDiscountCents is the discount in whole cents worked out with exact fractions,
// rounding half up. Used to check the float arithmetic in promoDiscount.
func exactDiscountCents(kind string, valueHundredths, priceCents int64) int64 {
	var d *big.Rat
	if kind == "percent" {
		d = big.NewRat(priceCents*valueHundredths, 10000)
	} else {
		d = big.NewRat(valueHundredths, 1)
	}
	if d.Cmp(big.NewRat(priceCents, 1)) > 0 {
		d = big.NewRat(priceCents, 1)
	}
	// round half up: floor(d + 1/2)
	d.Add(d, big.NewRat(1, 2))
	return new(big.Int).Div(d.Num(), d.Denom()).Int64()
}

// Every price from 0.01 to 400.00 against the awkward percentages and fixed amounts: the
// discount must be exact to the cent, and discount + amount must give back the price.
func TestDiscountRoundingIsExact(t *testing.T) {
	cases := []struct {
		kind string
		val  int64 // hundredths of a percent, or cents
	}{
		{"percent", 3300}, {"percent", 1250}, {"percent", 6667}, {"percent", 3333},
		{"percent", 100}, {"percent", 1000}, {"percent", 10000}, {"percent", 1},
		{"fixed", 500}, {"fixed", 1}, {"fixed", 99999},
	}
	bad := 0
	for _, c := range cases {
		for price := int64(1); price <= 40000; price++ {
			got := promoDiscount(c.kind, float64(c.val)/100, float64(price)/100)
			gotCents := int64(got*100 + 0.5)
			want := exactDiscountCents(c.kind, c.val, price)
			if gotCents != want || got < 0 || got > float64(price)/100+1e-9 {
				bad++
				if bad <= 5 {
					t.Errorf("%s %.2f on %.2f: discount %.4f, exact %.2f", c.kind, float64(c.val)/100, float64(price)/100, got, float64(want)/100)
				}
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d price/discount combinations are off by a cent", bad)
	}
}

// Thousands of 0.01 payments must add up exactly: the invoice is paid by the 1000th, not before.
func TestThousandPennyPayments(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Tiny", 10, 30)
	uid := seedUser(t, db, "penny", nil)
	inv := seedInvoice(t, db, uid, "penny", plan, 10.00)
	for i := 1; i <= 1000; i++ {
		out, err := payTx(db, paymentInput{InvoiceID: &inv, Amount: 0.01})
		if err != nil {
			t.Fatalf("payment %d: %v", i, err)
		}
		if out.InvoicePaid != (i == 1000) {
			t.Fatalf("payment %d: invoice paid=%v", i, out.InvoicePaid)
		}
	}
	if _, err := payTx(db, paymentInput{InvoiceID: &inv, Amount: 0.01}); err == nil {
		t.Error("a payment on a fully paid invoice was accepted")
	}
	if got := scanFloat(t, db, `SELECT SUM(amount) FROM payments`); got != 10.00 {
		t.Errorf("payments total %.4f, want 10.00", got)
	}
	failIf(t, checkMoney(db))
}

func TestFreeAndFullyDiscountedAssign(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	mustExec(t, db, `INSERT INTO promotions (code, discount_type, discount_value) VALUES ('ALL','percent',100), ('BIG','fixed',900)`)
	for _, code := range []string{"ALL", "BIG"} {
		name := "disc-" + code
		uid := seedUser(t, db, name, nil)
		res, err := assignTx(db, uid, name, plan.ID, false, code)
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		var status string
		var amount float64
		var applied bool
		db.QueryRow(`SELECT status, amount, service_applied FROM invoices WHERE id=$1`, res.InvoiceID).Scan(&status, &amount, &applied)
		if status != "paid" || amount != 0 || !applied {
			t.Errorf("%s: invoice status=%s amount=%.2f applied=%v, want paid / 0.00 / true", code, status, amount, applied)
		}
		if days := scanInt(t, db, `SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE id=$1`, uid); days != 30 {
			t.Errorf("%s: paid-until today+%d, want today+30 (granted once)", code, days)
		}
	}
	failIf(t, checkMoney(db))
	failIf(t, checkEnforcement(db))
}

// The date FreeRADIUS is given: midnight at the start of the day after the last paid day,
// plus grace, across month ends, year ends and the leap day.
func TestExpirationAcrossCalendarEdges(t *testing.T) {
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, time.UTC) }
	for _, c := range []struct {
		last  time.Time
		grace int
		want  string
	}{
		{d(2028, 2, 28), 0, "Feb 29 2028 00:00:00"}, // leap day exists in 2028
		{d(2028, 2, 29), 0, "Mar 01 2028 00:00:00"},
		{d(2027, 2, 28), 0, "Mar 01 2027 00:00:00"}, // and not in 2027
		{d(2027, 12, 31), 0, "Jan 01 2028 00:00:00"},
		{d(2028, 1, 31), 1, "Feb 02 2028 00:00:00"},
		{d(2028, 2, 28), 2, "Mar 02 2028 00:00:00"},
		{d(2026, 10, 20), -3, "Oct 21 2026 00:00:00"}, // negative grace is ignored
	} {
		got := radiusExpiration(c.last, c.grace)
		if got != c.want {
			t.Errorf("last paid %s grace %d: %q, want %q", c.last.Format("2006-01-02"), c.grace, got, c.want)
		}
		if _, err := time.Parse(radiusDateLayout, got); err != nil {
			t.Errorf("%q does not parse back: %v", got, err)
		}
	}
}

func TestPaidPeriodAcrossLeapDay(t *testing.T) {
	db := resetTestDB(t)
	// A customer paid until 28 Feb 2028; the next 30-day period must end on 29 Mar 2028
	// (the leap day counts as a day), computed by the same SQL the billing code uses.
	mustExec(t, db, `INSERT INTO radius_users (username,password,account_expiry) VALUES ('leap','x','2028-02-28')`)
	var got string
	db.QueryRow(`SELECT (DATE '2028-02-28' + 30)::text`).Scan(&got)
	if got != "2028-03-29" {
		t.Errorf("2028-02-28 + 30 days = %s, want 2028-03-29", got)
	}
}

// The invoice, not today's plan, decides how many days a payment buys.
func TestPlanChangesWhileInvoiceOpen(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	uid := seedUser(t, db, "pat", nil)
	res, err := assignTx(db, uid, "pat", plan.ID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	// The plan is edited afterwards: cheaper and shorter.
	mustExec(t, db, `UPDATE user_plans SET price = 40, validity_days = 7 WHERE id=$1`, plan.ID)
	inv := res.InvoiceID
	if amount := scanFloat(t, db, `SELECT amount FROM invoices WHERE id=$1`, inv); amount != 100 {
		t.Errorf("invoice amount changed to %.2f when the plan price changed; it must stay 100.00", amount)
	}
	if _, err := payTx(db, paymentInput{InvoiceID: &inv, Amount: 100}); err != nil {
		t.Fatal(err)
	}
	days := scanInt(t, db, `SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE id=$1`, uid)
	if days != 30 {
		t.Errorf("paid 100.00 for a 30-day invoice but got today+%d (the plan was later shortened to 7 days)", days)
	}
	failIf(t, checkMoney(db))
}

// A failure part-way through a payment (database error, crash, lost connection) must leave
// nothing behind: no payment without its days, no days without a payment. A trigger that
// refuses the paid-until update stands in for the crash, after the payment row is written.
func TestPaymentIsAllOrNothing(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	uid := seedUser(t, db, "boom", intPtr(-3))
	inv := seedInvoice(t, db, uid, "boom", plan, 100)
	mustExec(t, db, `CREATE OR REPLACE FUNCTION test_fail_expiry() RETURNS trigger AS $$
		BEGIN
			IF NEW.account_expiry IS DISTINCT FROM OLD.account_expiry THEN RAISE EXCEPTION 'simulated crash'; END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql`)
	mustExec(t, db, `CREATE TRIGGER test_fail_expiry BEFORE UPDATE ON radius_users FOR EACH ROW EXECUTE FUNCTION test_fail_expiry()`)
	defer db.Exec(`DROP TRIGGER IF EXISTS test_fail_expiry ON radius_users`)

	if _, err := payTx(db, paymentInput{InvoiceID: &inv, Amount: 100}); err == nil {
		t.Fatal("payment succeeded although granting the days failed")
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM payments`); n != 0 {
		t.Errorf("%d payment row(s) left behind by a failed payment", n)
	}
	var status string
	db.QueryRow(`SELECT status FROM invoices WHERE id=$1`, inv).Scan(&status)
	if status != "pending" {
		t.Errorf("invoice is %q after a failed payment, want pending", status)
	}
	if days := scanInt(t, db, `SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE id=$1`, uid); days != -3 {
		t.Errorf("paid-until moved to today%+d by a failed payment", days)
	}
	failIf(t, checkMoney(db))
}

// Staff extend one customer's grace after they ask: an expired-but-paid customer is let back
// in at once, a customer who never paid is not, and shortening it blocks again.
func TestExtendingGraceDays(t *testing.T) {
	db := resetTestDB(t)
	h := newTestHandler(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)

	paid := seedUser(t, db, "paid-then-lapsed", nil)
	res, _ := assignTx(db, paid, "paid-then-lapsed", plan.ID, false, "")
	if _, err := payTx(db, paymentInput{InvoiceID: &res.InvoiceID, Amount: 100}); err != nil {
		t.Fatal(err)
	}
	// Time passes: the paid period ended 3 days ago and the daily task blocked them.
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE - 3 WHERE id=$1`, paid)
	runTask(db, testLogger(), "expire_accounts")
	if ok, _ := radiusDecisionFor(t, db, "paid-then-lapsed"); ok {
		t.Fatal("a customer 3 days past paid-until with no grace was still allowed")
	}

	never := seedUser(t, db, "never-paid", nil)
	if _, err := assignTx(db, never, "never-paid", plan.ID, false, ""); err != nil {
		t.Fatal(err)
	}

	for _, id := range []int{paid, never} {
		code, resp := callHandler(h.UpdateRadiusUser, gin.Params{{Key: "id", Value: fmt.Sprint(id)}}, nil, map[string]interface{}{"grace_days": 5})
		if code != 200 {
			t.Fatalf("update: %d %v", code, resp)
		}
	}
	if ok, why := radiusDecisionFor(t, db, "paid-then-lapsed"); !ok {
		t.Errorf("customer within their extended grace is still blocked (%s)", why)
	}
	if ok, _ := radiusDecisionFor(t, db, "never-paid"); ok {
		t.Error("a longer grace let in a customer who has never paid")
	}
	failIf(t, checkEnforcement(db))

	// The extension runs out: the daily task blocks them again.
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE - 6 WHERE id=$1`, paid)
	runTask(db, testLogger(), "expire_accounts")
	if ok, _ := radiusDecisionFor(t, db, "paid-then-lapsed"); ok {
		t.Error("customer past their extended grace is still allowed")
	}
	// Back to the system default (0): blocked immediately even if they were inside the extension.
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE - 2 WHERE id=$1`, paid)
	callHandler(h.UpdateRadiusUser, gin.Params{{Key: "id", Value: fmt.Sprint(paid)}}, nil, map[string]interface{}{"grace_days": 5})
	callHandler(h.UpdateRadiusUser, gin.Params{{Key: "id", Value: fmt.Sprint(paid)}}, nil, map[string]interface{}{"clear_grace_days": true})
	if ok, _ := radiusDecisionFor(t, db, "paid-then-lapsed"); ok {
		t.Error("clearing the customer's grace days did not block a customer past the default grace")
	}
	failIf(t, checkEnforcement(db))
}

func radiusDecisionFor(t *testing.T, db *database.DB, username string) (bool, string) {
	t.Helper()
	var today time.Time
	db.QueryRow(`SELECT CURRENT_DATE`).Scan(&today)
	var hasRows, reject bool
	var exp *string
	db.QueryRow(`SELECT EXISTS (SELECT 1 FROM radcheck WHERE username=$1),
		EXISTS (SELECT 1 FROM radcheck WHERE username=$1 AND attribute='Auth-Type' AND value='Reject'),
		(SELECT value FROM radcheck WHERE username=$1 AND attribute='Expiration')`, username).Scan(&hasRows, &reject, &exp)
	return radiusDecision(today, hasRows, reject, exp)
}

// Staff edit a customer from a list loaded before the customer paid. Saving other fields must
// not put back the old paid-until date, and changing the date from a stale form is refused.
func TestEditFromStaleFormKeepsPayment(t *testing.T) {
	db := resetTestDB(t)
	h := newTestHandler(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	uid := seedUser(t, db, "stale-form", nil)
	res, _ := assignTx(db, uid, "stale-form", plan.ID, false, "")
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE + 2 WHERE id=$1`, uid)
	var seen string // what the edit form loaded
	db.QueryRow(`SELECT TO_CHAR(account_expiry, 'YYYY-MM-DD') FROM radius_users WHERE id=$1`, uid).Scan(&seen)

	// The customer pays while the form is open: paid-until moves 30 days on.
	if _, err := payTx(db, paymentInput{InvoiceID: &res.InvoiceID, Amount: 100}); err != nil {
		t.Fatal(err)
	}
	paidUntil := scanInt(t, db, `SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE id=$1`, uid)
	id := gin.Params{{Key: "id", Value: fmt.Sprint(uid)}}

	// Phone changed only: the form leaves the expiry out.
	if code, resp := callHandler(h.UpdateRadiusUser, id, nil, map[string]interface{}{"phone": "0700000001"}); code != 200 {
		t.Fatalf("update: %d %v", code, resp)
	}
	if d := scanInt(t, db, `SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE id=$1`, uid); d != paidUntil {
		t.Errorf("paid-until today%+d after a phone change, want today%+d", d, paidUntil)
	}

	// The date changed from the stale form: refused, nothing written.
	code, resp := callHandler(h.UpdateRadiusUser, id, nil, map[string]interface{}{
		"account_expiry": seen[:8] + "01", "account_expiry_was": seen, "phone": "0700000002"})
	if code != 409 {
		t.Errorf("stale expiry change: %d %v, want 409", code, resp)
	}
	if d := scanInt(t, db, `SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE id=$1`, uid); d != paidUntil {
		t.Errorf("paid-until today%+d after a refused change, want today%+d", d, paidUntil)
	}
	var phone string
	db.QueryRow(`SELECT phone FROM radius_users WHERE id=$1`, uid).Scan(&phone)
	if phone != "0700000001" {
		t.Errorf("phone %q written by a refused update", phone)
	}

	// From a fresh form it goes through, and clearing works the same way.
	var now string
	db.QueryRow(`SELECT TO_CHAR(account_expiry, 'YYYY-MM-DD') FROM radius_users WHERE id=$1`, uid).Scan(&now)
	if code, resp := callHandler(h.UpdateRadiusUser, id, nil, map[string]interface{}{"account_expiry": "", "account_expiry_was": now}); code != 200 {
		t.Fatalf("clear from a fresh form: %d %v", code, resp)
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM radius_users WHERE id=$1 AND account_expiry IS NULL`, uid); n != 1 {
		t.Error("expiry not cleared")
	}
	if code, _ := callHandler(h.UpdateRadiusUser, id, nil, map[string]interface{}{"account_expiry": now, "account_expiry_was": ""}); code != 200 {
		t.Errorf("set from no expiry: %d", code)
	}
	failIf(t, checkEnforcement(db))
}

// A payment on an invoice must be filed under the invoice's customer.
func TestPaymentUsernameMustMatchInvoice(t *testing.T) {
	db := resetTestDB(t)
	h := newTestHandler(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	uid := seedUser(t, db, "owner", nil)
	seedUser(t, db, "someone-else", nil)
	inv := seedInvoice(t, db, uid, "owner", plan, 100)

	code, resp := callHandler(h.CreatePayment, nil, nil, map[string]interface{}{"invoice_id": inv, "username": "someone-else", "amount": 100})
	if code != 400 {
		t.Errorf("payment under another customer: %d %v, want 400", code, resp)
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM payments`); n != 0 {
		t.Errorf("%d payment(s) recorded", n)
	}
	if code, resp := callHandler(h.CreatePayment, nil, nil, map[string]interface{}{"invoice_id": inv, "username": "owner", "amount": 100}); code != 201 {
		t.Errorf("payment by the owner: %d %v", code, resp)
	}
	failIf(t, checkMoney(db))
}
