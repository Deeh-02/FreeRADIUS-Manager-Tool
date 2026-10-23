package handlers

import (
	"errors"
	"fmt"
	"testing"

	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/gin-gonic/gin"
)

// A customer with money still on an open invoice cannot be deleted until staff settle it on
// purpose; afterwards the delete goes through and the history survives.
func TestDeleteRefusedWhileMoneyUnsettled(t *testing.T) {
	db := resetTestDB(t)
	h := newTestHandler(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	uid := seedUser(t, db, "gone", nil)
	res, err := assignTx(db, uid, "gone", plan.ID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	pay, err := payTx(db, paymentInput{InvoiceID: &res.InvoiceID, Amount: 40})
	if err != nil {
		t.Fatal(err)
	}

	del := func() int {
		code, _ := callHandler(h.DeleteRadiusUser, gin.Params{{Key: "id", Value: fmt.Sprint(uid)}}, nil, nil)
		return code
	}
	if code := del(); code != 409 {
		t.Fatalf("deleting a customer with 40 received on an open invoice returned %d, want 409", code)
	}
	if scanInt(t, db, `SELECT COUNT(*) FROM radius_users WHERE id=$1`, uid) != 1 {
		t.Fatal("the customer was deleted despite the refusal")
	}
	// Refund (reverse) the payment: now it can go.
	if _, err := reverseTx(db, pay.PaymentID); err != nil {
		t.Fatal(err)
	}
	if code := del(); code != 200 {
		t.Fatalf("delete after refunding returned %d, want 200", code)
	}
	var status string
	db.QueryRow(`SELECT status FROM invoices WHERE username='gone'`).Scan(&status)
	if status != "cancelled" {
		t.Errorf("the now-unpaid invoice is %q, want cancelled with the customer", status)
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM radcheck WHERE username='gone'`); n != 0 {
		t.Errorf("%d radcheck rows left for a deleted customer: they could still log in", n)
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM payments WHERE username='gone'`); n != 1 {
		t.Errorf("payment history lost on delete (%d rows)", n)
	}
	failIf(t, checkMoney(db))
}

// Service started on credit means money is owed: refuse until staff collect or write it off.
func TestDeleteRefusedForCreditInvoice(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	uid := seedUser(t, db, "owes", nil)
	res, err := assignTx(db, uid, "owes", plan.ID, true, "") // Start now
	if err != nil {
		t.Fatal(err)
	}
	del := func() error {
		tx, _ := db.Begin()
		defer tx.Rollback()
		if err := deleteRadiusUserTx(tx, uid, "owes"); err != nil {
			return err
		}
		return tx.Commit()
	}
	var unsettled *unsettledInvoicesError
	if err := del(); !errors.As(err, &unsettled) {
		t.Fatalf("delete of a customer owing for credit service: %v, want a refusal", err)
	}
	// Staff write the debt off on purpose by cancelling the invoice.
	mustExec(t, db, `UPDATE invoices SET status='cancelled' WHERE id=$1`, res.InvoiceID)
	if err := del(); err != nil {
		t.Errorf("delete after writing the debt off: %v", err)
	}
}

// Prepaid and nothing paid: the unpaid invoice is just an offer, withdrawn with the customer.
func TestDeleteCancelsUnpaidPrepaidInvoice(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	uid := seedUser(t, db, "leaver", nil)
	assignTx(db, uid, "leaver", plan.ID, false, "")
	tx, _ := db.Begin()
	defer tx.Rollback()
	if err := deleteRadiusUserTx(tx, uid, "leaver"); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	var status string
	db.QueryRow(`SELECT status FROM invoices WHERE username='leaver'`).Scan(&status)
	if status != "cancelled" {
		t.Errorf("unpaid prepaid invoice is %q after deleting the customer, want cancelled", status)
	}
	failIf(t, checkMoney(db))
}

// Free trial: the team sets trial_days; a brand-new customer is online until the trial ends,
// the first invoice is due then, nobody gets a second trial, and paying keeps the unused days.
func TestFreeTrialForNewCustomers(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	other := seedPlan(t, db, "Other", 50, 30)
	days := func(uid int) int {
		return scanInt(t, db, `SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE id=$1`, uid)
	}
	allowed := func(name string) bool { ok, _ := radiusDecisionFor(t, db, name); return ok }

	// No trial configured: blocked until paid (the default).
	a := seedUser(t, db, "no-trial", nil)
	assignTx(db, a, "no-trial", plan.ID, false, "")
	if allowed("no-trial") {
		t.Error("with trial_days=0 a new customer was let on before paying")
	}

	// The team sets 5 days on the Settings page.
	mustExec(t, db, `INSERT INTO system_settings (key, value) VALUES ('trial_days','5')`)
	settings.Invalidate()

	b := seedUser(t, db, "newbie", nil)
	res, err := assignTx(db, b, "newbie", plan.ID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed("newbie") || days(b) != 5 {
		t.Errorf("new customer: allowed=%v paid-until today%+d, want allowed until today+5", allowed("newbie"), days(b))
	}
	if due := scanInt(t, db, `SELECT due_date - CURRENT_DATE FROM invoices WHERE id=$1`, res.InvoiceID); due != 5 {
		t.Errorf("first invoice due today%+d, want the trial end (today+5)", due)
	}
	// Once the trial has run out, switching plan does not restart it.
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE - 2 WHERE id=$1`, b)
	runTask(db, testLogger(), "expire_accounts")
	assignTx(db, b, "newbie", other.ID, false, "")
	if allowed("newbie") {
		t.Error("a customer whose trial ended got a second trial by changing plan")
	}

	// Paying during the trial keeps the unused trial days on top of the paid period.
	c := seedUser(t, db, "early-payer", nil)
	r, _ := assignTx(db, c, "early-payer", plan.ID, false, "")
	if _, err := payTx(db, paymentInput{InvoiceID: &r.InvoiceID, Amount: 100}); err != nil {
		t.Fatal(err)
	}
	if got := days(c); got != 35 {
		t.Errorf("paid on day 0 of a 5-day trial: paid-until today%+d, want today+35", got)
	}
	// ...and reversing that payment takes back exactly the paid period.
	var pid int
	db.QueryRow(`SELECT id FROM payments WHERE username='early-payer'`).Scan(&pid)
	reverseTx(db, pid)
	if got := days(c); got != 5 {
		t.Errorf("after reversing: paid-until today%+d, want back to the trial end (today+5)", got)
	}

	// A customer who has paid before does not get a trial when they change plan.
	d := seedUser(t, db, "regular", intPtr(-10))
	rd, _ := assignTx(db, d, "regular", plan.ID, true, "") // paid history via Start now
	payTx(db, paymentInput{InvoiceID: &rd.InvoiceID, Amount: 100})
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE - 3 WHERE id=$1`, d)
	runTask(db, testLogger(), "expire_accounts")
	assignTx(db, d, "regular", other.ID, false, "")
	if allowed("regular") {
		t.Error("an existing paying customer was given a free trial")
	}
	failIf(t, checkMoney(db))
	failIf(t, checkEnforcement(db))
}

// Staff choose the trial length per customer when assigning a plan: a number overrides the
// system default either way (longer, shorter, or none).
func TestTrialLengthChosenPerCustomer(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	days := func(uid int) int {
		return scanInt(t, db, `SELECT COALESCE(account_expiry - CURRENT_DATE, -999) FROM radius_users WHERE id=$1`, uid)
	}
	assign := func(name string, trial *int) int {
		uid := seedUser(t, db, name, nil)
		tx, _ := db.Begin()
		defer tx.Rollback()
		if _, err := assignPlanTx(tx, uid, name, plan.ID, false, "", nil, trial); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		return uid
	}

	// System default is off: staff can still grant a trial to one customer.
	if got := days(assign("granted-7", intPtr(7))); got != 7 {
		t.Errorf("staff-chosen 7-day trial with default off: paid-until today%+d, want today+7", got)
	}
	if ok, _ := radiusDecisionFor(t, db, "granted-7"); !ok {
		t.Error("a customer given a 7-day trial is not online")
	}
	// Default 5 days: one customer gets 10, one gets none, one the default.
	mustExec(t, db, `INSERT INTO system_settings (key, value) VALUES ('trial_days','5')`)
	settings.Invalidate()
	if got := days(assign("longer", intPtr(10))); got != 10 {
		t.Errorf("override 10 over default 5: today%+d, want today+10", got)
	}
	if got := days(assign("default", nil)); got != 5 {
		t.Errorf("no override: today%+d, want the default today+5", got)
	}
	none := assign("none", intPtr(0))
	if ok, _ := radiusDecisionFor(t, db, "none"); ok {
		t.Error("override 0 should mean no trial, but the customer is online")
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM radius_users WHERE id=$1 AND trial_started IS NOT NULL`, none); n != 0 {
		t.Error("a customer refused a trial was marked as having used it")
	}
	failIf(t, checkEnforcement(db))
}
