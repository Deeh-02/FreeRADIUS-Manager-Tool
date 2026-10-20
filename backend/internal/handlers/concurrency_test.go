package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/freeradius-manager/backend/internal/auth"
	"github.com/freeradius-manager/backend/internal/database"
	"github.com/gin-gonic/gin"
)

// These tests fire the same money operations at the same instant and then check the books.
// Run them with: TEST_DATABASE_URL=... go test ./internal/handlers -run Concurrent -count=1

func TestConcurrentPartialPayments(t *testing.T) {
	for _, tc := range []struct {
		name         string
		count        int
		each         float64
		wantAccepted int
		wantPaid     int32
	}{
		{"20 payments that exactly fill the invoice", 20, 5.00, 20, 1},
		{"30 payments, only 20 fit", 30, 5.00, 20, 1},
		{"7 payments of 14.29, the 7th would overpay", 7, 14.29, 6, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := resetTestDB(t)
			plan := seedPlan(t, db, "Monthly", 100, 30)
			uid := seedUser(t, db, "alice", nil)
			inv := seedInvoice(t, db, uid, "alice", plan, 100)

			var ok, paidFlags int32
			parallel(tc.count, func(i int) {
				out, err := payTx(db, paymentInput{InvoiceID: &inv, Amount: tc.each, PaymentMethod: "cash"})
				if err == nil {
					atomic.AddInt32(&ok, 1)
					if out.InvoicePaid {
						atomic.AddInt32(&paidFlags, 1)
					}
				}
			})

			if int(ok) != tc.wantAccepted {
				t.Errorf("accepted %d payments, want %d", ok, tc.wantAccepted)
			}
			if paidFlags != tc.wantPaid {
				t.Errorf("%d payments reported 'invoice paid', want %d", paidFlags, tc.wantPaid)
			}
			failIf(t, checkMoney(db))
			if tc.wantPaid == 1 {
				if days := scanInt(t, db, `SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE id=$1`, uid); days != 30 {
					t.Errorf("paid-until is today+%d, want today+30 (plan days added exactly once)", days)
				}
			}
			failIf(t, checkEnforcement(db))
		})
	}
}

func TestConcurrentSameReference(t *testing.T) {
	for _, gateway := range []string{"mpesa", ""} {
		t.Run("gateway="+gateway, func(t *testing.T) {
			db := resetTestDB(t)
			plan := seedPlan(t, db, "Monthly", 100, 30)
			uid := seedUser(t, db, "bob", nil)
			inv := seedInvoice(t, db, uid, "bob", plan, 100)

			var ok int32
			parallel(10, func(i int) {
				_, err := payTx(db, paymentInput{InvoiceID: &inv, Amount: 25, PaymentMethod: "mobile_money",
					Gateway: gateway, GatewayRef: "QGH7XK29LP"})
				if err == nil {
					atomic.AddInt32(&ok, 1)
				}
			})
			if ok != 1 {
				t.Errorf("same reference accepted %d times, want exactly 1", ok)
			}
			if n := scanInt(t, db, `SELECT COUNT(*) FROM payments`); n != 1 {
				t.Errorf("%d payment rows recorded, want 1", n)
			}
			failIf(t, checkMoney(db))
		})
	}

	t.Run("standalone payments (no invoice)", func(t *testing.T) {
		db := resetTestDB(t)
		seedUser(t, db, "carol", nil)
		var ok int32
		parallel(10, func(i int) {
			_, err := payTx(db, paymentInput{Username: "carol", Amount: 10, PaymentMethod: "mobile_money",
				Gateway: "", GatewayRef: "REF-123"})
			if err == nil {
				atomic.AddInt32(&ok, 1)
			}
		})
		if ok != 1 {
			t.Errorf("same reference accepted %d times, want exactly 1", ok)
		}
	})
}

// A reference must also stay unique after the payment is reversed and re-entered: the
// reversed row is history, but a second confirmed payment with the same reference is a
// genuine duplicate only while the first is still confirmed.
func TestReferenceReusableAfterReversal(t *testing.T) {
	db := resetTestDB(t)
	seedUser(t, db, "dave", nil)
	out, err := payTx(db, paymentInput{Username: "dave", Amount: 10, Gateway: "mpesa", GatewayRef: "R1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reverseTx(db, out.PaymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := payTx(db, paymentInput{Username: "dave", Amount: 10, Gateway: "mpesa", GatewayRef: "R1"}); err != nil {
		t.Errorf("re-entering a reversed reference failed: %v", err)
	}
}

func TestConcurrentPayAndReverse(t *testing.T) {
	for iter := 0; iter < 40; iter++ {
		db := resetTestDB(t)
		plan := seedPlan(t, db, "Monthly", 100, 30)
		uid := seedUser(t, db, "erin", intPtr(-5)) // lapsed 5 days ago
		inv := seedInvoice(t, db, uid, "erin", plan, 100)

		first, err := payTx(db, paymentInput{InvoiceID: &inv, Amount: 50})
		if err != nil {
			t.Fatal(err)
		}
		// Race: reverse the first half against paying the second half.
		parallel(2, func(i int) {
			if i == 0 {
				reverseTx(db, first.PaymentID)
			} else {
				payTx(db, paymentInput{InvoiceID: &inv, Amount: 50})
			}
		})
		assertLedgerMatchesExpiry(t, db, uid, -5, fmt.Sprintf("iteration %d", iter))
	}

	// Fully paid invoice: reverse it while someone tries to pay it again.
	for iter := 0; iter < 40; iter++ {
		db := resetTestDB(t)
		plan := seedPlan(t, db, "Monthly", 100, 30)
		uid := seedUser(t, db, "frank", intPtr(-5))
		inv := seedInvoice(t, db, uid, "frank", plan, 100)
		first, err := payTx(db, paymentInput{InvoiceID: &inv, Amount: 100})
		if err != nil {
			t.Fatal(err)
		}
		parallel(3, func(i int) {
			if i == 0 {
				reverseTx(db, first.PaymentID)
			} else {
				payTx(db, paymentInput{InvoiceID: &inv, Amount: 100})
			}
		})
		assertLedgerMatchesExpiry(t, db, uid, -5, fmt.Sprintf("full-pay iteration %d", iter))
	}
}

// assertLedgerMatchesExpiry: the paid-until date must equal the starting date plus the days of
// every invoice that currently holds a granted period (the surviving payments).
func assertLedgerMatchesExpiry(t *testing.T, db dbtx, uid, startOffset int, label string) {
	t.Helper()
	got := scanInt(t, db, `SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE id=$1`, uid)
	granted := scanInt(t, db, `SELECT COALESCE(SUM(applied_days),0) FROM invoices WHERE user_id=$1 AND service_applied`, uid)
	if want := startOffset + granted; got != want {
		t.Errorf("%s: paid-until is today%+d but start(%+d) + granted days(%d) = today%+d", label, got, startOffset, granted, want)
	}
	// An invoice holds a period exactly when it is paid.
	if n := scanInt(t, db, `SELECT COUNT(*) FROM invoices WHERE user_id=$1 AND (status='paid') <> service_applied`, uid); n > 0 {
		t.Errorf("%s: %d invoice(s) where paid and period-granted disagree", label, n)
	}
	for _, p := range checkMoney(db.(*database.DB)) {
		t.Errorf("%s: %s", label, p)
	}
}

func TestConcurrentPromoLimit(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	mustExec(t, db, `INSERT INTO promotions (code, discount_type, discount_value, max_uses) VALUES ('TEN','percent',50,10)`)
	ids := make([]int, 100)
	for i := range ids {
		ids[i] = seedUser(t, db, fmt.Sprintf("promo%03d", i), nil)
	}
	var ok int32
	parallel(100, func(i int) {
		if _, err := assignTx(db, ids[i], fmt.Sprintf("promo%03d", i), plan.ID, false, "TEN"); err == nil {
			atomic.AddInt32(&ok, 1)
		}
	})
	if ok != 10 {
		t.Errorf("%d customers got the promo, want exactly 10", ok)
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM invoices WHERE promo_code='TEN'`); n != 10 {
		t.Errorf("%d discounted invoices, want 10", n)
	}
	if n := scanInt(t, db, `SELECT uses_count FROM promotions WHERE code='TEN'`); n != 10 {
		t.Errorf("uses_count=%d, want 10", n)
	}
	failIf(t, checkMoney(db))
}

// Renewal task running while staff assign plans: at most one open invoice per customer+plan.
func TestConcurrentRenewalAndAssign(t *testing.T) {
	for round := 0; round < 5; round++ {
		db := resetTestDB(t)
		plan := seedPlan(t, db, "Monthly", 100, 30)
		const n = 30
		ids := make([]int, n)
		for i := range ids {
			ids[i] = seedUser(t, db, fmt.Sprintf("ren%02d", i), intPtr(2)) // expires in 2 days: due for renewal
			mustExec(t, db, `UPDATE radius_users SET plan_id=$1 WHERE id=$2`, plan.ID, ids[i])
		}
		parallel(n*3, func(i int) {
			u := i % n
			switch i / n {
			case 0:
				raiseRenewalInvoices(db, testLogger())
			case 1:
				assignTx(db, ids[u], fmt.Sprintf("ren%02d", u), plan.ID, false, "")
			default:
				raiseRenewalInvoices(db, testLogger())
			}
		})
		rows, err := db.Query(`SELECT username, COUNT(*) FROM invoices WHERE status IN ('pending','overdue')
			GROUP BY username, plan_id HAVING COUNT(*) > 1`)
		if err != nil {
			t.Fatal(err)
		}
		dups := 0
		for rows.Next() {
			var u string
			var c int
			rows.Scan(&u, &c)
			dups++
			if dups <= 3 {
				t.Errorf("round %d: %s has %d open invoices for one plan", round, u, c)
			}
		}
		rows.Close()
		if dups > 0 {
			t.Fatalf("round %d: %d customers with duplicate open invoices", round, dups)
		}
		failIf(t, checkMoney(db))
	}
}

// Two staff clicking "Assign plan" for one customer at once.
func TestConcurrentDoubleAssign(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	uid := seedUser(t, db, "gina", nil)
	parallel(10, func(i int) { assignTx(db, uid, "gina", plan.ID, false, "") })
	if n := scanInt(t, db, `SELECT COUNT(*) FROM invoices WHERE user_id=$1`, uid); n != 1 {
		t.Errorf("%d invoices after 10 simultaneous assignments, want 1", n)
	}
}

// Many different invoices paid at once, plus new invoices: numbers never repeat, nothing errors.
func TestConcurrentDocumentNumbers(t *testing.T) {
	db := resetTestDB(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	const n = 200
	ids := make([]int, n)
	for i := range ids {
		uid := seedUser(t, db, fmt.Sprintf("num%03d", i), nil)
		ids[i] = seedInvoice(t, db, uid, fmt.Sprintf("num%03d", i), plan, 100)
	}
	var errs int32
	parallel(n, func(i int) {
		if _, err := payTx(db, paymentInput{InvoiceID: &ids[i], Amount: 100}); err != nil {
			atomic.AddInt32(&errs, 1)
			t.Logf("payment %d: %v", i, err)
		}
		uid := scanInt(t, db, `SELECT id FROM radius_users WHERE username=$1`, fmt.Sprintf("num%03d", i))
		tx, _ := db.Begin()
		_, err := createInvoiceTx(tx, newInvoice{UserID: &uid, Username: fmt.Sprintf("num%03d", i), Amount: 5})
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if err != nil {
			atomic.AddInt32(&errs, 1)
			t.Logf("invoice %d: %v", i, err)
		}
	})
	if errs > 0 {
		t.Errorf("%d operations failed under parallel load", errs)
	}
	failIf(t, checkMoney(db))
	if c := scanInt(t, db, `SELECT COUNT(*) FROM payments`); c != n {
		t.Errorf("%d payments, want %d", c, n)
	}
}

// ── HTTP-level helpers ───────────────────────────────────────────────────────

func newTestHandler(t testing.TB) *Handler {
	db := openTestDB(t)
	gin.SetMode(gin.TestMode)
	return &Handler{db: db, log: testLogger()}
}

// callHandler invokes a gin handler as the staff user (id 1) and returns status + decoded body.
func callHandler(fn gin.HandlerFunc, params gin.Params, headers map[string]string, body interface{}) (int, map[string]interface{}) {
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	c.Params = params
	c.Set("claims", &auth.Claims{UserID: 1, Username: "staff", Role: "admin"})
	fn(c)
	out := map[string]interface{}{}
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// A browser or network that retries a cash payment (which has no bank reference) must not
// charge the customer twice.
func TestIdempotentPaymentRetry(t *testing.T) {
	db := resetTestDB(t)
	h := newTestHandler(t)
	plan := seedPlan(t, db, "Monthly", 100, 30)
	uid := seedUser(t, db, "hank", nil)
	inv := seedInvoice(t, db, uid, "hank", plan, 100)

	var created, replayed int32
	parallel(10, func(i int) {
		code, _ := callHandler(h.CreatePayment, nil, map[string]string{"Idempotency-Key": "retry-key-0001"},
			map[string]interface{}{"invoice_id": inv, "amount": 25, "payment_method": "cash"})
		switch code {
		case http.StatusCreated:
			atomic.AddInt32(&created, 1)
		case http.StatusOK:
			atomic.AddInt32(&replayed, 1)
		}
	})
	if n := scanInt(t, db, `SELECT COUNT(*) FROM payments`); n != 1 {
		t.Errorf("%d payments recorded for one idempotency key, want 1 (created=%d replayed=%d)", n, created, replayed)
	}
	if created != 1 {
		t.Errorf("%d requests answered 201, want exactly 1", created)
	}

	// A different key is a different payment.
	code, _ := callHandler(h.CreatePayment, nil, map[string]string{"Idempotency-Key": "retry-key-0002"},
		map[string]interface{}{"invoice_id": inv, "amount": 25})
	if code != http.StatusCreated {
		t.Errorf("new key got status %d, want 201", code)
	}
}

// Cancelling or re-pricing an invoice while a payment lands on it.
func TestConcurrentCancelAndPay(t *testing.T) {
	h := newTestHandler(t)
	for iter := 0; iter < 30; iter++ {
		db := resetTestDB(t)
		plan := seedPlan(t, db, "Monthly", 100, 30)
		uid := seedUser(t, db, "ivy", nil)
		inv := seedInvoice(t, db, uid, "ivy", plan, 100)
		parallel(2, func(i int) {
			if i == 0 {
				callHandler(h.UpdateInvoice, gin.Params{{Key: "id", Value: fmt.Sprint(inv)}}, nil,
					map[string]interface{}{"status": "cancelled"})
			} else {
				payTx(db, paymentInput{InvoiceID: &inv, Amount: 100})
			}
		})
		status := ""
		db.QueryRow(`SELECT status FROM invoices WHERE id=$1`, inv).Scan(&status)
		paid := scanFloat(t, db, `SELECT COALESCE(SUM(amount),0) FROM payments WHERE invoice_id=$1 AND status='confirmed'`, inv)
		if status == "cancelled" && paid > 0 {
			t.Fatalf("iteration %d: invoice cancelled but %.2f of confirmed payments sit on it", iter, paid)
		}
		failIf(t, checkMoney(db))
	}
}

// Two backend copies wake up together, both read the same list of due tasks, and both try to
// run them. Each task must run once.
func TestSchedulerTwoCopiesRunEachTaskOnce(t *testing.T) {
	for round := 0; round < 20; round++ {
		db := resetTestDB(t)
		var runs sync.Map // task type -> *int32
		taskRunHook = func(tt string) {
			c, _ := runs.LoadOrStore(tt, new(int32))
			atomic.AddInt32(c.(*int32), 1)
		}
		listA, errA := listDueTasks(db)
		listB, errB := listDueTasks(db)
		if errA != nil || errB != nil || len(listA) != 3 || len(listB) != 3 {
			t.Fatalf("due lists: %v %v %d %d", errA, errB, len(listA), len(listB))
		}
		// Copy A runs everything before copy B starts: the worst case for a lock that is only
		// held while a task is running.
		runTaskList(db, testLogger(), listA)
		runTaskList(db, testLogger(), listB)
		taskRunHook = nil
		runs.Range(func(k, v interface{}) bool {
			if n := *v.(*int32); n != 1 {
				t.Errorf("round %d: task %v ran %d times, want 1", round, k, n)
			}
			return true
		})

		// And both copies at the same moment.
		mustExec(t, db, `UPDATE scheduled_tasks SET next_run = NULL`)
		runs = sync.Map{}
		taskRunHook = func(tt string) {
			c, _ := runs.LoadOrStore(tt, new(int32))
			atomic.AddInt32(c.(*int32), 1)
		}
		listA, _ = listDueTasks(db)
		listB, _ = listDueTasks(db)
		parallel(2, func(i int) {
			if i == 0 {
				runTaskList(db, testLogger(), listA)
			} else {
				runTaskList(db, testLogger(), listB)
			}
		})
		taskRunHook = nil
		runs.Range(func(k, v interface{}) bool {
			if n := *v.(*int32); n != 1 {
				t.Errorf("round %d (simultaneous): task %v ran %d times, want 1", round, k, n)
			}
			return true
		})
	}
}
