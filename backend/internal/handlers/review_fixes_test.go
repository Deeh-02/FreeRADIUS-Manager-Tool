package handlers

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeradius-manager/backend/internal/auth"
	"github.com/freeradius-manager/backend/internal/database"
	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/gin-gonic/gin"
)

// Fixes from the external review of 2026-10-25 (ARCHITECTURE.md §10, R-items). Each test
// fails on the code before its fix.

// R1: a database that goes away must not crash the API from a background job. A failed Exec
// returns a nil Result and RowsAffected on it panicked, outside any request recovery.
func TestBackgroundJobsSurviveDatabaseOutage(t *testing.T) {
	newMpesaRig(t) // M-Pesa configured, so reconcile gets as far as the database
	raw, err := sql.Open("postgres", "host=127.0.0.1 port=1 dbname=gone_test sslmode=disable connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()
	down := &database.DB{DB: raw}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("reconcileMpesa panicked with the database down: %v", r)
			}
		}()
		if msg, _ := reconcileMpesa(down, testLogger()); !strings.Contains(msg, "database") {
			t.Errorf("reconcile with the database down said %q", msg)
		}
	}()

	// Any task that panics is reported as failed instead of stopping the process.
	taskRunHook = func(string) { panic("boom") }
	defer func() { taskRunHook = nil }()
	if msg, _ := runTaskSafely(down, testLogger(), "expire_accounts"); !strings.Contains(msg, "panic") {
		t.Errorf("a panicking task returned %q", msg)
	}
}

// R2: deleting an invoice while a payment for it is being recorded. The check for payments
// ran before the payment committed, the delete after, and the payment lost its invoice.
func TestDeleteInvoiceDuringPayment(t *testing.T) {
	db := resetTestDB(t)
	h := newTestHandler(t)
	uid := seedUser(t, db, "racer", intPtr(5))
	inv := seedInvoice(t, db, uid, "racer", nil, 50)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := recordPaymentTx(tx, paymentInput{InvoiceID: &inv, Amount: 50, PaymentMethod: "cash"}); err != nil {
		t.Fatalf("payment: %v", err)
	}
	done := make(chan int)
	go func() {
		code, _ := callHandler(h.DeleteInvoice, gin.Params{{Key: "id", Value: strconv.Itoa(inv)}}, nil, nil)
		done <- code
	}()
	time.Sleep(300 * time.Millisecond) // the delete is now waiting on the invoice
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 409 {
		t.Errorf("delete during a payment: %d, want 409", code)
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM payments WHERE invoice_id = $1`, inv); n != 1 {
		t.Errorf("the payment is linked to its invoice %d time(s), want 1", n)
	}
}

// R3: reversing the payment that paid for an online customer's time ends their session, not
// only their next login.
func TestReversalDisconnectsCustomer(t *testing.T) {
	db := resetTestDB(t)
	mustExec(t, db, `TRUNCATE radacct`)
	h := newTestHandler(t)
	nas := startFakeNAS(t, db, nasSecret, "ack")
	plan := seedPlan(t, db, "Monthly", 100, 30)
	uid := seedUser(t, db, "bounced", intPtr(-5))
	inv := seedInvoice(t, db, uid, "bounced", plan, 100)
	code, resp := callHandler(h.CreatePayment, nil, nil, map[string]interface{}{"invoice_id": inv, "amount": 100, "payment_method": "cash"})
	if code != 201 {
		t.Fatalf("payment: %d %v", code, resp)
	}
	mustExec(t, db, `INSERT INTO radacct (acctsessionid, acctuniqueid, username, nasipaddress, acctstarttime, acctupdatetime)
		VALUES ('s-bounced', 'u-bounced', 'bounced', '127.0.0.1', NOW() - INTERVAL '1 hour', NOW())`)
	pid := scanString(t, db, `SELECT id::text FROM payments WHERE invoice_id = $1`, inv)

	if code, resp := callHandler(h.ReversePayment, gin.Params{{Key: "id", Value: pid}}, nil, map[string]interface{}{"reason": "cheque bounced"}); code != 200 {
		t.Fatalf("reverse: %d %v", code, resp)
	}
	if got := nas.received(); len(got) != 1 || got[0] != "bounced" {
		t.Errorf("router received disconnects for %v, want [bounced]", got)
	}
	if n := openSessions(t, "bounced"); n != 0 {
		t.Errorf("%d session(s) still open after the reversal took the paid time back", n)
	}
}

// R4: an idempotency key reused for a payment by another customer is a different payment, not
// a retry: answering with the first one would tell staff the second was recorded.
func TestPaymentKeyReusedForAnotherCustomer(t *testing.T) {
	db := resetTestDB(t)
	seedUser(t, db, "first", intPtr(5))
	seedUser(t, db, "second", intPtr(5))
	pay := func(user, method string) error {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		_, err = recordPaymentTx(tx, paymentInput{Username: user, Amount: 20, PaymentMethod: method, IdempotencyKey: "shared-key-1"})
		if err == nil {
			err = tx.Commit()
		}
		return err
	}
	if err := pay("first", "cash"); err != nil {
		t.Fatal(err)
	}
	if err := pay("second", "cash"); err != errKeyMismatch {
		t.Errorf("same key for another customer: %v, want errKeyMismatch", err)
	}
	if err := pay("first", "bank"); err != errKeyMismatch {
		t.Errorf("same key with another payment method: %v, want errKeyMismatch", err)
	}
	if err := pay("first", "cash"); err != nil {
		t.Errorf("a true retry: %v", err)
	}
}

func seedProfile(t *testing.T, db *database.DB, name string, down int) int {
	t.Helper()
	return scanInt(t, db, `INSERT INTO bandwidth_profiles (name, upload_kbps, download_kbps) VALUES ($1, 512, $2) RETURNING id`, name, down)
}

func rateLimitOf(t *testing.T, db *database.DB, user string) string {
	t.Helper()
	return scanString(t, db, `SELECT COALESCE((SELECT value FROM radreply WHERE username = $1 AND attribute = 'Mikrotik-Rate-Limit'), '')`, user)
}

// R5: speed limits. A wrong profile ID stripped the working limits; deleting a profile, or a
// plan dropping its profile, left the old limits on the router.
func TestSpeedLimitsFollowProfilesAndPlans(t *testing.T) {
	db := resetTestDB(t)
	mustExec(t, db, `DELETE FROM bandwidth_profiles`)
	h := newTestHandler(t)
	fast := seedProfile(t, db, "Fast", 10240)
	manual := seedProfile(t, db, "Manual", 4096)
	apply := func(uid int, profile interface{}) int {
		code, _ := callHandler(h.ApplyBandwidthProfile, gin.Params{{Key: "id", Value: strconv.Itoa(uid)}}, nil,
			map[string]interface{}{"profile_id": profile})
		return code
	}

	// A wrong profile ID changes nothing.
	alice := seedUser(t, db, "alice", intPtr(10))
	if code := apply(alice, fast); code != 200 {
		t.Fatalf("apply: %d", code)
	}
	before := rateLimitOf(t, db, "alice")
	if code := apply(alice, 999999); code != 404 {
		t.Errorf("unknown profile: %d, want 404", code)
	}
	if got := rateLimitOf(t, db, "alice"); got != before || got == "" {
		t.Errorf("after a 404 the rate limit is %q, want %q", got, before)
	}

	// Deleting a profile removes its limits from the customers who had it.
	if code, _ := callHandler(h.DeleteBandwidthProfile, gin.Params{{Key: "id", Value: strconv.Itoa(fast)}}, nil, nil); code != 200 {
		t.Fatalf("delete profile: %d", code)
	}
	if got := rateLimitOf(t, db, "alice"); got != "" {
		t.Errorf("profile deleted but alice is still limited to %q", got)
	}

	// A plan that stops setting a speed takes away the speed it gave, but not one given by hand.
	fast = seedProfile(t, db, "Fast2", 10240)
	plan := seedPlan(t, db, "Speedy", 100, 30)
	mustExec(t, db, `UPDATE user_plans SET bandwidth_profile_id = $1 WHERE id = $2`, fast, plan.ID)
	plan, _ = loadPlan(db, plan.ID)
	bob := seedUser(t, db, "bob", intPtr(10))
	carol := seedUser(t, db, "carol", intPtr(10))
	for _, u := range []struct {
		id   int
		name string
	}{{bob, "bob"}, {carol, "carol"}} {
		if err := applyPlanLimits(db, u.id, u.name, plan); err != nil {
			t.Fatal(err)
		}
	}
	if code := apply(carol, manual); code != 200 { // staff give carol her own speed
		t.Fatalf("manual apply: %d", code)
	}
	mustExec(t, db, `UPDATE user_plans SET bandwidth_profile_id = NULL WHERE id = $1`, plan.ID)
	if err := h.reapplyPlan(plan.ID, sql.NullInt64{Int64: int64(fast), Valid: true}); err != nil {
		t.Fatal(err)
	}
	if got := rateLimitOf(t, db, "bob"); got != "" {
		t.Errorf("plan no longer sets a speed but bob is still limited to %q", got)
	}
	if got := rateLimitOf(t, db, "carol"); got == "" {
		t.Error("carol's hand-given speed was removed by the plan edit")
	}

	// Moving to a plan without a speed takes the old plan's speed away.
	mustExec(t, db, `UPDATE user_plans SET bandwidth_profile_id = $1 WHERE id = $2`, fast, plan.ID)
	plan, _ = loadPlan(db, plan.ID)
	if err := applyPlanLimits(db, bob, "bob", plan); err != nil {
		t.Fatal(err)
	}
	basic := seedPlan(t, db, "Basic", 50, 30)
	if err := applyPlanLimits(db, bob, "bob", basic); err != nil {
		t.Fatal(err)
	}
	if got := rateLimitOf(t, db, "bob"); got != "" {
		t.Errorf("bob moved to a plan without a speed but is still limited to %q", got)
	}
}

// R6: re-applying an edited plan must not put back a customer who moved to another plan while
// it ran.
func TestPlanReapplyKeepsCustomerWhoMoved(t *testing.T) {
	db := resetTestDB(t)
	h := newTestHandler(t)
	old := seedPlan(t, db, "Old", 100, 30)
	next := seedPlan(t, db, "Next", 200, 30)
	uid := seedUser(t, db, "mover", intPtr(10))
	if err := applyPlanLimits(db, uid, "mover", old); err != nil {
		t.Fatal(err)
	}

	// The customer is being moved (row locked) when the re-apply reaches them.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := lockUser(tx, uid); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		h.reapplyPlan(old.ID, sql.NullInt64{})
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)
	if err := applyPlanLimits(tx, uid, "mover", next); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	<-done
	if got := scanInt(t, db, `SELECT plan_id FROM radius_users WHERE id = $1`, uid); got != next.ID {
		t.Errorf("customer is on plan %d after the re-apply, want the new plan %d", got, next.ID)
	}
}

// R7: a template with an empty operator (applied as :=) or a lower-case name could overwrite
// the password, paid-until date or device limit that the billing code manages.
func TestTemplateCannotSetManagedAttributes(t *testing.T) {
	for _, a := range []AttributeEntry{
		{Table: "check", Attribute: "Expiration", Op: "", Value: "01 Jan 2099"},
		{Table: "check", Attribute: "expiration", Op: ":=", Value: "01 Jan 2099"},
		{Table: "check", Attribute: "Cleartext-Password", Value: "x"},
		{Table: "check", Attribute: "SIMULTANEOUS-USE", Value: "50"},
	} {
		if err := validateTemplateAttrs([]AttributeEntry{a}); err == nil {
			t.Errorf("template may set %s (op %q)", a.Attribute, a.Op)
		}
	}
	if err := validateTemplateAttrs([]AttributeEntry{{Table: "reply", Attribute: "Session-Timeout", Value: "3600"}}); err != nil {
		t.Errorf("an ordinary reply attribute is refused: %v", err)
	}
}

// R8: two refreshes with the same token at once both got new tokens (check and revoke were
// separate statements).
func TestRefreshTokenUsedOnce(t *testing.T) {
	db := resetTestDB(t)
	mustExec(t, db, `DELETE FROM cred_stuffing_blocks`)
	h := newTestHandler(t)
	hash, _ := auth.HashPassword("staff-password-1")
	mustExec(t, db, `UPDATE app_users SET password_hash = $1, failed_attempts = 0, locked_until = NULL WHERE username = 'staff'`, hash)
	code, resp := callHandler(h.Login, nil, nil, map[string]interface{}{"username": "staff", "password": "staff-password-1"})
	if code != 200 {
		t.Fatalf("login: %d %v", code, resp)
	}
	token, _ := resp["refresh_token"].(string)
	// Rounds of 40 at once, each round with the token the last round handed out.
	for round := 0; round < 5 && !t.Failed(); round++ {
		var ok int32
		var next atomic.Value
		parallel(40, func(int) {
			code, body := callHandler(h.RefreshToken, nil, nil, map[string]interface{}{"refresh_token": token})
			if code == 200 {
				atomic.AddInt32(&ok, 1)
				next.Store(body["refresh_token"])
			} else if code != 401 {
				t.Errorf("refresh: %d %v", code, body)
			}
		})
		if ok != 1 {
			t.Errorf("round %d: %d of 40 refreshes with one token succeeded, want 1", round, ok)
		}
		if n := scanInt(t, db, `SELECT COUNT(*) FROM refresh_tokens WHERE revoked = FALSE`); n != 1 {
			t.Errorf("round %d: %d live refresh tokens, want 1", round, n)
		}
		token, _ = next.Load().(string)
	}
}

// R9: failed sign-ins arriving together overwrote each other's count (read, add one, write
// back), so a burst of guesses did not lock the account.
func TestSimultaneousFailedLoginsAllCount(t *testing.T) {
	db := resetTestDB(t)
	mustExec(t, db, `DELETE FROM cred_stuffing_blocks`)
	h := newTestHandler(t)
	hash, _ := auth.HashPassword("staff-password-1")
	mustExec(t, db, `UPDATE app_users SET password_hash = $1, failed_attempts = 0, locked_until = NULL WHERE username = 'staff'`, hash)
	if code, resp := callHandler(h.CreateRadiusUser, nil, nil, map[string]interface{}{"username": "guessme", "password": "right-password-1"}); code != 201 {
		t.Fatalf("create: %d %v", code, resp)
	}
	burst := func(fn gin.HandlerFunc, user string) int32 {
		var refused int32
		parallel(12, func(int) {
			if code, _ := callHandler(fn, nil, nil, map[string]interface{}{"username": user, "password": "wrong"}); code == 401 {
				atomic.AddInt32(&refused, 1)
			}
		})
		return refused
	}
	if n := burst(h.Login, "staff"); int(n) != scanInt(t, db, `SELECT failed_attempts FROM app_users WHERE username = 'staff'`) {
		t.Errorf("staff: %d wrong passwords refused but failed_attempts = %d", n,
			scanInt(t, db, `SELECT failed_attempts FROM app_users WHERE username = 'staff'`))
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM app_users WHERE username = 'staff' AND locked_until > NOW()`); n != 1 {
		t.Error("staff account not locked after a burst of 12 wrong passwords")
	}
	if n := burst(h.PortalLogin, "guessme"); int(n) != scanInt(t, db, `SELECT portal_failed_attempts FROM radius_users WHERE username = 'guessme'`) {
		t.Errorf("portal: %d wrong passwords refused but portal_failed_attempts = %d", n,
			scanInt(t, db, `SELECT portal_failed_attempts FROM radius_users WHERE username = 'guessme'`))
	}
}

// R10: staff session idle time was measured with Go's clock against a TIMESTAMP written with
// the database's: with the two in different zones a session idle for hours stayed signed in
// (or, the other way round, every session timed out at once). Go is put in UTC here; the
// scratch database is in Africa/Nairobi.
func TestSessionIdleTimeoutUsesDatabaseClock(t *testing.T) {
	db := resetTestDB(t)
	saved := time.Local
	time.Local = time.UTC
	defer func() { time.Local = saved }()
	uid := scanInt(t, db, `SELECT id FROM app_users WHERE username = 'staff'`)
	mustExec(t, db, `INSERT INTO refresh_tokens (user_id, token_hash, session_id, expires_at, last_activity) VALUES
		($1, 'h-busy', 'busy', NOW() + INTERVAL '1 day', NOW() - INTERVAL '10 minutes'),
		($1, 'h-idle', 'idle', NOW() + INTERVAL '1 day', NOW() - INTERVAL '2 hours')`, uid)
	for sid, want := range map[string]bool{"busy": true, "idle": false} {
		st, err := auth.ValidateSession(db.DB, uid, sid)
		if err != nil {
			t.Fatal(err)
		}
		if st.Active != want {
			t.Errorf("session %s (1 h timeout): active = %v, want %v", sid, st.Active, want)
		}
	}
}

// R11: the guard that stops tests wiping a real database accepted any connection string with
// "_test" somewhere in it, such as a user or password.
func TestTestDatabaseGuardReadsTheName(t *testing.T) {
	for conn, want := range map[string]string{
		"host=127.0.0.1 port=54329 user=postgres dbname=radius_test sslmode=disable": "radius_test",
		"host=db user=app_test password=x dbname=radius sslmode=require":             "radius",
		"postgres://ci_test:pw@db.example.com:5432/radius?sslmode=require":           "radius",
		"postgresql://u@localhost/billing_test":                                      "billing_test",
	} {
		if got := testDBName(conn); got != want {
			t.Errorf("testDBName(%q) = %q, want %q", conn, got, want)
		}
	}
}

// R12: a customer password over bcrypt's 72 bytes (40 accented letters are 80 bytes) passed
// the 128-character check and then failed with a server error.
func TestOverlongPasswordIsRefusedPolitely(t *testing.T) {
	resetTestDB(t)
	h := newTestHandler(t)
	code, resp := callHandler(h.CreateRadiusUser, nil, nil, map[string]interface{}{
		"username": "longpw", "password": strings.Repeat("é", 40)})
	if code != 400 {
		t.Errorf("80-byte password: %d %v, want 400", code, resp)
	}
}

// R13: a portal without a fixed login URL took the form's destination from the link, so a
// crafted link showed the genuine page and sent the customer's password to any server.
func TestPortalFormOnlyPostsToARouter(t *testing.T) {
	db := resetTestDB(t)
	h := newTestHandler(t)
	mustExec(t, db, `DELETE FROM nas WHERE nasname = '203.0.113.9'`)
	mustExec(t, db, `INSERT INTO nas (nasname, shortname, secret) VALUES ('203.0.113.9', 'public-router', 'x')`)
	defer db.Exec(`DELETE FROM nas WHERE nasname = '203.0.113.9'`)
	id := scanInt(t, db, `INSERT INTO captive_portals (name) VALUES ('Lobby') RETURNING id`)
	defer db.Exec(`DELETE FROM captive_portals WHERE id = $1`, id)
	page := func(loginLink string) string {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/?link-login-only="+url.QueryEscape(loginLink), nil)
		c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
		h.ServeCaptivePortal(c)
		return w.Body.String()
	}
	for link, ok := range map[string]bool{
		"http://10.5.50.1/login":               true,
		"http://192.168.88.1/login":            true,
		"http://hotspot.lan/login":             true,
		"http://wifi/login":                    true,
		"http://203.0.113.9/login":             true, // a registered router
		"https://evil.example.com/harvest":     false,
		"http://198.51.100.20/login":           false,
		"https://10.5.50.1.evil.example/login": false,
	} {
		posts := strings.Contains(page(link), `action="`+link+`"`)
		if posts != ok {
			t.Errorf("link-login-only=%s: form posts there = %v, want %v", link, posts, ok)
		}
	}
}

// R14: endpoints answered "revoked", "deleted" or "updated" when nothing matched or the
// write failed.
func TestWritesThatChangeNothingSaySo(t *testing.T) {
	resetTestDB(t)
	h := newTestHandler(t)
	missing := gin.Params{{Key: "id", Value: "987654"}}
	for name, fn := range map[string]gin.HandlerFunc{
		"revoke API key": h.RevokeAPIKey, "delete customer": h.DeleteCustomer, "delete zone": h.DeleteZone,
		"delete organization": h.DeleteOrganization, "unblock IP": h.UnblockIP,
	} {
		if code, _ := callHandler(fn, missing, nil, nil); code != 404 {
			t.Errorf("%s with an unknown ID: %d, want 404", name, code)
		}
	}
	if code, _ := callHandler(h.AssignNASToZone, nil, nil, map[string]interface{}{"nas_id": 987654}); code != 404 {
		t.Errorf("zone for an unknown NAS: %d, want 404", code)
	}
}

// R15: limits under concurrency. An organisation with room for one took several at once, and
// one customer could end up holding two addresses from simultaneous assignments.
func TestOrgLimitAndIPAssignmentUnderConcurrency(t *testing.T) {
	db := resetTestDB(t)
	h := newTestHandler(t)
	mustExec(t, db, `DELETE FROM ip_pool_assignments`)
	mustExec(t, db, `DELETE FROM ip_pools WHERE name = 'race-pool'`)
	mustExec(t, db, `UPDATE radius_users SET org_id = NULL`)
	mustExec(t, db, `DELETE FROM organizations WHERE slug = 'one-seat'`)
	org := scanInt(t, db, `INSERT INTO organizations (name, slug, user_limit) VALUES ('One seat', 'one-seat', 1) RETURNING id`)
	defer db.Exec(`DELETE FROM organizations WHERE id = $1`, org)
	for i := 0; i < 10; i++ {
		seedUser(t, db, "seat"+strconv.Itoa(i), intPtr(5))
	}
	var joined int32
	parallel(10, func(i int) {
		if code, _ := callHandler(h.AssignUserToOrg, nil, nil, map[string]interface{}{"username": "seat" + strconv.Itoa(i), "org_id": org}); code == 200 {
			atomic.AddInt32(&joined, 1)
		}
	})
	if n := scanInt(t, db, `SELECT COUNT(*) FROM radius_users WHERE org_id = $1`, org); joined != 1 || n != 1 {
		t.Errorf("organisation limited to 1 user: %d assignments succeeded, %d users in it", joined, n)
	}

	pool := scanInt(t, db, `INSERT INTO ip_pools (name, network) VALUES ('race-pool', '10.9.0.0/24') RETURNING id`)
	for i := 1; i <= 20; i++ {
		mustExec(t, db, `INSERT INTO ip_pool_assignments (pool_id, ip_address) VALUES ($1, $2)`, pool, "10.9.0."+strconv.Itoa(i))
	}
	parallel(10, func(int) {
		callHandler(h.AssignIP, nil, nil, map[string]interface{}{"username": "seat0", "pool_id": pool})
	})
	if n := scanInt(t, db, `SELECT COUNT(*) FROM ip_pool_assignments WHERE username = 'seat0'`); n != 1 {
		t.Errorf("seat0 holds %d addresses after simultaneous assignments, want 1", n)
	}
}

// R16: the live-stats stream was authorised once, when it opened; signing out or disabling
// the account did not stop it.
func TestLiveStreamStopsAfterSignOut(t *testing.T) {
	db := resetTestDB(t)
	mustExec(t, db, `DELETE FROM cred_stuffing_blocks`)
	h := newTestHandler(t)
	hash, _ := auth.HashPassword("staff-password-1")
	mustExec(t, db, `UPDATE app_users SET password_hash = $1 WHERE username = 'staff'`, hash)
	signIn := func() string {
		code, resp := callHandler(h.Login, nil, nil, map[string]interface{}{"username": "staff", "password": "staff-password-1"})
		if code != 200 {
			t.Fatalf("login: %d %v", code, resp)
		}
		return "Bearer " + resp["access_token"].(string)
	}
	header := signIn()
	if !middleware.StillAuthorized(db, header) {
		t.Fatal("a fresh session is not authorised")
	}
	mustExec(t, db, `UPDATE refresh_tokens SET revoked = TRUE`)
	if middleware.StillAuthorized(db, header) {
		t.Error("still authorised after signing out")
	}
	header = signIn()
	mustExec(t, db, `UPDATE app_users SET is_active = FALSE WHERE username = 'staff'`)
	defer db.Exec(`UPDATE app_users SET is_active = TRUE WHERE username = 'staff'`)
	if middleware.StillAuthorized(db, header) {
		t.Error("still authorised after the account was disabled")
	}
	if middleware.StillAuthorized(db, "Bearer not-a-token") {
		t.Error("a forged token is authorised")
	}
}

// R17: changing the system-wide grace days only reached the scheduler; the Expiration rows the
// router enforces kept the old grace, so raising it still cut customers off on the old day.
func TestGraceDaysChangeReachesTheRouter(t *testing.T) {
	db := resetTestDB(t)
	setTestSettings(t, db, map[string]string{"billing_grace_days": "0"})
	seedUser(t, db, "graceful", intPtr(2))
	own := seedUser(t, db, "owngrace", intPtr(2))
	mustExec(t, db, `UPDATE radius_users SET grace_days = 1 WHERE id = $1`, own)
	if err := syncAccess(db, "owngrace"); err != nil {
		t.Fatal(err)
	}
	expiration := func(u string) string {
		return scanString(t, db, `SELECT value FROM radcheck WHERE username = $1 AND attribute = 'Expiration'`, u)
	}
	paidUntil := time.Now().AddDate(0, 0, 2)
	ownBefore := expiration("owngrace")

	h := newTestHandler(t)
	if code, resp := callHandler(h.UpdateSettings, nil, nil, map[string]string{"billing_grace_days": "5"}); code != 200 {
		t.Fatalf("settings: %d %v", code, resp)
	}
	want := radiusExpiration(paidUntil, 5)
	for i := 0; i < 50 && expiration("graceful") != want; i++ {
		time.Sleep(100 * time.Millisecond) // rewritten in the background
	}
	if got := expiration("graceful"); got != want {
		t.Errorf("after raising grace to 5 days the router cuts off at %q, want %q", got, want)
	}
	if got := expiration("owngrace"); got != ownBefore {
		t.Errorf("a customer with their own grace changed from %q to %q", ownBefore, got)
	}
}
