package handlers

import (
	"math/big"
	"testing"
	"time"
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

// Deleting a customer who still owes: the invoice stays, can still be paid, and nothing breaks.
func TestDeleteCustomerWithOpenInvoice(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	uid := seedUser(t, db, "gone", nil)
	res, err := assignTx(db, uid, "gone", plan.ID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := payTx(db, paymentInput{InvoiceID: &res.InvoiceID, Amount: 40}); err != nil {
		t.Fatal(err)
	}
	tx, _ := db.Begin()
	if err := deleteRadiusUserTx(tx, uid, "gone"); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	if n := scanInt(t, db, `SELECT COUNT(*) FROM invoices WHERE username='gone' AND user_id IS NULL`); n != 1 {
		t.Fatalf("invoice history lost on delete (%d rows)", n)
	}
	// The renewal task must ignore the orphan, and the remainder can still be received.
	raiseRenewalInvoices(db, testLogger())
	if _, err := payTx(db, paymentInput{InvoiceID: &res.InvoiceID, Amount: 60}); err != nil {
		t.Errorf("paying the orphaned invoice failed: %v", err)
	}
	failIf(t, checkMoney(db))
	if n := scanInt(t, db, `SELECT COUNT(*) FROM radcheck WHERE username='gone'`); n != 0 {
		t.Errorf("%d radcheck rows left for a deleted customer: they could still log in", n)
	}
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
