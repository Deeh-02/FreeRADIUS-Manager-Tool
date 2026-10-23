package handlers

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeradius-manager/backend/internal/database"
	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────────
// Time simulation: months of billing in minutes
// ─────────────────────────────────────────────────────────────────────────────
//
// The database clock cannot be moved, so the simulation moves the data instead: after each
// simulated day every date and timestamp in the database is shifted back by one day (and the
// Expiration text FreeRADIUS holds in radcheck with it), which is exactly what the passage of
// a day looks like from today's point of view. The real task code then runs against it.
//
// Customers behave in different ways (pay on time, late, in part, never, get reversed,
// switch plans, use promo codes, are deleted with open invoices) and after every day the
// invariants in invariants_test.go and below are checked.
//
//	SIM_DAYS       days to simulate           (default 120; 560 covers 29 Feb 2028)
//	SIM_CUSTOMERS  number of customers        (default 300)
//	SIM_SEED       random seed                (default 1)
//
// Full run:  SIM_DAYS=560 SIM_CUSTOMERS=1000 go test ./internal/handlers -run TimeSimulation -timeout 60m

func simEnv(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// advanceOneDay makes it one day later for everything the database remembers.
func advanceOneDay(t testing.TB, db *database.DB) {
	t.Helper()
	for _, q := range []string{
		`UPDATE radius_users SET account_expiry = account_expiry - 1, period_start = period_start - 1,
			last_reminder_for = last_reminder_for - 1, created_at = created_at - INTERVAL '1 day'`,
		`UPDATE invoices SET due_date = due_date - 1, period_end = period_end - 1,
			paid_at = paid_at - INTERVAL '1 day', created_at = created_at - INTERVAL '1 day'`,
		`UPDATE payments SET created_at = created_at - INTERVAL '1 day', reversed_at = reversed_at - INTERVAL '1 day'`,
		`UPDATE radcheck SET value = to_char(to_timestamp(value, 'Mon DD YYYY HH24:MI:SS') - INTERVAL '1 day',
			'Mon DD YYYY HH24:MI:SS') WHERE attribute = 'Expiration'`,
		`UPDATE promotions SET valid_from = valid_from - INTERVAL '1 day', valid_until = valid_until - INTERVAL '1 day'`,
		`UPDATE scheduled_tasks SET next_run = NULL`,
	} {
		mustExec(t, db, q)
	}
}

type simCustomer struct {
	id       int
	name     string
	profile  string // ontime | late | partial | never | flaky | postpaid | free
	planName string
	anchor   int // paid-until offset (days from today) minus the days granted by invoices
	deleted  bool
	paidOnce map[int]bool // invoices this customer already paid the first half of
	lateBy   map[int]int  // per-invoice random delay
}

type simPlan struct {
	row      *planRow
	currency string
}

type simulation struct {
	t        *testing.T
	db       *database.DB
	h        *Handler
	rng      *rand.Rand
	day      int
	custs    map[int]*simCustomer
	order    []int
	plans    []*simPlan
	money    map[string]float64 // what the harness believes is collected, per currency
	reopened map[int]bool       // invoices that were re-opened by a reversal
	keySeq   int
	refSeq   int
	stats    map[string]int
	spent    map[string]time.Duration
}

// timed adds the time fn takes to the named phase of the run report.
func (s *simulation) timed(phase string, fn func()) {
	t0 := time.Now()
	fn()
	s.spent[phase] += time.Since(t0)
}

func (s *simulation) stat(k string) { s.stats[k]++ }

func TestTimeSimulation(t *testing.T) {
	days := simEnv("SIM_DAYS", 120)
	nCust := simEnv("SIM_CUSTOMERS", 300)
	seed := int64(simEnv("SIM_SEED", 1))
	db := resetTestDB(t)
	mustExec(t, db, `INSERT INTO system_settings (key, value) VALUES
		('billing_grace_days','1'), ('renewal_invoice_days','3'), ('invoice_due_days','7'), ('trial_days','4')`)
	settings.Invalidate()

	s := &simulation{t: t, db: db, h: &Handler{db: db, log: testLogger()}, rng: rand.New(rand.NewSource(seed)),
		custs: map[int]*simCustomer{}, money: map[string]float64{}, reopened: map[int]bool{}, stats: map[string]int{}, spent: map[string]time.Duration{}}
	gin.SetMode(gin.TestMode)

	s.plans = []*simPlan{
		{row: seedPlan(t, db, "Basic", 10.00, 30), currency: "USD"},
		{row: seedPlan(t, db, "Pro", 25.00, 30), currency: "USD"},
		{row: seedPlan(t, db, "Weekly", 3.50, 7), currency: "USD"},
		{row: seedPlan(t, db, "Odd", 99.99, 31), currency: "USD"},
	}
	mustExec(t, db, `INSERT INTO user_plans (name, price, currency, validity_days) VALUES ('Mama', 1500, 'KES', 30)`)
	kes, _ := loadPlan(db, scanInt(t, db, `SELECT id FROM user_plans WHERE name='Mama'`))
	s.plans = append(s.plans, &simPlan{row: kes, currency: "KES"})
	freePlan := seedPlan(t, db, "Free", 0, 30)
	mustExec(t, db, `INSERT INTO promotions (code, discount_type, discount_value, max_uses) VALUES
		('PCT33','percent',33,0), ('FREE100','percent',100,40), ('FLAT5','fixed',5,0), ('HUGE','fixed',500,0)`)

	profiles := []struct {
		name string
		w    int
	}{{"ontime", 40}, {"late", 20}, {"partial", 15}, {"never", 10}, {"flaky", 5}, {"postpaid", 6}, {"free", 4}}
	pick := func() string {
		r := s.rng.Intn(100)
		for _, p := range profiles {
			if r < p.w {
				return p.name
			}
			r -= p.w
		}
		return "ontime"
	}

	signupDay := make([]int, nCust)
	for i := range signupDay {
		signupDay[i] = s.rng.Intn(1 + days/4)
	}
	started := time.Now()

	for s.day = 0; s.day < days; s.day++ {
		if s.day > 0 {
			s.timed("advance clock", func() { advanceOneDay(t, db) })
			for _, c := range s.custs {
				c.anchor--
			}
		}
		// ── the daily tasks, as the scheduler would run them ──
		for _, task := range []string{"mark_overdue_invoices", "expire_accounts", "renew_plans"} {
			s.timed("task "+task, func() { runTask(db, testLogger(), task) })
		}

		// ── new customers sign up ──
		for i, d := range signupDay {
			if d == s.day {
				s.signUp(i, pick(), freePlan)
			}
		}
		// ── customers and staff act ──
		s.timed("customers pay", s.actOnOpenInvoices)
		s.timed("staff actions", func() { s.randomStaffActions(freePlan) })

		// ── the books must still balance ──
		s.timed("invariant checks", s.checkDay)
		if t.Failed() {
			t.Fatalf("stopped at simulated day %d (seed %d)", s.day, seed)
		}
	}
	s.checkRevenueThroughApi()
	t.Logf("simulated %d days, %d customers in %s", days, len(s.custs), time.Since(started).Round(time.Second))
	keys := make([]string, 0, len(s.stats))
	for k := range s.stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-34s %d\n", k, s.stats[k])
	}
	t.Logf("events:\n%s", b.String())
	b.Reset()
	for k, v := range s.spent {
		fmt.Fprintf(&b, "  %-34s %s\n", k, v.Round(time.Millisecond))
	}
	t.Logf("time spent:\n%s", b.String())
}

// ── actors ──────────────────────────────────────────────────────────────────

func (s *simulation) signUp(i int, profile string, free *planRow) {
	name := fmt.Sprintf("cust%04d", i)
	var id int
	if err := s.db.QueryRow(`INSERT INTO radius_users (username, password) VALUES ($1,'x') RETURNING id`, name).Scan(&id); err != nil {
		s.t.Fatalf("sign up: %v", err)
	}
	mustExec(s.t, s.db, `INSERT INTO radcheck (username, attribute, op, value) VALUES ($1,'Cleartext-Password',':=','x')`, name)
	c := &simCustomer{id: id, name: name, profile: profile, anchor: -1, paidOnce: map[int]bool{}, lateBy: map[int]int{}}
	s.custs[id] = c
	s.order = append(s.order, id)
	s.stat("signups")

	var plan *planRow
	if profile == "free" {
		plan = free
	} else {
		plan = s.plans[s.rng.Intn(len(s.plans))].row
	}
	s.assign(c, plan, profile == "postpaid", s.maybePromo())
}

func (s *simulation) maybePromo() string {
	if s.rng.Intn(100) < 8 {
		return []string{"PCT33", "FREE100", "FLAT5", "HUGE"}[s.rng.Intn(4)]
	}
	return ""
}

// assign performs the staff "Assign plan" action and updates the harness's ledger anchor.
func (s *simulation) assign(c *simCustomer, plan *planRow, startNow bool, promo string) {
	priceNow := scanFloat(s.t, s.db, `SELECT price FROM user_plans WHERE id=$1`, plan.ID)
	res, err := assignTx(s.db, c.id, c.name, plan.ID, startNow, promo)
	if err != nil {
		s.stat("assign refused: " + shorten(err.Error()))
		return
	}
	s.stat("plan assignments")
	if promo != "" && res.InvoiceID != 0 {
		var amount, discount float64
		s.db.QueryRow(`SELECT amount, discount_amount FROM invoices WHERE id=$1`, res.InvoiceID).Scan(&amount, &discount)
		cents := func(f float64) int { return int(f*100 + 0.5) }
		if cents(amount)+cents(discount) != cents(priceNow) {
			s.t.Errorf("day %d: %s promo %s: amount %.2f + discount %.2f != plan price %.2f", s.day, c.name, promo, amount, discount, priceNow)
		}
		if amount < 0 {
			s.t.Errorf("day %d: negative invoice amount %.2f", s.day, amount)
		}
		s.stat("promo " + promo + " used")
	}
	after := s.expiryOffset(c.id)
	afterApplied := s.appliedDays(c.id)
	if startNow && plan.Price > 0 {
		// Granted straight from the old date: the ledger must still add up exactly.
		if after != c.anchor+afterApplied {
			s.t.Errorf("day %d: %s start-now: paid-until today%+d != baseline %+d + granted %d",
				s.day, c.name, after, c.anchor, afterApplied)
		}
	}
	// Otherwise the assignment may have blocked the customer (paid-until set to yesterday) or
	// settled a 100% promotion by granting a period: take the new baseline.
	c.anchor = after - afterApplied
	if plan.Price > 0 {
		c.planName = plan.Name
	}
}

func shorten(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

func (s *simulation) expiryOffset(uid int) int {
	var off *int
	s.db.QueryRow(`SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE id=$1`, uid).Scan(&off)
	if off == nil {
		return 0
	}
	return *off
}

func (s *simulation) appliedDays(uid int) int {
	return scanInt(s.t, s.db, `SELECT COALESCE(SUM(applied_days),0) FROM invoices WHERE user_id=$1 AND service_applied`, uid)
}

type openInvoice struct {
	id, userID int
	amount     float64
	paid       float64
	dueOffset  int // due date minus today
	currency   string
}

func (s *simulation) openInvoices() []openInvoice {
	rows, err := s.db.Query(`
		SELECT i.id, i.user_id, i.amount, i.currency, i.due_date - CURRENT_DATE,
		       COALESCE((SELECT SUM(p.amount) FROM payments p WHERE p.invoice_id=i.id AND p.status='confirmed'),0)
		FROM invoices i WHERE i.status IN ('pending','overdue') AND i.user_id IS NOT NULL ORDER BY i.id`)
	if err != nil {
		s.t.Fatalf("open invoices: %v", err)
	}
	defer rows.Close()
	var out []openInvoice
	for rows.Next() {
		var o openInvoice
		rows.Scan(&o.id, &o.userID, &o.amount, &o.currency, &o.dueOffset, &o.paid)
		out = append(out, o)
	}
	return out
}

func (s *simulation) actOnOpenInvoices() {
	for _, inv := range s.openInvoices() {
		c := s.custs[inv.userID]
		if c == nil || c.deleted {
			continue
		}
		owed := round2(inv.amount - inv.paid)
		if owed <= 0 {
			continue
		}
		if _, ok := c.lateBy[inv.id]; !ok {
			c.lateBy[inv.id] = s.rng.Intn(6)
		}
		late := c.lateBy[inv.id]
		switch c.profile {
		case "ontime", "flaky", "free":
			if inv.dueOffset <= s.rng.Intn(3) {
				s.pay(c, inv, owed)
			}
		case "postpaid":
			if inv.dueOffset <= -late {
				s.pay(c, inv, owed)
			}
		case "late":
			if inv.dueOffset <= -(1 + late) {
				s.pay(c, inv, owed)
			}
		case "partial":
			if !c.paidOnce[inv.id] && inv.dueOffset <= 1 {
				c.paidOnce[inv.id] = true
				half := round2(inv.amount / 2)
				if half < 0.01 {
					half = 0.01
				}
				if half >= owed {
					half = owed
				}
				s.pay(c, inv, half)
			} else if c.paidOnce[inv.id] && inv.dueOffset <= -(2+late) {
				s.pay(c, inv, owed)
			}
		case "never":
			if inv.dueOffset < -60 && s.rng.Intn(10) == 0 {
				// staff give up and cancel the unpaid invoice
				if _, err := s.db.Exec(`UPDATE invoices SET status='cancelled' WHERE id=$1 AND status IN ('pending','overdue')
					AND NOT EXISTS (SELECT 1 FROM payments WHERE invoice_id=$1 AND status='confirmed')`, inv.id); err == nil {
					s.stat("invoices cancelled by staff")
				}
			}
		}
	}
}

// pay records a payment the way the API does: cash payments carry an idempotency key and are
// sometimes submitted twice; mobile-money payments carry a unique reference and are
// sometimes submitted twice.
func (s *simulation) pay(c *simCustomer, inv openInvoice, amount float64) {
	in := paymentInput{InvoiceID: &inv.id, Amount: amount, Currency: inv.currency}
	if s.rng.Intn(2) == 0 {
		s.keySeq++
		in.PaymentMethod, in.IdempotencyKey = "cash", fmt.Sprintf("sim-%d", s.keySeq)
	} else {
		s.refSeq++
		in.PaymentMethod, in.Gateway, in.GatewayRef = "mobile_money", "mpesa", fmt.Sprintf("MP%08d", s.refSeq)
	}
	out, err := payTx(s.db, in)
	if err != nil {
		s.stat("payment refused: " + shorten(err.Error()))
		return
	}
	s.money[inv.currency] += amount
	if out.InvoicePaid {
		s.stat("invoices paid")
	} else {
		s.stat("partial payments")
	}
	if s.rng.Intn(12) == 0 { // the same request arrives again
		out2, err := payTx(s.db, in)
		switch {
		case in.IdempotencyKey != "" && (err != nil || !out2.Replayed || out2.PaymentID != out.PaymentID):
			s.t.Errorf("day %d: retried payment with the same idempotency key was not recognised (err=%v)", s.day, err)
		case in.IdempotencyKey == "" && err == nil:
			s.t.Errorf("day %d: the same mobile-money reference was recorded twice", s.day)
		}
		s.stat("duplicate submissions blocked")
	}
	if c.profile == "flaky" && s.rng.Intn(100) < 25 {
		if _, err := reverseTx(s.db, out.PaymentID); err == nil {
			s.money[inv.currency] -= amount
			s.noteReopened(inv.id)
			s.stat("reversals (flaky customer)")
		}
	}
}

func (s *simulation) noteReopened(invoiceID int) {
	var status string
	s.db.QueryRow(`SELECT status FROM invoices WHERE id=$1`, invoiceID).Scan(&status)
	if status == "pending" || status == "overdue" {
		s.reopened[invoiceID] = true
	}
}

func (s *simulation) randomStaffActions(free *planRow) {
	// Reverse an old payment (after newer periods may already have been paid).
	if s.rng.Intn(100) < 40 {
		rows, _ := s.db.Query(`SELECT id, invoice_id, amount, currency FROM payments
			WHERE status='confirmed' AND invoice_id IS NOT NULL ORDER BY id DESC LIMIT 300`)
		type pr struct {
			id, inv int
			amt     float64
			cur     string
		}
		var ps []pr
		for rows != nil && rows.Next() {
			var p pr
			rows.Scan(&p.id, &p.inv, &p.amt, &p.cur)
			ps = append(ps, p)
		}
		if rows != nil {
			rows.Close()
		}
		if len(ps) > 0 {
			p := ps[s.rng.Intn(len(ps))]
			if _, err := reverseTx(s.db, p.id); err == nil {
				s.money[p.cur] -= p.amt
				s.noteReopened(p.inv)
				s.stat("reversals (staff, any age)")
			}
		}
	}
	// A customer asks for more (or fewer) grace days; staff change it on the customer's dialog.
	if len(s.order) > 0 && s.rng.Intn(100) < 15 {
		c := s.custs[s.order[s.rng.Intn(len(s.order))]]
		if c != nil && !c.deleted {
			body := map[string]interface{}{"grace_days": s.rng.Intn(11)}
			if s.rng.Intn(4) == 0 {
				body = map[string]interface{}{"clear_grace_days": true}
			}
			if code, resp := callHandler(s.h.UpdateRadiusUser, gin.Params{{Key: "id", Value: fmt.Sprint(c.id)}}, nil, body); code != 200 {
				s.t.Errorf("day %d: grace change for %s refused: %v", s.day, c.name, resp)
			}
			s.stat("grace changes by staff")
		}
	}
	// Plan switch.
	if len(s.order) > 0 && s.rng.Intn(100) < 60 {
		c := s.custs[s.order[s.rng.Intn(len(s.order))]]
		if c != nil && !c.deleted && c.profile != "free" {
			s.assign(c, s.plans[s.rng.Intn(len(s.plans))].row, false, s.maybePromo())
			s.stat("plan switches")
		}
	}
	// A plan's price changes while invoices for it are open.
	if s.rng.Intn(100) < 8 {
		p := s.plans[s.rng.Intn(len(s.plans))]
		newPrice := round2(p.row.Price * (0.8 + float64(s.rng.Intn(50))/100))
		mustExec(s.t, s.db, `UPDATE user_plans SET price=$1 WHERE id=$2`, newPrice, p.row.ID)
		p.row.Price = newPrice
		s.stat("plan price changes")
	}
	// A customer is deleted while they still have open invoices.
	if s.rng.Intn(100) < 5 && len(s.order) > 10 {
		c := s.custs[s.order[s.rng.Intn(len(s.order))]]
		if c != nil && !c.deleted {
			tx, _ := s.db.Begin()
			if err := deleteRadiusUserTx(tx, c.id, c.name); err != nil {
				tx.Rollback()
				var unsettled *unsettledInvoicesError
				if !errors.As(err, &unsettled) {
					s.t.Fatalf("delete customer: %v", err)
				}
				s.stat("deletes refused (money unsettled)")
				return
			}
			tx.Commit()
			c.deleted = true
			s.stat("customers deleted")
		}
	}
}

// ── invariants ──────────────────────────────────────────────────────────────

func (s *simulation) checkDay() {
	t := s.t
	label := func(p string) string { return fmt.Sprintf("day %d: %s", s.day, p) }
	var bad []string
	for _, p := range checkMoney(s.db) {
		bad = append(bad, label(p))
	}
	for _, p := range checkEnforcement(s.db) {
		bad = append(bad, label(p))
	}
	// With a real FreeRADIUS running (scripts/test-radius.sh), ask it about every customer,
	// deleted ones included, every SIM_RADIUS_EVERY days.
	if r := optionalRadius(); r != nil && s.day%simEnv("SIM_RADIUS_EVERY", 1) == 0 {
		pw := map[string]string{}
		for _, c := range s.custs {
			pw[c.name] = "x"
		}
		var real []string
		s.timed("real FreeRADIUS checks", func() { real = checkRealRadius(r, s.db, pw) })
		for _, p := range real {
			bad = append(bad, label(p))
		}
		s.stats["real FreeRADIUS logins checked"] += len(pw)
	}

	// Paid-until equals the starting point plus the days of every invoice that holds a period.
	rows, err := s.db.Query(`SELECT ru.id, COALESCE(ru.account_expiry - CURRENT_DATE, 0),
		COALESCE((SELECT SUM(applied_days) FROM invoices i WHERE i.user_id=ru.id AND i.service_applied),0)
		FROM radius_users ru WHERE ru.account_expiry IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, off, granted int
		rows.Scan(&id, &off, &granted)
		c := s.custs[id]
		if c == nil || c.profile == "free" {
			continue
		}
		if off != c.anchor+granted {
			bad = append(bad, label(fmt.Sprintf("LEDGER %s (%s): paid-until today%+d but baseline %+d + granted %d = today%+d",
				c.name, c.profile, off, c.anchor, granted, c.anchor+granted)))
		}
	}
	rows.Close()

	// One open plan invoice per customer (re-opened ones excepted), and every customer who is
	// due a renewal has one.
	orows, err := s.db.Query(`SELECT i.id, i.user_id, i.plan_id FROM invoices i
		WHERE i.status IN ('pending','overdue') AND i.user_id IS NOT NULL AND i.plan_id IS NOT NULL
		AND NOT i.service_applied
		AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.invoice_id=i.id AND p.status='confirmed')`)
	// (started-on-credit and part-paid invoices are debts for staff to settle, not renewals)
	if err != nil {
		t.Fatal(err)
	}
	openCount := map[int]int{}
	for orows.Next() {
		var iid, uid, pid int
		orows.Scan(&iid, &uid, &pid)
		if !s.reopened[iid] {
			openCount[uid]++
		}
	}
	orows.Close()
	for uid, n := range openCount {
		if n > 1 {
			bad = append(bad, label(fmt.Sprintf("OPEN-INVOICES %s has %d open plan invoices", s.custs[uid].name, n)))
		}
	}
	rr, err := s.db.Query(`SELECT ru.id FROM radius_users ru JOIN user_plans up ON up.id=ru.plan_id AND up.is_active AND up.price > 0
		WHERE ru.status='active' AND ru.account_expiry BETWEEN CURRENT_DATE - 1 AND CURRENT_DATE + 3
		AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.user_id=ru.id AND i.status IN ('pending','overdue'))`)
	if err != nil {
		t.Fatal(err)
	}
	for rr.Next() {
		var id int
		rr.Scan(&id)
		// Only the invoices raised by the renewal task earlier today count: customers who just
		// paid an invoice today (and so have none open) are renewed by tomorrow's run.
		if s.custs[id] != nil {
			var paidToday int
			s.db.QueryRow(`SELECT COUNT(*) FROM invoices WHERE user_id=$1 AND paid_at::date = CURRENT_DATE`, id).Scan(&paidToday)
			if paidToday == 0 {
				bad = append(bad, label(fmt.Sprintf("RENEWAL %s is due a renewal but has no open invoice", s.custs[id].name)))
			}
		}
	}
	rr.Close()

	// Money in the harness's own ledger equals the database, per currency.
	mrows, _ := s.db.Query(`SELECT currency, COALESCE(SUM(amount) FILTER (WHERE status='confirmed'),0) FROM payments GROUP BY currency`)
	got := map[string]float64{}
	for mrows != nil && mrows.Next() {
		var cur string
		var amt float64
		mrows.Scan(&cur, &amt)
		got[cur] = amt
	}
	if mrows != nil {
		mrows.Close()
	}
	for cur, want := range s.money {
		if d := got[cur] - want; d > 0.004 || d < -0.004 {
			bad = append(bad, label(fmt.Sprintf("REVENUE %s: database has %.2f, expected %.2f from the operations performed", cur, got[cur], want)))
		}
	}
	if len(bad) > 0 {
		max := len(bad)
		if max > 12 {
			max = 12
		}
		t.Errorf("%d violation(s):\n  %s", len(bad), strings.Join(bad[:max], "\n  "))
	}
}

// checkRevenueThroughApi reads the totals the Billing and Payments pages show and compares
// them with the harness's own tally, per currency.
func (s *simulation) checkRevenueThroughApi() {
	call := func(fn gin.HandlerFunc) map[string]interface{} {
		_, body := callHandler(fn, nil, nil, nil)
		return body
	}
	sum := func(v interface{}) map[string]float64 {
		out := map[string]float64{}
		list, _ := v.([]interface{})
		for _, it := range list {
			m := it.(map[string]interface{})
			out[m["currency"].(string)] = m["amount"].(float64)
		}
		return out
	}
	pay := call(s.h.PaymentSummary)
	allTime := sum(pay["all_time"])
	inv := call(s.h.ListInvoices)
	collected := map[string]float64{}
	if sm, ok := inv["summary"].(map[string]interface{}); ok {
		collected = sum(sm["collected"])
	}
	standalone := map[string]float64{}
	rows, _ := s.db.Query(`SELECT currency, SUM(amount) FROM payments WHERE status='confirmed' AND invoice_id IS NULL GROUP BY currency`)
	for rows != nil && rows.Next() {
		var c string
		var a float64
		rows.Scan(&c, &a)
		standalone[c] = a
	}
	if rows != nil {
		rows.Close()
	}
	for cur, want := range s.money {
		if d := allTime[cur] - want; d > 0.004 || d < -0.004 {
			s.t.Errorf("Payments page all-time %s = %.2f, expected %.2f", cur, allTime[cur], want)
		}
		if d := collected[cur] + standalone[cur] - allTime[cur]; d > 0.004 || d < -0.004 {
			s.t.Errorf("Billing page collected %s (%.2f) + standalone (%.2f) != Payments page %.2f", cur, collected[cur], standalone[cur], allTime[cur])
		}
	}
}
