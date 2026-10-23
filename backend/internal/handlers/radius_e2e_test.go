package handlers

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeradius-manager/backend/internal/database"
	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

// ─────────────────────────────────────────────────────────────────────────────
// End to end: the real FreeRADIUS, reading the same scratch database
// ─────────────────────────────────────────────────────────────────────────────
//
// invariants_test.go judges radcheck by imitating FreeRADIUS (radiusDecision). These tests
// ask a real FreeRADIUS, started by scripts/test-radius.sh with this repository's config,
// over UDP, as a router would. They are skipped unless TEST_RADIUS_ADDR is set:
//
//	eval "$(scripts/test-db.sh start)"; eval "$(scripts/test-radius.sh start)"
//	go test ./internal/handlers -run RealRadius -count=1

type liveRadius struct {
	host   string
	port   int
	secret string
	// clockSkew, when set, makes accounting carry an Event-Timestamp this far from the real
	// time, like a router whose clock is wrong.
	clockSkew time.Duration
}

func optionalRadius() *liveRadius {
	addr := os.Getenv("TEST_RADIUS_ADDR")
	if addr == "" {
		return nil
	}
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	port, _ := strconv.Atoi(p)
	return &liveRadius{host: host, port: port, secret: os.Getenv("TEST_RADIUS_SECRET")}
}

func needRadius(t testing.TB) *liveRadius {
	t.Helper()
	r := optionalRadius()
	if r == nil {
		t.Skip("TEST_RADIUS_ADDR not set; skipping real FreeRADIUS test")
	}
	return r
}

// login sends an Access-Request. UDP may drop a packet, so a missing answer is retried; a
// server that never answers is reported as such, not as a refusal.
func (r *liveRadius) login(user, pass string) SimResult {
	var res SimResult
	for attempt := 0; attempt < 3; attempt++ {
		res = runRADIUSSimulation(r.host, r.port, r.secret, user, pass, "", "", 5*time.Second)
		if res.Success {
			return res
		}
	}
	return res
}

func (r *liveRadius) accepts(t testing.TB, user, pass string) bool {
	t.Helper()
	res := r.login(user, pass)
	if !res.Success {
		t.Fatalf("FreeRADIUS gave no valid answer for %s: %s", user, res.Error)
	}
	return res.Reply == "Access-Accept"
}

const (
	acctStart   = 1
	acctStop    = 2
	acctInterim = 3
	acctOn      = 7

	attrAcctStatusType     = 40
	attrAcctInputOctets    = 42
	attrAcctOutputOctets   = 43
	attrAcctSessionTime    = 46
	attrAcctInputGigawords = 52
	attrAcctOutputGiga     = 53
	attrEventTimestamp     = 55
	codeAccountingRequest  = 4
	codeAccountingResponse = 5
)

func u32(v uint64) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(v))
	return b
}

// account sends an Accounting-Request (Start, Interim-Update, Stop or Accounting-On) the way
// a router does and waits for the Accounting-Response, which FreeRADIUS sends only after the
// row is written to radacct.
func (r *liveRadius) account(t testing.TB, status int, user, session string, octetsIn, octetsOut uint64, seconds int) {
	t.Helper()
	attrs := []radAttr{
		{attrAcctStatusType, u32(uint64(status))},
		{attrNASIPAddress, net.ParseIP(r.host).To4()},
	}
	if r.clockSkew != 0 {
		attrs = append(attrs, radAttr{attrEventTimestamp, u32(uint64(time.Now().Add(r.clockSkew).Unix()))})
	}
	if status != acctOn {
		attrs = append(attrs,
			radAttr{attrUserName, []byte(user)},
			radAttr{attrAcctSessionID, []byte(session)},
			radAttr{attrNASPort, u32(1)},
			radAttr{attrAcctInputOctets, u32(octetsIn & 0xffffffff)},
			radAttr{attrAcctInputGigawords, u32(octetsIn >> 32)},
			radAttr{attrAcctOutputOctets, u32(octetsOut & 0xffffffff)},
			radAttr{attrAcctOutputGiga, u32(octetsOut >> 32)},
			radAttr{attrAcctSessionTime, u32(uint64(seconds))},
		)
	}
	addr := net.JoinHostPort(r.host, strconv.Itoa(r.port+1))
	for attempt := 0; ; attempt++ {
		pkt, err := buildPacket(codeAccountingRequest, r.secret, attrs, nil)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := sendRADIUSPacket(context.Background(), addr, pkt, 5*time.Second)
		if err == nil {
			if err := verifyResponse(pkt, reply, r.secret); err != nil {
				t.Fatalf("accounting reply: %v", err)
			}
			if reply[0] != codeAccountingResponse {
				t.Fatalf("accounting reply code %d", reply[0])
			}
			return
		}
		if attempt == 2 {
			t.Fatalf("no Accounting-Response for %s/%s: %v", user, session, err)
		}
	}
}

// radcheckOf prints a customer's radcheck rows for failure messages.
func radcheckOf(db *database.DB, user string) string {
	rows, err := db.Query(`SELECT attribute, op, value FROM radcheck WHERE username=$1 AND attribute <> 'Cleartext-Password' ORDER BY id`, user)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var a, o, v string
		rows.Scan(&a, &o, &v)
		parts = append(parts, a+" "+o+" "+v)
	}
	if len(parts) == 0 {
		return "(no rows)"
	}
	return strings.Join(parts, "; ")
}

func runDailyTasks(db *database.DB) {
	for _, task := range []string{"mark_overdue_invoices", "expire_accounts", "renew_plans"} {
		runTask(db, testLogger(), task)
	}
}

// checkRealRadius asks FreeRADIUS about every customer the caller knows the password of and
// compares the answer with what the books say. Customers who are gone must be refused.
func checkRealRadius(r *liveRadius, db *database.DB, passwords map[string]string) []string {
	facts, err := expectedAccess(db)
	if err != nil {
		return []string{"expected access: " + err.Error()}
	}
	type job struct{ name, pass string }
	jobs := make(chan job)
	var mu sync.Mutex
	var bad []string
	var wg sync.WaitGroup
	for w := 0; w < 64; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				res := r.login(j.name, j.pass)
				var msg string
				f := facts[j.name]
				want := f != nil && f.want
				switch {
				case !res.Success:
					msg = fmt.Sprintf("R0 %s: FreeRADIUS gave no answer: %s", j.name, res.Error)
				case (res.Reply == "Access-Accept") != want:
					state := "deleted"
					if f != nil {
						state = fmt.Sprintf("status=%s paid_up=%v", f.status, f.paidUp)
					}
					msg = fmt.Sprintf("R1 %s: %s, books say allowed=%v (%s); radcheck: %s",
						j.name, res.Reply, want, state, radcheckOf(db, j.name))
				}
				if msg != "" {
					mu.Lock()
					bad = append(bad, msg)
					mu.Unlock()
				}
			}
		}()
	}
	for name, pass := range passwords {
		jobs <- job{name, pass}
	}
	close(jobs)
	wg.Wait()
	return bad
}

func setTestSettings(t testing.TB, db *database.DB, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		mustExec(t, db, `INSERT INTO system_settings (key, value) VALUES ($1,$2)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, k, v)
	}
	settings.Invalidate()
}

// One customer from sign-up to deletion; after every step the real FreeRADIUS must agree with
// the books, and so must the imitation the other tests rely on.
func TestRealRadiusFollowsBilling(t *testing.T) {
	r := needRadius(t)
	db := resetTestDB(t)
	h := newTestHandler(t)
	setTestSettings(t, db, map[string]string{"trial_days": "3", "billing_grace_days": "0", "renewal_invoice_days": "3"})
	plan := seedPlan(t, db, "Home 10", 1500, 30)

	const user, pass = "wanjiku", "s3cret-pw!"
	code, body := callHandler(h.CreateRadiusUser, nil, nil, map[string]interface{}{
		"username": user, "password": pass, "plan_id": plan.ID})
	if code != 201 {
		t.Fatalf("create customer: %d %v", code, body)
	}
	uid := int(body["id"].(float64))
	param := gin.Params{{Key: "id", Value: strconv.Itoa(uid)}}

	step := func(label string, want bool) {
		t.Helper()
		got := r.accepts(t, user, pass)
		imitation, why := radiusDecisionFor(t, db, user)
		if got != want {
			t.Errorf("%s: FreeRADIUS accepted=%v, want %v; radcheck: %s", label, got, want, radcheckOf(db, user))
		}
		if imitation != got {
			t.Errorf("%s: test imitation says %v (%s) but real FreeRADIUS says %v", label, imitation, why, got)
		}
	}
	day := func(n int) {
		for i := 0; i < n; i++ {
			advanceOneDay(t, db)
			runDailyTasks(db)
		}
	}

	step("new customer on free trial", true)
	if r.accepts(t, user, "wrong-password") {
		t.Error("wrong password accepted")
	}
	day(3)
	step("last day of the 3-day trial", true)
	day(1)
	step("first day after the trial, unpaid", false)

	inv := scanInt(t, db, `SELECT id FROM invoices WHERE user_id=$1 AND status IN ('pending','overdue')`, uid)
	first, err := payTx(db, paymentInput{InvoiceID: &inv, Amount: 1500, PaymentMethod: "mobile_money", Gateway: "mpesa", GatewayRef: "QAB1CD2EF3"})
	if err != nil {
		t.Fatal(err)
	}
	step("seconds after paying (no restart, no wait)", true)

	// Session-Timeout must stop a session that is still online at the paid-until midnight.
	res := r.login(user, pass)
	var expiry time.Time
	db.QueryRow(`SELECT account_expiry FROM radius_users WHERE id=$1`, uid).Scan(&expiry)
	midnight := time.Date(expiry.Year(), expiry.Month(), expiry.Day()+1, 0, 0, 0, 0, time.Local)
	st, _ := strconv.ParseInt(res.ReplyAttrs["Session-Timeout"], 0, 64) // shown as 0x… by decodeReplyAttrs
	if limit := int64(time.Until(midnight).Seconds()) + 2; st <= 0 || st > limit {
		t.Errorf("Session-Timeout %q; want 1..%d (seconds until paid-until midnight %s)", res.ReplyAttrs["Session-Timeout"], limit, midnight)
	}

	left := scanInt(t, db, `SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE id=$1`, uid)
	day(left)
	step("last paid day", true)
	day(1)
	step("day after the paid period, no grace", false)

	// The customer asks for more time; staff give them 2 grace days.
	if code, b := callHandler(h.UpdateRadiusUser, param, nil, map[string]interface{}{"grace_days": 2}); code != 200 {
		t.Fatalf("grace: %d %v", code, b)
	}
	step("staff gave 2 grace days", true)
	day(1)
	step("second grace day", true)
	day(1)
	step("grace used up", false)

	renewal := scanInt(t, db, `SELECT id FROM invoices WHERE user_id=$1 AND status IN ('pending','overdue') ORDER BY id DESC LIMIT 1`, uid)
	second, err := payTx(db, paymentInput{InvoiceID: &renewal, Amount: 1500, PaymentMethod: "cash", IdempotencyKey: "till-0001"})
	if err != nil {
		t.Fatal(err)
	}
	step("paid the renewal late", true)

	callHandler(h.SuspendRadiusUser, param, nil, nil)
	step("suspended by staff", false)
	callHandler(h.ActivateRadiusUser, param, nil, nil)
	step("re-activated by staff", true)

	if _, err := reverseTx(db, second.PaymentID); err != nil {
		t.Fatal(err)
	}
	step("renewal payment reversed", false)
	if _, err := reverseTx(db, first.PaymentID); err != nil {
		t.Fatal(err)
	}
	step("both payments reversed", false)

	if code, b := callHandler(h.DeleteRadiusUser, param, nil, nil); code != 200 {
		t.Fatalf("delete: %d %v", code, b)
	}
	if r.accepts(t, user, pass) {
		t.Errorf("deleted customer still accepted; radcheck: %s", radcheckOf(db, user))
	}
	failIf(t, checkMoney(db))
}

// payingCustomer creates a customer with a paid period on the given plan and returns its id.
func payingCustomer(t *testing.T, db *database.DB, h *Handler, user, pass string, p *planRow) int {
	t.Helper()
	code, body := callHandler(h.CreateRadiusUser, nil, nil, map[string]interface{}{
		"username": user, "password": pass, "plan_id": p.ID, "trial_days": 0})
	if code != 201 {
		t.Fatalf("create %s: %d %v", user, code, body)
	}
	uid := int(body["id"].(float64))
	inv := scanInt(t, db, `SELECT id FROM invoices WHERE user_id=$1 AND status='pending'`, uid)
	if _, err := payTx(db, paymentInput{InvoiceID: &inv, Amount: p.Price}); err != nil {
		t.Fatal(err)
	}
	return uid
}

// The device limit (Simultaneous-Use) is enforced by FreeRADIUS from open accounting rows.
func TestRealRadiusDeviceLimit(t *testing.T) {
	r := needRadius(t)
	db := resetTestDB(t)
	h := newTestHandler(t)
	setTestSettings(t, db, map[string]string{"trial_days": "0"})
	mustExec(t, db, `TRUNCATE nasreload`)
	plan := seedPlan(t, db, "Family", 2500, 30)
	mustExec(t, db, `UPDATE user_plans SET max_devices=2 WHERE id=$1`, plan.ID)
	plan, _ = loadPlan(db, plan.ID)
	const user, pass = "otieno", "family-pass"
	payingCustomer(t, db, h, user, pass, plan)

	if !r.accepts(t, user, pass) {
		t.Fatalf("paid customer refused; radcheck: %s", radcheckOf(db, user))
	}
	r.account(t, acctStart, user, "s1", 0, 0, 0)
	if !r.accepts(t, user, pass) {
		t.Error("second device refused with a limit of 2")
	}
	r.account(t, acctStart, user, "s2", 0, 0, 0)
	if r.accepts(t, user, pass) {
		t.Error("third device accepted with a limit of 2")
	}
	r.account(t, acctStop, user, "s1", 1000, 2000, 60)
	if !r.accepts(t, user, pass) {
		t.Error("device refused after another one logged off")
	}
	// The router reboots: its old sessions are gone even though no Stop arrived.
	r.account(t, acctStart, user, "s3", 0, 0, 0)
	if r.accepts(t, user, pass) {
		t.Error("third device accepted with a limit of 2 (before NAS reboot)")
	}
	time.Sleep(1100 * time.Millisecond) // nasreload compares timestamps at one-second resolution
	r.account(t, acctOn, "", "", 0, 0, 0)
	if !r.accepts(t, user, pass) {
		t.Error("refused after the router rebooted (Accounting-On): stale sessions still counted")
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM radacct WHERE username=$1`, user); n != 3 {
		t.Errorf("%d radacct rows, want 3 (accounting not stored?)", n)
	}
}

// A plan data cap (Max-Octets) is enforced at login from radacct, and a new paid period gives
// the full allowance back.
func TestRealRadiusDataCap(t *testing.T) {
	r := needRadius(t)
	db := resetTestDB(t)
	h := newTestHandler(t)
	setTestSettings(t, db, map[string]string{"trial_days": "0", "renewal_invoice_days": "3"})
	plan := seedPlan(t, db, "Capped 1MB", 500, 30)
	mustExec(t, db, `UPDATE user_plans SET data_limit_mb=1, max_devices=5 WHERE id=$1`, plan.ID)
	plan, _ = loadPlan(db, plan.ID)
	const user, pass = "kamau", "cap-pass-1"
	uid := payingCustomer(t, db, h, user, pass, plan)

	if !r.accepts(t, user, pass) {
		t.Fatalf("fresh capped customer refused; radcheck: %s", radcheckOf(db, user))
	}
	r.account(t, acctStart, user, "c1", 0, 0, 0)
	r.account(t, acctInterim, user, "c1", 400_000, 400_000, 300)
	if !r.accepts(t, user, pass) {
		t.Error("refused at 800 kB of a 1 MB cap")
	}
	r.account(t, acctStop, user, "c1", 600_000, 600_000, 600)
	if r.accepts(t, user, pass) {
		t.Error("accepted after using 1.2 MB of a 1 MB cap")
	}
	// Large counters: over 4 GB a router reports Gigawords; the total must still count.
	mustExec(t, db, `DELETE FROM radacct WHERE username=$1`, user)
	mustExec(t, db, `UPDATE radcheck SET value='5368709120' WHERE username=$1 AND attribute='Max-Octets'`, user) // 5 GiB
	r.account(t, acctStart, user, "g1", 0, 0, 0)
	r.account(t, acctStop, user, "g1", 3<<30, 3<<30, 600) // 6 GiB in total, needs gigawords
	if r.accepts(t, user, pass) {
		t.Error("accepted after 6 GiB of a 5 GiB cap (gigawords lost?)")
	}
	mustExec(t, db, `UPDATE radcheck SET value='1048576' WHERE username=$1 AND attribute='Max-Octets'`, user)

	// The customer pays early for the next period, the same day they ran out.
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE + 1 WHERE id=$1`, uid)
	runDailyTasks(db)
	next := scanInt(t, db, `SELECT id FROM invoices WHERE user_id=$1 AND status='pending'`, uid)
	if _, err := payTx(db, paymentInput{InvoiceID: &next, Amount: plan.Price}); err != nil {
		t.Fatal(err)
	}
	if !r.accepts(t, user, pass) {
		t.Errorf("paid for a new period after running out of data, still refused the same day; period_start=%s",
			scanString(t, db, `SELECT period_start::text FROM radius_users WHERE id=$1`, uid))
	}

	// The new period's traffic counts even from a router whose clock is 10 minutes slow: its
	// session looks as if it started before the payment.
	r.clockSkew = -10 * time.Minute
	r.account(t, acctStart, user, "c2", 0, 0, 0)
	r.account(t, acctStop, user, "c2", 700_000, 700_000, 60)
	if r.accepts(t, user, pass) {
		t.Error("accepted after using 1.4 MB of the new 1 MB allowance on a router with a slow clock")
	}
	// The quota task (which disconnects people who are online over the cap) counts the same way.
	nas := startFakeNAS(t, db, r.secret, "ack")
	r.account(t, acctStart, user, "c3", 0, 0, 0)
	if n := runTaskCount(db, "enforce_quota"); n != 1 || len(nas.received()) != 1 {
		t.Errorf("enforce_quota disconnected %d session(s) (router got %v), want 1", n, nas.received())
	}
}

func runTaskCount(db *database.DB, task string) int {
	_, n := runTask(db, testLogger(), task)
	return n
}

func scanString(t testing.TB, q dbtx, query string, args ...interface{}) string {
	t.Helper()
	var s *string
	if err := q.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", firstLine(query), err)
	}
	if s == nil {
		return "NULL"
	}
	return *s
}

// startRadiusInstance runs another FreeRADIUS from scripts/test-radius.sh with extra settings
// (another port and directory), stopped when the test ends.
func startRadiusInstance(t *testing.T, port int, env ...string) *liveRadius {
	t.Helper()
	script, _ := filepath.Abs("../../../scripts/test-radius.sh")
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("radius-manager-instance-%d", port))
	env = append(append(os.Environ(), "TEST_RADIUS_DIR="+dir, "TEST_RADIUS_PORT="+strconv.Itoa(port),
		"TEST_PG_PORT="+testPGPort()), env...)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start FreeRADIUS: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		stop.Run()
	})
	return &liveRadius{host: "127.0.0.1", port: port, secret: "testing123-e2e"}
}

// FreeRADIUS reaching a database elsewhere (Supabase, RDS) over TLS, logging in with a
// password full of characters that break naive quoting. init.sh builds the connection string.
func TestRealRadiusOverTLS(t *testing.T) {
	needRadius(t)
	db := resetTestDB(t)
	const pass = `p'a\ss "w|o&rd $x`
	db.Exec(`DROP OWNED BY tls_radius`)
	db.Exec(`DROP ROLE IF EXISTS tls_radius`)
	mustExec(t, db, fmt.Sprintf(`CREATE ROLE tls_radius LOGIN PASSWORD %s`, pq.QuoteLiteral(pass)))
	mustExec(t, db, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO tls_radius`)
	mustExec(t, db, `GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO tls_radius`)
	t.Cleanup(func() { db.Exec(`DROP OWNED BY tls_radius`); db.Exec(`DROP ROLE IF EXISTS tls_radius`) })
	mustExec(t, db, `INSERT INTO radcheck (username, attribute, op, value) VALUES ('tlsuser','Cleartext-Password',':=','pw')`)

	r := startRadiusInstance(t, 18170, "TEST_RADIUS_DB_USER=tls_radius", "TEST_RADIUS_DB_PASS="+pass, "TEST_RADIUS_DB_SSLMODE=require")
	if !r.accepts(t, "tlsuser", "pw") {
		t.Fatalf("FreeRADIUS over TLS refused a valid login")
	}
	r.account(t, acctStart, "tlsuser", "tls-1", 0, 0, 0)
	var ssl bool
	if err := db.QueryRow(`SELECT bool_and(s.ssl) FROM pg_stat_ssl s JOIN pg_stat_activity a ON a.pid = s.pid
		WHERE a.usename = 'tls_radius'`).Scan(&ssl); err != nil || !ssl {
		t.Errorf("FreeRADIUS's database connections are not encrypted (ssl=%v, %v)", ssl, err)
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM radacct WHERE acctsessionid='tls-1'`); n != 1 {
		t.Error("accounting over TLS not stored")
	}
}
