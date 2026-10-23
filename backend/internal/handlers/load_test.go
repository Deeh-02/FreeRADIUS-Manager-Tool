package handlers

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeradius-manager/backend/internal/database"
)

// ─────────────────────────────────────────────────────────────────────────────
// Load tests (docs/phase.md A.11–A.15)
// ─────────────────────────────────────────────────────────────────────────────
//
// They need LOAD_TEST=1 and the test stack (scripts/test-stack.sh start). Sizes are set with
// environment variables; the defaults are for a laptop. Every test checks that the answers are
// still right under load, not just fast.
//
//	LOAD_CUSTOMERS     customers                         (default 5000)
//	LOAD_CONCURRENCY   simultaneous RADIUS clients        (default 64)
//	LOAD_SECONDS       length of the login run            (default 20)
//	LOAD_RADACCT_ROWS  usage rows for A.13                (default 2000000)
//	LOAD_INVOICES      invoices for A.15                  (default 100000)

func needLoad(t *testing.T) {
	if os.Getenv("LOAD_TEST") == "" {
		t.Skip("LOAD_TEST not set")
	}
}

type latencies struct {
	mu sync.Mutex
	d  []time.Duration
}

func (l *latencies) add(d time.Duration) {
	l.mu.Lock()
	l.d = append(l.d, d)
	l.mu.Unlock()
}

func (l *latencies) summary() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.d) == 0 {
		return "no samples"
	}
	sort.Slice(l.d, func(i, j int) bool { return l.d[i] < l.d[j] })
	p := func(q float64) time.Duration { return l.d[int(q*float64(len(l.d)-1))].Round(100 * time.Microsecond) }
	return fmt.Sprintf("n=%d p50=%s p95=%s p99=%s max=%s", len(l.d), p(.5), p(.95), p(.99), l.d[len(l.d)-1].Round(time.Millisecond))
}

func (l *latencies) pct(q float64) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.d) == 0 {
		return 0
	}
	sort.Slice(l.d, func(i, j int) bool { return l.d[i] < l.d[j] })
	return l.d[int(q*float64(len(l.d)-1))]
}

// seedLoadCustomers creates n customers in the mix a real ISP has (mostly paid, some expired,
// some suspended, some capped) with their radcheck rows written by the real syncAccess.
func seedLoadCustomers(t *testing.T, db *database.DB, n int) {
	t.Helper()
	t0 := time.Now()
	plan := seedPlan(t, db, "Load", 1500, 30)
	capped := seedPlan(t, db, "Load capped", 1000, 30)
	mustExec(t, db, `UPDATE user_plans SET data_limit_mb = 10240 WHERE id=$1`, capped.ID)
	mustExec(t, db, `
		INSERT INTO radius_users (username, password, status, account_expiry, plan_id, device_limit)
		SELECT 'load' || lpad(g::text, 6, '0'), 'x',
		       CASE WHEN g % 10 = 0 THEN 'expired' WHEN g % 47 = 0 THEN 'suspended' ELSE 'active' END,
		       CURRENT_DATE + CASE WHEN g % 10 = 0 THEN -3 ELSE g % 30 END,
		       CASE WHEN g % 5 = 0 THEN $2::int ELSE $1::int END, 2
		FROM generate_series(1, $3::int) g`, plan.ID, capped.ID, n)
	mustExec(t, db, `INSERT INTO radcheck (username, attribute, op, value)
		SELECT username, 'Cleartext-Password', ':=', 'pw-' || username FROM radius_users WHERE username LIKE 'load%'`)
	mustExec(t, db, `INSERT INTO radcheck (username, attribute, op, value)
		SELECT ru.username, 'Max-Octets', ':=', (10240::bigint * 1024 * 1024)::text
		FROM radius_users ru WHERE ru.plan_id = $1`, capped.ID)
	var names []string
	rows, _ := db.Query(`SELECT username FROM radius_users WHERE username LIKE 'load%'`)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		names = append(names, s)
	}
	rows.Close()
	work := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range work {
				if err := syncAccess(db, u); err != nil {
					t.Errorf("sync %s: %v", u, err)
				}
			}
		}()
	}
	for _, u := range names {
		work <- u
	}
	close(work)
	wg.Wait()
	mustExec(t, db, `ANALYZE`)
	t.Logf("seeded %d customers in %s", n, time.Since(t0).Round(time.Millisecond))
}

// A.11 Logins at full speed, from many routers at once. Every answer must still match the
// books; none may be lost.
func TestLoadRadiusLogins(t *testing.T) {
	needLoad(t)
	r := needRadius(t)
	db := resetTestDB(t)
	n := simEnv("LOAD_CUSTOMERS", 5000)
	seedLoadCustomers(t, db, n)
	facts, err := expectedAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(facts))
	for k := range facts {
		names = append(names, k)
	}

	conc := simEnv("LOAD_CONCURRENCY", 64)
	dur := time.Duration(simEnv("LOAD_SECONDS", 20)) * time.Second
	var acc, rej latencies
	var total, wrong, lost int64
	var wrongMu sync.Mutex
	var wrongSample []string
	stop := time.Now().Add(dur)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for time.Now().Before(stop) {
				u := names[rng.Intn(len(names))]
				t0 := time.Now()
				res := runRADIUSSimulation(r.host, r.port, r.secret, u, "pw-"+u, "", "", 5*time.Second)
				d := time.Since(t0)
				atomic.AddInt64(&total, 1)
				if !res.Success {
					atomic.AddInt64(&lost, 1)
					continue
				}
				ok := res.Reply == "Access-Accept"
				if ok {
					acc.add(d)
				} else {
					rej.add(d)
				}
				if ok != facts[u].want {
					atomic.AddInt64(&wrong, 1)
					wrongMu.Lock()
					if len(wrongSample) < 5 {
						wrongSample = append(wrongSample, fmt.Sprintf("%s got %s want %v", u, res.Reply, facts[u].want))
					}
					wrongMu.Unlock()
				}
			}
		}(int64(w))
	}
	wg.Wait()
	rate := float64(total) / dur.Seconds()
	t.Logf("%d customers, %d concurrent clients, %s: %d logins = %.0f/s; lost %d, wrong %d", n, conc, dur, total, rate, lost, wrong)
	t.Logf("accepted: %s", acc.summary())
	t.Logf("rejected: %s (includes FreeRADIUS reject_delay = 1s, on purpose: slows password guessing)", rej.summary())
	if wrong > 0 {
		t.Errorf("%d answers did not match the books under load, e.g. %v", wrong, wrongSample)
	}
	if lost > total/1000 {
		t.Errorf("%d of %d logins got no answer (over 0.1%%)", lost, total)
	}
	if p := acc.pct(.99); p > 500*time.Millisecond {
		t.Errorf("accepted logins p99 %s; a router gives up after a few seconds", p)
	}
}

// A.12 Accounting flood: thousands of sessions starting, sending interim updates and stopping.
// Every update must be stored (FreeRADIUS answers only after writing it), none lost.
func TestLoadAccountingFlood(t *testing.T) {
	needLoad(t)
	r := needRadius(t)
	db := resetTestDB(t)
	mustExec(t, db, `TRUNCATE radacct`)
	sessions := simEnv("LOAD_SESSIONS", 5000)
	rounds := simEnv("LOAD_INTERIM_ROUNDS", 4)
	conc := simEnv("LOAD_CONCURRENCY", 64)

	var lat latencies
	send := func(status int, k int, octets uint64) {
		t0 := time.Now()
		r.account(t, status, fmt.Sprintf("acct%05d", k%2000), fmt.Sprintf("flood-%06d", k), octets, octets/2, int(octets/1000))
		lat.add(time.Since(t0))
	}
	phase := func(status int, octets uint64) time.Duration {
		t0 := time.Now()
		work := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < conc; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := range work {
					send(status, k, octets)
				}
			}()
		}
		for k := 0; k < sessions; k++ {
			work <- k
		}
		close(work)
		wg.Wait()
		return time.Since(t0)
	}
	total := 0
	elapsed := phase(acctStart, 0)
	total += sessions
	for i := 1; i <= rounds; i++ {
		elapsed += phase(acctInterim, uint64(i)*1_000_000)
		total += sessions
	}
	elapsed += phase(acctStop, uint64(rounds+1)*1_000_000)
	total += sessions
	t.Logf("%d accounting packets (%d sessions × start/%d interim/stop) in %s = %.0f/s; %s",
		total, sessions, rounds, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), lat.summary())
	t.Logf("for scale: 5,000 online customers with 5-minute interim updates send about 17 per second")

	if n := scanInt(t, db, `SELECT COUNT(*) FROM radacct WHERE acctsessionid LIKE 'flood-%'`); n != sessions {
		t.Errorf("%d radacct rows, want %d", n, sessions)
	}
	if n := scanInt(t, db, `SELECT COUNT(*) FROM radacct WHERE acctsessionid LIKE 'flood-%' AND acctstoptime IS NULL`); n != 0 {
		t.Errorf("%d sessions still open after their Stop", n)
	}
	want := int64(rounds+1) * 1_000_000
	if n := scanInt(t, db, `SELECT COUNT(*) FROM radacct WHERE acctsessionid LIKE 'flood-%' AND acctinputoctets <> $1`, want); n != 0 {
		t.Errorf("%d sessions with the wrong final byte count (lost or reordered updates)", n)
	}
}

// A.13 Login speed and the data-cap total with millions of usage rows.
func TestLoadBigUsageTable(t *testing.T) {
	needLoad(t)
	r := needRadius(t)
	db := resetTestDB(t)
	h := newTestHandler(t)
	users := simEnv("LOAD_CUSTOMERS", 5000)
	seedLoadCustomers(t, db, users)
	rowsWanted := simEnv("LOAD_RADACCT_ROWS", 2000000)
	t0 := time.Now()
	mustExec(t, db, `TRUNCATE radacct`)
	mustExec(t, db, `
		INSERT INTO radacct (acctsessionid, acctuniqueid, username, nasipaddress, acctstarttime, acctupdatetime,
		                     acctstoptime, acctsessiontime, acctinputoctets, acctoutputoctets, callingstationid)
		SELECT 'big-' || g, 'bigu-' || g, 'load' || lpad((1 + g % $2)::text, 6, '0'), ('10.0.0.' || (1 + g % 20))::inet,
		       NOW() - (g % 400) * INTERVAL '1 day' - (g % 1440) * INTERVAL '1 minute',
		       NOW() - (g % 400) * INTERVAL '1 day',
		       CASE WHEN g % 1000 = 0 THEN NULL ELSE NOW() - (g % 400) * INTERVAL '1 day' + INTERVAL '1 hour' END,
		       3600, (g % 50) * 100000, (g % 70) * 100000, 'AA:BB:CC:' || lpad(to_hex(g % 255), 2, '0') || ':00:01'
		FROM generate_series(1, $1::int) g`, rowsWanted, users)
	mustExec(t, db, `ANALYZE radacct`)
	t.Logf("loaded %d usage rows in %s", rowsWanted, time.Since(t0).Round(time.Second))
	// Every capped customer's current period started 30 days ago.
	mustExec(t, db, `UPDATE radius_users SET period_acct_floor = (SELECT MAX(radacctid) FROM radacct WHERE acctstarttime < NOW() - INTERVAL '30 days')
		WHERE id IN (SELECT ru.id FROM radius_users ru JOIN radcheck rc ON rc.username = ru.username AND rc.attribute = 'Max-Octets')`)

	// Logins of capped customers run the data-cap total and the device count over radacct.
	var capped []string
	rows, _ := db.Query(`SELECT username FROM radcheck WHERE attribute='Max-Octets' LIMIT 500`)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		capped = append(capped, s)
	}
	rows.Close()
	var lat latencies
	var wg sync.WaitGroup
	work := make(chan string)
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range work {
				t0 := time.Now()
				res := r.login(u, "pw-"+u)
				if res.Success && res.Reply == "Access-Accept" {
					lat.add(time.Since(t0))
				}
			}
		}()
	}
	for _, u := range capped {
		work <- u
	}
	close(work)
	wg.Wait()
	t.Logf("capped customers' logins over %d usage rows: %s", rowsWanted, lat.summary())
	if p := lat.pct(.95); p > 200*time.Millisecond {
		t.Errorf("login p95 %s with %d usage rows; needs an index", p, rowsWanted)
	}

	// The staff pages and tasks that read radacct.
	timeIt := func(label string, limit time.Duration, fn func()) {
		t0 := time.Now()
		fn()
		d := time.Since(t0)
		t.Logf("%-38s %s", label, d.Round(time.Millisecond))
		if d > limit {
			t.Errorf("%s took %s (limit %s) with %d usage rows", label, d.Round(time.Millisecond), limit, rowsWanted)
		}
	}
	timeIt("task enforce_quota", 10*time.Second, func() { runTask(db, testLogger(), "enforce_quota") })
	timeIt("task cleanup_sessions", 10*time.Second, func() { runTask(db, testLogger(), "cleanup_sessions") })
	for _, ep := range []struct {
		name string
		fn   func()
	}{
		{"GET /statistics/dashboard", func() { callHandler(h.DashboardStats, nil, nil, nil) }},
		{"GET /sessions/active", func() { callHandler(h.ActiveSessions, nil, nil, nil) }},
		{"GET /reports/usage", func() { callHandler(h.UsageReport, nil, nil, nil) }},
		{"GET /reports/usage/daily", func() { callHandler(h.DailyUsageReport, nil, nil, nil) }},
		{"GET /reports/nas", func() { callHandler(h.NASUsageReport, nil, nil, nil) }},
		{"GET /live/stats/current", func() { callHandler(h.GetCurrentStats, nil, nil, nil) }},
		{"GET /radius/users (list)", func() { callHandler(h.ListRadiusUsers, nil, nil, nil) }},
	} {
		timeIt(ep.name, 2*time.Second, ep.fn)
	}
}

// A.14 + A.15 The billing tasks over many customers while staff use the panel, then the
// Billing and Payments pages with a hundred thousand invoices.
func TestLoadBillingAtScale(t *testing.T) {
	needLoad(t)
	a, db := needStack(t)
	nInv := simEnv("LOAD_INVOICES", 100000)
	nCust := simEnv("LOAD_CUSTOMERS", 5000)
	a.setSettings(map[string]string{"renewal_invoice_days": "3"})
	t0 := time.Now()
	plan := seedPlan(t, db, "Scale", 1500, 30)
	mustExec(t, db, `
		INSERT INTO radius_users (username, password, status, account_expiry, plan_id)
		SELECT 'scale' || lpad(g::text, 6, '0'), 'x', 'active', CURRENT_DATE + (g % 40) - 5, $1
		FROM generate_series(1, $2) g`, plan.ID, nCust)
	mustExec(t, db, `INSERT INTO radcheck (username, attribute, op, value)
		SELECT username, 'Cleartext-Password', ':=', 'x' FROM radius_users`)
	// History: paid invoices with their payments, spread over three years.
	mustExec(t, db, `
		INSERT INTO invoices (invoice_number, user_id, username, plan_id, plan_name, amount, currency, status,
		                      due_date, paid_at, created_at, service_applied, applied_days, plan_days)
		SELECT 'H-' || g, ru.id, ru.username, $1, 'Scale', 1500, 'KES', 'paid',
		       CURRENT_DATE - (g % 1000), NOW() - (g % 1000) * INTERVAL '1 day', NOW() - (g % 1000) * INTERVAL '1 day',
		       TRUE, 30, 30
		FROM generate_series(1, $2::int) g JOIN radius_users ru ON ru.id = 1 + g % $3::int`, plan.ID, nInv, nCust)
	mustExec(t, db, `
		INSERT INTO payments (receipt_number, invoice_id, username, amount, currency, payment_method, status, created_at)
		SELECT 'HR-' || i.id, i.id, i.username, i.amount, 'KES', 'mobile_money', 'confirmed', i.paid_at FROM invoices i`)
	mustExec(t, db, `ANALYZE`)
	t.Logf("seeded %d customers, %d invoices and payments in %s", nCust, nInv, time.Since(t0).Round(time.Second))

	// The tasks on their own first, on a copy of the expiry dates (restored afterwards), so
	// their own speed is known apart from the load below.
	mustExec(t, db, `CREATE TEMP TABLE IF NOT EXISTS expiry_backup AS SELECT id, account_expiry, status FROM radius_users`)
	for _, task := range []string{"expire_accounts", "renew_plans"} {
		t0 := time.Now()
		msg := a.runTask(db, task)
		t.Logf("alone: task %-22s %s: %s", task, time.Since(t0).Round(time.Millisecond), msg)
	}
	mustExec(t, db, `DELETE FROM invoices WHERE invoice_number NOT LIKE 'H-%'`)
	mustExec(t, db, `UPDATE radius_users ru SET account_expiry = b.account_expiry, status = b.status FROM expiry_backup b WHERE b.id = ru.id`)
	rows, _ := db.Query(`SELECT username FROM radius_users`)
	var all []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		all = append(all, s)
	}
	rows.Close()
	for _, u := range all {
		syncAccess(db, u)
	}

	// Staff keep working (lists, searches, payments) while the daily tasks run.
	var staffLat latencies
	var staffErr int64
	var baseline *latencies
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var payN int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			paths := []string{"/radius/users?search=scale0001", "/invoices", "/invoices?status=pending", "/payments",
				"/payments/summary", "/statistics/dashboard", "/radius/users?page=40"}
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				t0 := time.Now()
				var code int
				var err error
				if w == 0 && i%3 == 0 {
					// a walk-in customer pays cash
					n := atomic.AddInt64(&payN, 1)
					code, _, err = a.do("POST", "/payments", map[string]interface{}{"username": fmt.Sprintf("scale%06d", 1+n%int64(nCust)),
						"amount": 50, "payment_method": "cash"}, map[string]string{"Idempotency-Key": fmt.Sprintf("walkin-%d", n)})
				} else {
					code, _, err = a.do("GET", paths[(i+w)%len(paths)], nil, nil)
				}
				staffLat.add(time.Since(t0))
				if err != nil || code >= 300 {
					atomic.AddInt64(&staffErr, 1)
				}
			}
		}(w)
	}
	// Sample lock waits while the tasks run.
	var maxLockWaiters int64
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			var n int64
			db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'`).Scan(&n)
			if n > atomic.LoadInt64(&maxLockWaiters) {
				atomic.StoreInt64(&maxLockWaiters, n)
			}
		}
	}()
	time.Sleep(3 * time.Second) // baseline: staff load with no task running
	staffLat.mu.Lock()
	baseline = &latencies{d: append([]time.Duration(nil), staffLat.d...)}
	staffLat.d = nil
	staffLat.mu.Unlock()
	t.Logf("staff requests before the tasks (baseline): %s", baseline.summary())
	for _, task := range []string{"mark_overdue_invoices", "expire_accounts", "renew_plans"} {
		t0 := time.Now()
		msg := a.runTask(db, task)
		t.Logf("task %-22s %s: %s", task, time.Since(t0).Round(time.Millisecond), msg)
	}
	time.Sleep(time.Second)
	close(stop)
	wg.Wait()
	t.Logf("staff requests during the tasks: %s, errors %d, most sessions waiting on a lock at once: %d", staffLat.summary(), staffErr, maxLockWaiters)
	if staffErr > 0 {
		t.Errorf("%d staff requests failed while the tasks ran", staffErr)
	}
	if p := staffLat.pct(.95); p > 2*time.Second {
		t.Errorf("staff requests p95 %s while tasks ran", p)
	}
	failIf(t, checkMoney(db))

	// A.15 page speed with the history in place.
	for _, path := range []string{"/invoices", "/invoices?status=paid&page=2000", "/invoices?username=scale000123",
		"/payments", "/payments?page=3000", "/payments/summary", "/statistics/dashboard", "/radius/users",
		"/radius/users?search=scale00012", "/reports/usage"} {
		var lat latencies
		for i := 0; i < 5; i++ {
			t0 := time.Now()
			code, body, err := a.do("GET", path, nil, nil)
			lat.add(time.Since(t0))
			if err != nil || code != 200 {
				t.Errorf("GET %s: %d %v", path, code, strings.TrimSpace(fmt.Sprint(body))[:min(120, len(fmt.Sprint(body)))])
				break
			}
		}
		t.Logf("GET %-40s %s", path, lat.summary())
		if p := lat.pct(.5); p > time.Second {
			t.Errorf("GET %s median %s with %d invoices", path, p.Round(time.Millisecond), nInv)
		}
	}
}
