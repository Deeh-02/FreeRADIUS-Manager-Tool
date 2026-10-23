package handlers

import (
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/freeradius-manager/backend/internal/database"
	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/sirupsen/logrus"
)

// Integration tests run against a real PostgreSQL that already has init-db.sql and
// migrations/*.sql applied (scripts/test-db.sh sets one up). They are skipped when
// TEST_DATABASE_URL is not set, so a plain `go test ./...` still passes anywhere.
//
//	TEST_DATABASE_URL="host=127.0.0.1 port=54329 user=postgres dbname=radius_test sslmode=disable"
//
// The database name must end in "_test": every test wipes the billing and RADIUS tables.

var (
	testDBOnce sync.Once
	testDBConn *database.DB
	testDBErr  error
)

func openTestDB(t testing.TB) *database.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database integration test")
	}
	if !strings.Contains(url, "_test") {
		t.Fatalf("TEST_DATABASE_URL must point at a database whose name ends in _test (tests delete data)")
	}
	testDBOnce.Do(func() {
		var raw *sql.DB
		raw, testDBErr = sql.Open("postgres", url)
		if testDBErr != nil {
			return
		}
		raw.SetMaxOpenConns(80)
		raw.SetMaxIdleConns(20)
		testDBErr = raw.Ping()
		testDBConn = &database.DB{DB: raw}
	})
	if testDBErr != nil {
		t.Fatalf("cannot open test database: %v", testDBErr)
	}
	return testDBConn
}

// resetTestDB empties every table the billing code touches and restarts the numbering.
func resetTestDB(t testing.TB) *database.DB {
	t.Helper()
	db := openTestDB(t)
	_, err := db.Exec(`TRUNCATE payments, invoices, promotions, user_plans, radius_users, radcheck,
		radreply, radusergroup, radacct, webhooks, system_settings, app_users, scheduled_tasks
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	for _, seq := range []string{"invoice_number_seq", "receipt_number_seq"} {
		if _, err := db.Exec(`ALTER SEQUENCE ` + seq + ` RESTART WITH 1`); err != nil {
			t.Fatalf("reset %s: %v", seq, err)
		}
	}
	mustExec(t, db, `INSERT INTO scheduled_tasks (name, task_type, schedule) VALUES
		('Expire','expire_accounts','daily'), ('Overdue','mark_overdue_invoices','daily'),
		('Renew','renew_plans','daily')`)
	mustExec(t, db, `INSERT INTO app_users (username, password_hash, email, role)
		VALUES ('staff','x','staff@example.com','admin')`)
	settings.Init(db.DB)
	return db
}

func testLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(os.Stderr)
	l.SetLevel(logrus.ErrorLevel)
	return l
}

func mustExec(t testing.TB, q dbtx, query string, args ...interface{}) {
	t.Helper()
	if _, err := q.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", firstLine(query), err)
	}
}

func scanInt(t testing.TB, q dbtx, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := q.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", firstLine(query), err)
	}
	return n
}

func scanFloat(t testing.TB, q dbtx, query string, args ...interface{}) float64 {
	t.Helper()
	var n float64
	if err := q.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", firstLine(query), err)
	}
	return n
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func seedPlan(t testing.TB, q dbtx, name string, price float64, days int) *planRow {
	t.Helper()
	var id int
	if err := q.QueryRow(`INSERT INTO user_plans (name, price, currency, validity_days, max_devices)
		VALUES ($1,$2,'USD',$3,1) RETURNING id`, name, price, days).Scan(&id); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	p, err := loadPlan(q, id)
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	return p
}

// seedUser creates a customer whose paid-until date is today+offset days (nil = never paid).
func seedUser(t testing.TB, q dbtx, name string, expiryOffset *int) int {
	t.Helper()
	var id int
	var err error
	if expiryOffset == nil {
		err = q.QueryRow(`INSERT INTO radius_users (username, password) VALUES ($1,'x') RETURNING id`, name).Scan(&id)
	} else {
		err = q.QueryRow(`INSERT INTO radius_users (username, password, account_expiry)
			VALUES ($1,'x', CURRENT_DATE + $2::int) RETURNING id`, name, *expiryOffset).Scan(&id)
	}
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := syncAccess(q, name); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return id
}

func intPtr(n int) *int { return &n }

// seedInvoice raises a plain open invoice (no plan unless p is given).
func seedInvoice(t testing.TB, q dbtx, userID int, username string, p *planRow, amount float64) int {
	t.Helper()
	in := newInvoice{UserID: &userID, Username: username, Amount: amount, Currency: "USD"}
	if p != nil {
		in.PlanID, in.PlanName = &p.ID, &p.Name
	}
	inv, err := createInvoiceTx(q, in)
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	return inv.ID
}

// parallel runs fn(i) for i in [0,n) and releases every goroutine at the same instant.
func parallel(n int, fn func(i int)) {
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			fn(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
}

// payTx records a payment in its own transaction, as the HTTP handler does.
func payTx(db *database.DB, in paymentInput) (*paymentOutcome, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out, err := recordPaymentTx(tx, in)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func reverseTx(db *database.DB, paymentID int) (string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	msg, err := reversePaymentTx(tx, paymentID, nil, "test")
	if err != nil {
		return "", err
	}
	return msg, tx.Commit()
}

func assignTx(db *database.DB, userID int, username string, planID int, startNow bool, promo string) (*assignResult, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := assignPlanTx(tx, userID, username, planID, startNow, promo, nil, nil)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit()
}
