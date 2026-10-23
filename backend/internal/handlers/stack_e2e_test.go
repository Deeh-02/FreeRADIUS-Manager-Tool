package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeradius-manager/backend/internal/auth"
	"github.com/freeradius-manager/backend/internal/database"
)

// ─────────────────────────────────────────────────────────────────────────────
// The whole system: real backend binary + real FreeRADIUS + PostgreSQL
// ─────────────────────────────────────────────────────────────────────────────
//
// These tests talk to the backend over HTTP exactly as the web UI does, crash and restart its
// parts (scripts/test-stack.sh), and then check the books and ask FreeRADIUS. They read the
// database directly only to check results. Skipped unless TEST_API_URL is set:
//
//	eval "$(scripts/test-stack.sh start)"
//	go test ./internal/handlers -run Stack -count=1 -v
//	scripts/test-stack.sh stop

const stackAdminPassword = "Stack-test-pass-1"

type stackAPI struct {
	t     testing.TB
	base  string
	token string
	http  *http.Client
}

// needStack empties the data (keeping system settings and scheduled tasks, which the running
// backend relies on), creates a super admin and logs in.
func needStack(t *testing.T) (*stackAPI, *database.DB) {
	t.Helper()
	base := os.Getenv("TEST_API_URL")
	if base == "" {
		t.Skip("TEST_API_URL not set; skipping whole-stack test (scripts/test-stack.sh start)")
	}
	db := openTestDB(t)
	mustExec(t, db, `TRUNCATE payments, invoices, promotions, user_plans, radius_users, radcheck,
		radreply, radusergroup, radacct, radpostauth, nasreload, app_users RESTART IDENTITY CASCADE`)
	for _, seq := range []string{"invoice_number_seq", "receipt_number_seq"} {
		mustExec(t, db, `ALTER SEQUENCE `+seq+` RESTART WITH 1`)
	}
	mustExec(t, db, `UPDATE scheduled_tasks SET is_active = FALSE`) // tests run tasks themselves
	hash, err := auth.HashPassword(stackAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO app_users (username, password_hash, email, role, is_active)
		VALUES ('boss', $1, 'boss@example.com', 'super_admin', TRUE)`, hash)
	a := &stackAPI{t: t, base: strings.TrimRight(base, "/") + "/api/v1", http: &http.Client{Timeout: 30 * time.Second}}
	a.token = a.login("boss", stackAdminPassword)
	a.setSettings(map[string]string{"trial_days": "0", "billing_grace_days": "0", "renewal_invoice_days": "3", "invoice_due_days": "7"})
	return a, db
}

func (a *stackAPI) login(user, pass string) string {
	a.t.Helper()
	code, body, err := a.do("POST", "/auth/login", map[string]string{"username": user, "password": pass}, nil)
	if err != nil || code != 200 {
		a.t.Fatalf("login %s: %d %v %v", user, code, body, err)
	}
	return body["access_token"].(string)
}

// do sends one request. err is a transport error (no HTTP answer at all).
func (a *stackAPI) do(method, path string, body interface{}, hdr map[string]string) (int, map[string]interface{}, error) {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out := map[string]interface{}{}
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &out)
	if len(out) == 0 && len(raw) > 0 {
		out["raw"] = string(raw)
	}
	return resp.StatusCode, out, nil
}

func (a *stackAPI) must(method, path string, body interface{}, want int) map[string]interface{} {
	a.t.Helper()
	code, out, err := a.do(method, path, body, nil)
	if err != nil || code != want {
		a.t.Fatalf("%s %s: got %d %v (err %v), want %d", method, path, code, out, err, want)
	}
	return out
}

func (a *stackAPI) setSettings(kv map[string]string) {
	a.t.Helper()
	a.must("PUT", "/settings", kv, 200)
}

func (a *stackAPI) runTask(db *database.DB, taskType string) string {
	a.t.Helper()
	id := scanInt(a.t, db, `SELECT id FROM scheduled_tasks WHERE task_type=$1`, taskType)
	out := a.must("POST", fmt.Sprintf("/scheduler/%d/run", id), nil, 200)
	msg, _ := out["message"].(string)
	return msg
}

func stackCtl(t testing.TB, cmd string) {
	t.Helper()
	script := os.Getenv("TEST_STACK")
	if script == "" {
		t.Skip("TEST_STACK not set")
	}
	out, err := exec.Command(script, cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", script, cmd, err, out)
	}
}

// waitFor polls fn until it is true or the time runs out, returning how long it took.
func waitFor(limit time.Duration, fn func() bool) (time.Duration, bool) {
	t0 := time.Now()
	for time.Since(t0) < limit {
		if fn() {
			return time.Since(t0), true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return time.Since(t0), false
}

// customerWithInvoice creates a customer on a plan (no trial) and returns its id and its open
// invoice.
func (a *stackAPI) customerWithInvoice(db *database.DB, name string, planID int) (int, int) {
	a.t.Helper()
	out := a.must("POST", "/radius/users", map[string]interface{}{
		"username": name, "password": "pw-" + name, "plan_id": planID, "trial_days": 0}, 201)
	uid := int(out["id"].(float64))
	inv := scanInt(a.t, db, `SELECT id FROM invoices WHERE user_id=$1 AND status='pending'`, uid)
	return uid, inv
}

// Everything a member of staff does with one customer, through the API, with the real
// FreeRADIUS answering after each step.
func TestStackCustomerJourney(t *testing.T) {
	a, db := needStack(t)
	r := needRadius(t)
	plan := a.must("POST", "/plans", map[string]interface{}{"name": "Home 10", "price": 1500, "currency": "KES", "validity_days": 30, "max_devices": 1}, 201)
	planID := int(plan["id"].(float64))
	uid, inv := a.customerWithInvoice(db, "achieng", planID)
	if r.accepts(t, "achieng", "pw-achieng") {
		t.Error("unpaid customer (no trial) accepted")
	}

	key := map[string]string{"Idempotency-Key": "journey-0001"}
	code, out, err := a.do("POST", "/payments", map[string]interface{}{"invoice_id": inv, "amount": 1500, "payment_method": "mobile_money",
		"gateway": "mpesa", "gateway_ref": "QJK3H5L7M9"}, key)
	if err != nil || code != 201 {
		t.Fatalf("pay: %d %v %v", code, out, err)
	}
	payID := int(out["id"].(float64))
	if !r.accepts(t, "achieng", "pw-achieng") {
		t.Errorf("paid customer refused; radcheck: %s", radcheckOf(db, "achieng"))
	}
	// The browser retries (timeout): same key, same receipt, nothing new.
	code, out, _ = a.do("POST", "/payments", map[string]interface{}{"invoice_id": inv, "amount": 1500, "payment_method": "mobile_money",
		"gateway": "mpesa", "gateway_ref": "QJK3H5L7M9"}, key)
	if code != 200 || out["duplicate"] != true {
		t.Errorf("retry with the same key: %d %v, want 200 duplicate", code, out)
	}
	// The same M-Pesa code typed again by someone else (new key) is refused.
	code, _, _ = a.do("POST", "/payments", map[string]interface{}{"invoice_id": inv, "amount": 1500, "payment_method": "mobile_money",
		"gateway": "mpesa", "gateway_ref": "qjk3h5l7m9"}, map[string]string{"Idempotency-Key": "journey-0002"})
	if code < 400 {
		t.Errorf("the same M-Pesa reference (other case) was accepted again: %d", code)
	}
	a.must("GET", fmt.Sprintf("/payments/%d/receipt", payID), nil, 200)

	// An operator can look but not reverse money or delete customers.
	a.must("POST", "/admin/users", map[string]interface{}{"username": "clerk", "password": "Clerk-pass-123", "email": "clerk@example.com",
		"full_name": "Clerk", "role": "operator"}, 201)
	op := &stackAPI{t: t, base: a.base, http: a.http}
	op.token = op.login("clerk", "Clerk-pass-123")
	op.must("GET", "/payments", nil, 200)
	for _, c := range []struct{ method, path string }{
		{"POST", fmt.Sprintf("/payments/%d/reverse", payID)},
		{"DELETE", fmt.Sprintf("/radius/users/%d", uid)},
		{"PUT", "/settings"},
	} {
		if code, _, _ := op.do(c.method, c.path, map[string]string{}, nil); code != 403 {
			t.Errorf("operator %s %s: %d, want 403", c.method, c.path, code)
		}
	}

	// Expiry through the scheduler endpoint.
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE - 1 WHERE id=$1`, uid)
	if msg := a.runTask(db, "expire_accounts"); !strings.Contains(msg, "1 account") {
		t.Errorf("expire task said %q", msg)
	}
	if r.accepts(t, "achieng", "pw-achieng") {
		t.Error("expired customer accepted")
	}

	// Staff reverse the payment as admin; delete then works (nothing owed is left open).
	a.must("POST", fmt.Sprintf("/payments/%d/reverse", payID), map[string]string{"reason": "wrong customer"}, 200)
	a.must("DELETE", fmt.Sprintf("/radius/users/%d", uid), nil, 200)
	if r.accepts(t, "achieng", "pw-achieng") {
		t.Error("deleted customer accepted")
	}
	failIf(t, checkMoney(db))

	// A stolen or expired token is refused; logout ends the session.
	bad := &stackAPI{t: t, base: a.base, http: a.http, token: a.token + "x"}
	if code, _, _ := bad.do("GET", "/payments", nil, nil); code != 401 {
		t.Errorf("tampered token: %d, want 401", code)
	}
	a.must("POST", "/auth/logout", nil, 200)
	if code, _, _ := a.do("GET", "/payments", nil, nil); code != 401 {
		t.Errorf("token still works after logout: %d", code)
	}
}

// payOutcome is what a client learnt about one payment request.
type payOutcome struct {
	key      string
	invoice  int
	amount   float64
	attempts int
	settled  bool // got 201 or 200 (duplicate) in the end
	firstErr string
}

// payUntilSettled submits one payment and, like the Record Payment dialog, retries with the
// same idempotency key until the server gives a definite answer.
func (a *stackAPI) payUntilSettled(o *payOutcome, deadline time.Time) {
	body := map[string]interface{}{"invoice_id": o.invoice, "amount": o.amount, "payment_method": "cash"}
	for time.Now().Before(deadline) {
		o.attempts++
		code, out, err := a.do("POST", "/payments", body, map[string]string{"Idempotency-Key": o.key})
		if err == nil && (code == 201 || code == 200) {
			o.settled = true
			return
		}
		if o.firstErr == "" {
			if err != nil {
				o.firstErr = err.Error()
			} else {
				o.firstErr = fmt.Sprintf("%d %v", code, out)
			}
		}
		time.Sleep(time.Duration(100+rand.Intn(300)) * time.Millisecond)
	}
}

// checkPaymentsByKey: every request the client saw settled is in the books exactly once, and
// nothing is in the books that no client sent.
func checkPaymentsByKey(t *testing.T, db *database.DB, outcomes []*payOutcome) {
	t.Helper()
	sent := map[string]*payOutcome{}
	for _, o := range outcomes {
		sent[o.key] = o
		n := scanInt(t, db, `SELECT COUNT(*) FROM payments WHERE idempotency_key=$1`, o.key)
		switch {
		case o.settled && n != 1:
			t.Errorf("payment %s: client was told it was recorded, %d rows in the books", o.key, n)
		case !o.settled && n > 1:
			t.Errorf("payment %s: %d rows for one request", o.key, n)
		}
	}
	rows, _ := db.Query(`SELECT idempotency_key FROM payments`)
	for rows != nil && rows.Next() {
		var k *string
		rows.Scan(&k)
		if k == nil || sent[*k] == nil {
			t.Errorf("payment in the books that no client sent (key %v)", k)
		}
	}
	if rows != nil {
		rows.Close()
	}
}

// A.7 PostgreSQL crashes (no clean shutdown) while staff record payments. Afterwards: no
// payment without its days, no days without a payment, every acknowledged payment kept, the
// backend and FreeRADIUS working again without being restarted.
func TestStackPostgresCrashDuringPayments(t *testing.T) {
	a, db := needStack(t)
	r := needRadius(t)
	plan := a.must("POST", "/plans", map[string]interface{}{"name": "Weekly", "price": 1000, "currency": "KES", "validity_days": 7}, 201)
	planID := int(plan["id"].(float64))

	const customers, instalments = 24, 10
	type cust struct {
		name     string
		uid, inv int
	}
	var cs []cust
	for i := 0; i < customers; i++ {
		name := fmt.Sprintf("crash%02d", i)
		uid, inv := a.customerWithInvoice(db, name, planID)
		cs = append(cs, cust{name, uid, inv})
	}
	// A customer who is online before the crash and must be online after it.
	_, sentinelInv := a.customerWithInvoice(db, "sentinel", planID)
	sentinelPay := &payOutcome{key: "sentinel-0", invoice: sentinelInv, amount: 1000}
	a.payUntilSettled(sentinelPay, time.Now().Add(10*time.Second))
	if !r.accepts(t, "sentinel", "pw-sentinel") {
		t.Fatal("sentinel customer refused before the crash")
	}

	outcomes := []*payOutcome{sentinelPay}
	var mu sync.Mutex
	var started int32
	deadline := time.Now().Add(90 * time.Second)
	crashDone := make(chan struct{})
	go func() {
		for atomic.LoadInt32(&started) < customers*3 {
			time.Sleep(20 * time.Millisecond)
		}
		stackCtl(t, "db-crash")
		close(crashDone)
	}()

	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func(c cust) {
			defer wg.Done()
			for k := 0; k < instalments; k++ {
				o := &payOutcome{key: fmt.Sprintf("%s-%02d", c.name, k), invoice: c.inv, amount: 100}
				mu.Lock()
				outcomes = append(outcomes, o)
				mu.Unlock()
				atomic.AddInt32(&started, 1)
				a.payUntilSettled(o, deadline)
			}
		}(c)
	}
	<-crashDone
	t0 := time.Now()
	radiusBack, ok := waitFor(120*time.Second, func() bool { return r.login("sentinel", "pw-sentinel").Reply == "Access-Accept" })
	t.Logf("FreeRADIUS accepted the paid customer again %s after PostgreSQL came back (ok=%v)", radiusBack.Round(100*time.Millisecond), ok)
	if !ok {
		t.Errorf("FreeRADIUS still refusing a paid customer 2 minutes after PostgreSQL restarted")
	}
	wg.Wait()
	t.Logf("payment loop finished %s after the crash", time.Since(t0).Round(100*time.Millisecond))

	retried, unsettled := 0, 0
	for _, o := range outcomes {
		if o.attempts > 1 {
			retried++
		}
		if !o.settled {
			unsettled++
		}
	}
	t.Logf("%d payment requests, %d needed a retry, %d never settled", len(outcomes), retried, unsettled)
	if unsettled > 0 {
		t.Errorf("%d payments never went through after the database came back", unsettled)
	}
	if retried == 0 {
		t.Log("note: the crash hit no request in flight; the test proved little this run")
	}
	checkPaymentsByKey(t, db, outcomes)
	failIf(t, checkMoney(db))
	for _, c := range cs {
		paid := scanFloat(t, db, `SELECT COALESCE(SUM(amount),0) FROM payments WHERE invoice_id=$1 AND status='confirmed'`, c.inv)
		status := scanString(t, db, `SELECT status FROM invoices WHERE id=$1`, c.inv)
		days := scanInt(t, db, `SELECT COALESCE(account_expiry - CURRENT_DATE, -999) FROM radius_users WHERE id=$1`, c.uid)
		if paid != 1000 || status != "paid" || days != 7 {
			t.Errorf("%s: paid %.0f, invoice %s, paid-until today%+d; want 1000, paid, +7", c.name, paid, status, days)
		}
	}
	pw := map[string]string{"sentinel": "pw-sentinel"}
	for _, c := range cs {
		pw[c.name] = "pw-" + c.name
	}
	failIf(t, checkRealRadius(r, db, pw))
}

// A.10 The backend is killed (kill -9) in the middle of payments, again and again. The client
// retries with the same key after each restart; every payment must end up recorded once.
func TestStackBackendKilledMidPayment(t *testing.T) {
	a, db := needStack(t)
	plan := a.must("POST", "/plans", map[string]interface{}{"name": "Daily", "price": 50, "currency": "KES", "validity_days": 1}, 201)
	planID := int(plan["id"].(float64))
	_, inv := a.customerWithInvoice(db, "killme", planID)
	// One big invoice paid in many small amounts so every request lands on the same row lock.
	mustExec(t, db, `UPDATE invoices SET amount = 100000 WHERE id=$1`, inv)

	rounds := simEnv("STACK_KILL_ROUNDS", 15)
	var outcomes []*payOutcome
	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		batch := make([]*payOutcome, 8)
		for i := range batch {
			batch[i] = &payOutcome{key: fmt.Sprintf("kill-%02d-%d", round, i), invoice: inv, amount: 10}
			outcomes = append(outcomes, batch[i])
		}
		for _, o := range batch {
			wg.Add(1)
			go func(o *payOutcome) {
				defer wg.Done()
				a.payUntilSettled(o, time.Now().Add(60*time.Second))
			}(o)
		}
		time.Sleep(time.Duration(rand.Intn(40)) * time.Millisecond)
		stackCtl(t, "kill-backend")
		stackCtl(t, "start-backend")
		wg.Wait()
		// Sessions live in the database, so the token still works after a restart.
	}
	retried := 0
	for _, o := range outcomes {
		if o.attempts > 1 {
			retried++
		}
	}
	t.Logf("%d rounds, %d payments, %d needed a retry after a kill", rounds, len(outcomes), retried)
	checkPaymentsByKey(t, db, outcomes)
	failIf(t, checkMoney(db))
	want := 0.0
	for _, o := range outcomes {
		if o.settled {
			want += o.amount
		}
	}
	if got := scanFloat(t, db, `SELECT COALESCE(SUM(amount),0) FROM payments WHERE invoice_id=$1 AND status='confirmed'`, inv); got != want {
		t.Errorf("invoice holds %.2f, the clients were told %.2f", got, want)
	}
}

// A.8 FreeRADIUS is down while a payment and an expiry happen. When it starts again the paying
// customer gets online and the expired one does not: FreeRADIUS keeps no state of its own.
func TestStackRadiusDownDuringBilling(t *testing.T) {
	a, db := needStack(t)
	r := needRadius(t)
	plan := a.must("POST", "/plans", map[string]interface{}{"name": "Home", "price": 1500, "currency": "KES", "validity_days": 30}, 201)
	planID := int(plan["id"].(float64))
	_, payInv := a.customerWithInvoice(db, "payer", planID)
	lapsedID, lapsedInv := a.customerWithInvoice(db, "lapser", planID)
	a.must("POST", "/payments", map[string]interface{}{"invoice_id": lapsedInv, "amount": 1500}, 201)

	stackCtl(t, "radius-down")
	defer stackCtl(t, "radius-up")
	a.must("POST", "/payments", map[string]interface{}{"invoice_id": payInv, "amount": 1500}, 201)
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE - 1 WHERE id=$1`, lapsedID)
	a.runTask(db, "expire_accounts")
	stackCtl(t, "radius-up")

	if !r.accepts(t, "payer", "pw-payer") {
		t.Errorf("customer who paid while FreeRADIUS was down is refused; radcheck: %s", radcheckOf(db, "payer"))
	}
	if r.accepts(t, "lapser", "pw-lapser") {
		t.Error("customer who expired while FreeRADIUS was down is accepted")
	}
}

// PostgreSQL down: what routers get from FreeRADIUS, what staff get from the API, and how
// quickly both recover once it is back, with nothing restarted by hand.
func TestStackDatabaseOutage(t *testing.T) {
	a, db := needStack(t)
	r := needRadius(t)
	plan := a.must("POST", "/plans", map[string]interface{}{"name": "Home", "price": 1500, "currency": "KES", "validity_days": 30}, 201)
	_, inv := a.customerWithInvoice(db, "steady", int(plan["id"].(float64)))
	a.must("POST", "/payments", map[string]interface{}{"invoice_id": inv, "amount": 1500}, 201)
	if !r.accepts(t, "steady", "pw-steady") {
		t.Fatal("paid customer refused before the outage")
	}

	stackCtl(t, "db-down")
	defer stackCtl(t, "db-up")
	res := r.login("steady", "pw-steady")
	t.Logf("database down: FreeRADIUS answers a paid customer with %q (error %q)", res.Reply, res.Error)
	if res.Reply == "Access-Accept" {
		t.Log("note: FreeRADIUS accepted with the database down (cached?)")
	}
	code, body, err := a.do("GET", "/payments", nil, nil)
	t.Logf("database down: API answers %d %v (transport error %v)", code, body, err)
	if err != nil {
		t.Errorf("the API stopped answering HTTP while the database was down: %v", err)
	} else if code < 500 {
		t.Errorf("the API answered %d with the database down; staff must see an error", code)
	}
	code, _, _ = a.do("POST", "/payments", map[string]interface{}{"invoice_id": inv, "amount": 1}, map[string]string{"Idempotency-Key": "outage-1"})
	if code == 201 || code == 200 {
		t.Errorf("a payment was 'recorded' with the database down (%d)", code)
	}
	t.Logf("database down: recording a payment answers %d", code)

	time.Sleep(3 * time.Second)
	stackCtl(t, "db-up")
	apiBack, ok := waitFor(60*time.Second, func() bool { c, _, e := a.do("GET", "/payments", nil, nil); return e == nil && c == 200 })
	t.Logf("API working again %s after PostgreSQL came back (ok=%v)", apiBack.Round(100*time.Millisecond), ok)
	if !ok {
		t.Error("API did not recover within 60s after the database came back")
	}
	radBack, ok := waitFor(120*time.Second, func() bool { return r.login("steady", "pw-steady").Reply == "Access-Accept" })
	t.Logf("FreeRADIUS accepting again %s after PostgreSQL came back (ok=%v)", radBack.Round(100*time.Millisecond), ok)
	if !ok {
		t.Error("FreeRADIUS did not recover within 120s after the database came back")
	} else if radBack > 10*time.Second {
		t.Errorf("FreeRADIUS took %s to recover after the database came back: paying customers are refused for that long", radBack.Round(time.Second))
	}
}
