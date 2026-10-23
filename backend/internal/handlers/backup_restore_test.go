package handlers

import (
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeradius-manager/backend/internal/database"
)

// A.4 A backup is only as good as a restore. Build books with every kind of history (paid,
// part-paid, reversed, trial, grace, deleted, usage), take a backup through the API, download
// it, restore it the way docs/ARCHITECTURE.md says, and check that nothing changed: every row
// of the billing and RADIUS tables, the invariants, and what FreeRADIUS decides.

// booksFingerprint is a digest of every table that holds money or access.
func booksFingerprint(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tbl := range []string{"payments", "invoices", "radius_users", "radcheck", "radreply", "user_plans",
		"promotions", "radacct", "system_settings", "app_users", "scheduled_tasks"} {
		var n int
		var sum *string
		if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*), md5(string_agg(t::text, '|' ORDER BY t::text)) FROM %s t`, tbl)).Scan(&n, &sum); err != nil {
			t.Fatalf("fingerprint %s: %v", tbl, err)
		}
		s := ""
		if sum != nil {
			s = *sum
		}
		out[tbl] = fmt.Sprintf("%d rows %s", n, s)
	}
	for _, seq := range []string{"invoice_number_seq", "receipt_number_seq"} {
		var v int64
		db.QueryRow(`SELECT last_value FROM ` + seq).Scan(&v)
		out[seq] = fmt.Sprint(v)
	}
	return out
}

func compareFingerprints(t *testing.T, label string, want, got map[string]string) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %s differs: %s, want %s", label, k, got[k], w)
		}
	}
}

// buildHistory gives the books some of everything, through the same code paths staff use.
func buildHistory(t *testing.T, a *stackAPI, db *database.DB, customers int) map[string]string {
	t.Helper()
	rng := rand.New(rand.NewSource(7))
	plan := a.must("POST", "/plans", map[string]interface{}{"name": "Home", "price": 1500, "currency": "KES", "validity_days": 30}, 201)
	planID := int(plan["id"].(float64))
	a.must("POST", "/promotions", map[string]interface{}{"code": "KARIBU", "discount_type": "percent", "discount_value": 20, "max_uses": 0}, 201)
	pw := map[string]string{}
	for i := 0; i < customers; i++ {
		name := fmt.Sprintf("hist%03d", i)
		pw[name] = "pw-" + name
		body := map[string]interface{}{"username": name, "password": pw[name], "plan_id": planID, "trial_days": rng.Intn(3) * 3}
		if i%7 == 0 {
			body["promo_code"] = "KARIBU"
		}
		if i%11 == 0 {
			body["grace_days"] = 2
		}
		out := a.must("POST", "/radius/users", body, 201)
		uid := int(out["id"].(float64))
		var inv int
		var amount float64
		if db.QueryRow(`SELECT id, amount FROM invoices WHERE user_id=$1 AND status='pending'`, uid).Scan(&inv, &amount) != nil {
			continue
		}
		switch rng.Intn(5) {
		case 0, 1: // paid in full
			a.must("POST", "/payments", map[string]interface{}{"invoice_id": inv, "amount": amount, "payment_method": "mobile_money",
				"gateway": "mpesa", "gateway_ref": fmt.Sprintf("HIST%06d", i)}, 201)
		case 2: // part-paid
			a.must("POST", "/payments", map[string]interface{}{"invoice_id": inv, "amount": 500, "payment_method": "cash"}, 201)
		case 3: // paid, then reversed
			out := a.must("POST", "/payments", map[string]interface{}{"invoice_id": inv, "amount": amount}, 201)
			a.must("POST", fmt.Sprintf("/payments/%d/reverse", int(out["id"].(float64))), map[string]string{"reason": "test"}, 200)
		}
		mustExec(t, db, `INSERT INTO radacct (acctsessionid, acctuniqueid, username, nasipaddress, acctstarttime, acctstoptime,
			acctsessiontime, acctinputoctets, acctoutputoctets) VALUES ($1, $2, $3, '10.1.1.1', NOW() - INTERVAL '2 hours', NOW() - INTERVAL '1 hour', 3600, $4, $5)`,
			"h-"+name, "hu-"+name, name, rng.Intn(1e9), rng.Intn(1e9))
	}
	// A deleted customer (the first one nothing unsettled stands in the way of; the others are
	// refused, which must leave no trace) and a cash sale with no invoice.
	deleted := false
	for i := 0; i < customers && !deleted; i++ {
		var uid int
		if db.QueryRow(`SELECT id FROM radius_users WHERE username=$1`, fmt.Sprintf("hist%03d", i)).Scan(&uid) != nil {
			continue
		}
		code, _, _ := a.do("DELETE", fmt.Sprintf("/radius/users/%d", uid), nil, nil)
		deleted = code == 200
	}
	if !deleted {
		t.Fatal("no customer could be deleted")
	}
	a.must("POST", "/payments", map[string]interface{}{"username": "hist001", "amount": 200, "payment_method": "cash", "notes": "router fee"}, 201)
	return pw
}

func TestStackBackupRestore(t *testing.T) {
	a, db := needStack(t)
	r := needRadius(t)
	pw := buildHistory(t, a, db, simEnv("RESTORE_CUSTOMERS", 120))
	failIf(t, checkMoney(db))
	failIf(t, checkEnforcement(db))
	before := booksFingerprint(t, db.DB)
	accessBefore, _ := expectedAccess(db)

	// Back up through the API, as the Backups page does, and download the file.
	existing := map[string]bool{}
	if items, ok := a.must("GET", "/backups", nil, 200)["backups"].([]interface{}); ok {
		for _, it := range items {
			existing[it.(map[string]interface{})["name"].(string)] = true
		}
	}
	a.must("POST", "/backup", nil, 202)
	var name string
	took, ok := waitFor(60*time.Second, func() bool {
		list := a.must("GET", "/backups", nil, 200)
		if e, _ := list["last_error"].(string); e != "" {
			t.Fatalf("backup failed: %s", e)
		}
		items, _ := list["backups"].([]interface{})
		for _, it := range items {
			if n := it.(map[string]interface{})["name"].(string); !existing[n] && list["running"] == false {
				name = n
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatal("backup did not appear within 60s")
	}
	t.Logf("backup took about %s", took.Round(100*time.Millisecond))
	req, _ := http.NewRequest("GET", a.base+"/backups/"+name, nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.http.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("download %s: %v %v", name, err, resp)
	}
	file := filepath.Join(t.TempDir(), name)
	f, _ := os.Create(file)
	size, _ := io.Copy(f, resp.Body)
	resp.Body.Close()
	f.Close()
	t.Logf("backup %s: %d bytes", name, size)

	restore := func(dbname string) time.Duration {
		t.Helper()
		in, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		gz, err := gzip.NewReader(in)
		if err != nil {
			t.Fatalf("backup is not valid gzip: %v", err)
		}
		cmd := exec.Command("psql", "-h", "127.0.0.1", "-p", testPGPort(), "-U", "postgres", "-d", dbname,
			"-v", "ON_ERROR_STOP=1", "-q")
		cmd.Stdin = gz
		t0 := time.Now()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restore into %s failed: %v\n%s", dbname, err, tail(string(out), 2000))
		}
		return time.Since(t0)
	}

	// 1. Into an empty scratch database (a new server after losing the old one).
	mustExec(t, db, `DROP DATABASE IF EXISTS radius_restore_test`)
	mustExec(t, db, `CREATE DATABASE radius_restore_test`)
	defer db.Exec(`DROP DATABASE IF EXISTS radius_restore_test`)
	took = restore("radius_restore_test")
	t.Logf("restored into an empty database in %s", took.Round(time.Millisecond))
	scratch, err := sql.Open("postgres", strings.Replace(os.Getenv("TEST_DATABASE_URL"), "dbname=radius_test", "dbname=radius_restore_test", 1))
	if err != nil {
		t.Fatal(err)
	}
	compareFingerprints(t, "fresh restore", before, booksFingerprint(t, scratch))
	sdb := &database.DB{DB: scratch}
	failIf(t, checkMoney(sdb))
	failIf(t, checkEnforcement(sdb))
	scratch.Close()

	// 2. Over the live database after a disaster (someone wiped the payments), following the
	// documented steps: stop backend and FreeRADIUS, restore, start them.
	mustExec(t, db, `TRUNCATE payments`)
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE - 100`)
	stackCtl(t, "kill-backend")
	stackCtl(t, "radius-down")
	db.DB.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = 'radius_test' AND pid <> pg_backend_pid()`)
	took = restore("radius_test")
	t.Logf("restored over the live database in %s", took.Round(time.Millisecond))
	stackCtl(t, "start-backend")
	stackCtl(t, "radius-up")
	db.DB.Ping()
	compareFingerprints(t, "restore over live", before, booksFingerprint(t, db.DB))
	failIf(t, checkMoney(db))
	failIf(t, checkEnforcement(db))
	accessAfter, _ := expectedAccess(db)
	for u, f := range accessBefore {
		if g := accessAfter[u]; g == nil || g.want != f.want {
			t.Errorf("%s: allowed=%v before the backup, %v after the restore", u, f.want, g != nil && g.want)
		}
	}
	failIf(t, checkRealRadius(r, db, pw))
	// Staff sessions survive (they are in the backup); the system takes new payments.
	a.must("GET", "/payments", nil, 200)
	inv := scanInt(t, db, `SELECT id FROM invoices WHERE status='pending' ORDER BY id LIMIT 1`)
	amount := scanFloat(t, db, `SELECT amount - COALESCE((SELECT SUM(amount) FROM payments WHERE invoice_id=$1 AND status='confirmed'),0) FROM invoices WHERE id=$1`, inv)
	a.must("POST", "/payments", map[string]interface{}{"invoice_id": inv, "amount": amount}, 201)
	failIf(t, checkMoney(db))
}

func testPGPort() string {
	for _, part := range strings.Fields(os.Getenv("TEST_DATABASE_URL")) {
		if strings.HasPrefix(part, "port=") {
			return strings.TrimPrefix(part, "port=")
		}
	}
	return "5432"
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// A managed database (Supabase, RDS): the system logs in as a role that does not own the
// database or the public schema, and the provider keeps schemas of its own that this role
// cannot read. A full dump fails there; BACKUP_SCHEMA=public must dump and restore as that role.
func TestBackupManagedDatabase(t *testing.T) {
	db := openTestDB(t)
	port := testPGPort()
	for _, q := range []string{
		`DROP DATABASE IF EXISTS radius_managed_test`, `DROP DATABASE IF EXISTS radius_managed2_test`,
		`DROP ROLE IF EXISTS managed_app`,
		`CREATE ROLE managed_app LOGIN PASSWORD 'managed-pass'`,
		`CREATE DATABASE radius_managed_test`, `CREATE DATABASE radius_managed2_test`,
	} {
		mustExec(t, db, q)
	}
	defer func() {
		db.Exec(`DROP DATABASE IF EXISTS radius_managed_test`)
		db.Exec(`DROP DATABASE IF EXISTS radius_managed2_test`)
		db.Exec(`DROP ROLE IF EXISTS managed_app`)
	}()
	super := func(dbname string, sqls ...string) {
		for _, s := range sqls {
			if out, err := exec.Command("psql", "-h", "127.0.0.1", "-p", port, "-U", "postgres", "-d", dbname, "-v", "ON_ERROR_STOP=1", "-qc", s).CombinedOutput(); err != nil {
				t.Fatalf("%s: %v %s", s, err, out)
			}
		}
	}
	for _, d := range []string{"radius_managed_test", "radius_managed2_test"} {
		super(d, `GRANT ALL ON SCHEMA public TO managed_app`,
			`CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`, `CREATE EXTENSION IF NOT EXISTS pgcrypto`, // the provider installs these
			`CREATE SCHEMA auth`, `CREATE TABLE auth.users (id int)`) // the provider's own, unreadable to us
	}
	url := func(d string) string { return "postgresql://managed_app:managed-pass@127.0.0.1:" + port + "/" + d + "?sslmode=disable" }
	setup := exec.Command("../../../scripts/db-setup.sh")
	setup.Env = append(os.Environ(), "DATABASE_URL="+url("radius_managed_test"), "TZ=Africa/Nairobi")
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("db-setup.sh as a non-owner: %v\n%s", err, out)
	}
	mdb, err := sql.Open("postgres", url("radius_managed_test"))
	if err != nil {
		t.Fatal(err)
	}
	defer mdb.Close()
	for _, q := range []string{
		`INSERT INTO radius_users (username, password, account_expiry) VALUES ('mteja','x', CURRENT_DATE + 10)`,
		`INSERT INTO radcheck (username, attribute, op, value) VALUES ('mteja','Cleartext-Password',':=','pw')`,
		`INSERT INTO payments (receipt_number, username, amount, currency, status) VALUES ('RCP-1','mteja',1500,'KES','confirmed')`,
	} {
		if _, err := mdb.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := booksFingerprint(t, mdb)

	t.Setenv("DB_HOST", "127.0.0.1")
	t.Setenv("DB_PORT", port)
	t.Setenv("DB_USER", "managed_app")
	t.Setenv("DB_PASSWORD", "managed-pass")
	t.Setenv("DB_NAME", "radius_managed_test")
	t.Setenv("BACKUP_DIR", t.TempDir())
	t.Setenv("BACKUP_SCHEMA", "")
	if _, err := dumpDatabase(); err == nil {
		t.Log("note: a full dump worked here; on Supabase it does not")
	} else {
		t.Logf("full dump as a non-owner fails, as expected: %.120s", err.Error())
	}
	t.Setenv("BACKUP_SCHEMA", "public")
	name, err := dumpDatabase()
	if err != nil {
		t.Fatalf("BACKUP_SCHEMA=public dump: %v", err)
	}
	file := filepath.Join(os.Getenv("BACKUP_DIR"), name)
	restoreAs := func(dbname string) {
		in, _ := os.Open(file)
		defer in.Close()
		gz, _ := gzip.NewReader(in)
		cmd := exec.Command("psql", url(dbname), "-v", "ON_ERROR_STOP=1", "-q")
		cmd.Stdin = gz
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restore into %s as a non-owner: %v\n%s", dbname, err, tail(string(out), 1500))
		}
	}
	// Over the live database, after damage.
	mdb.Exec(`DELETE FROM payments`)
	restoreAs("radius_managed_test")
	compareFingerprints(t, "managed restore over live", before, booksFingerprint(t, mdb))
	// Into a fresh database of the same kind.
	restoreAs("radius_managed2_test")
	m2, _ := sql.Open("postgres", url("radius_managed2_test"))
	defer m2.Close()
	compareFingerprints(t, "managed restore into fresh", before, booksFingerprint(t, m2))
	var tz string
	mdb.QueryRow(`SHOW timezone`).Scan(&tz)
	if tz != "Africa/Nairobi" {
		t.Errorf("db-setup.sh left the database timezone at %s", tz)
	}
}
