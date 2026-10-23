package database

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// DB wraps the standard database/sql DB with helper methods.
type DB struct {
	*sql.DB
}

// New creates and configures a PostgreSQL connection pool.
func New() (*DB, error) {
	host := getEnv("DB_HOST", "localhost")
	port := getEnv("DB_PORT", "5432")
	user := getEnv("DB_USER", "radius_user")
	password := getEnv("DB_PASSWORD", "")
	dbname := getEnv("DB_NAME", "radius")
	sslMode := getEnv("DB_SSL_MODE", "disable")

	// Values are quoted so a password containing spaces or quotes still parses.
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		dsnQuote(host), dsnQuote(port), dsnQuote(user), dsnQuote(password), dsnQuote(dbname), dsnQuote(sslMode),
	)
	// Billing dates ("today", paid-until) are worked out by PostgreSQL (CURRENT_DATE), and
	// FreeRADIUS cuts a customer off at midnight of its own local time. Both must use the same
	// zone whatever the database server's default is: managed databases (Supabase, RDS) run in
	// UTC, so in Kenya a payment between midnight and 3 a.m. would start from yesterday and
	// expiry would run 3 hours late. The session zone is set from DB_TIMEZONE, else PGTZ, else TZ.
	if tz := sessionTimezone(); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return nil, fmt.Errorf("timezone %q (DB_TIMEZONE/PGTZ/TZ) is not a valid zone name: %w", tz, err)
		}
		dsn += " timezone=" + dsnQuote(tz)
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// Connection pool settings
	maxOpen, _ := strconv.Atoi(getEnv("DB_MAX_OPEN_CONNS", "25"))
	maxIdle, _ := strconv.Atoi(getEnv("DB_MAX_IDLE_CONNS", "5"))
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(2 * time.Minute)

	// Verify connectivity with retry (up to ~20s). This covers the backend starting before
	// Postgres accepts connections; docker-compose also waits on Postgres's healthcheck.
	// There is no migration step here: the schema must already exist (init-db.sql plus
	// migrations/*.sql, applied by compose on first start or by deploy-update.sh).
	for i := 0; i < 10; i++ {
		if err = db.Ping(); err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	return &DB{db}, nil
}

func sessionTimezone() string {
	for _, k := range []string{"DB_TIMEZONE", "PGTZ", "TZ"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return strings.TrimPrefix(v, ":")
		}
	}
	return ""
}

// ClockMismatch reports, in words, if the database's clock zone differs from this process's
// (whose zone is what FreeRADIUS uses when both follow TZ). Empty when they agree.
func (db *DB) ClockMismatch() string {
	var zone, today string
	var offset float64
	if err := db.QueryRow(`SELECT current_setting('TimeZone'), CURRENT_DATE::text, EXTRACT(TIMEZONE FROM now())`).
		Scan(&zone, &today, &offset); err != nil {
		return "could not read the database clock: " + err.Error()
	}
	now := time.Now()
	_, local := now.Zone()
	if int(offset) == local && today == now.Format("2006-01-02") {
		return ""
	}
	return fmt.Sprintf("the database works in zone %s (UTC%+.1fh, today %s) but this server in %s (UTC%+.1fh, today %s): "+
		"customers would be cut off and renewed at the wrong hour. Set TZ (or DB_TIMEZONE) to your local zone, e.g. TZ=Africa/Nairobi",
		zone, offset/3600, today, now.Location(), float64(local)/3600, now.Format("2006-01-02"))
}

// dsnQuote quotes a libpq key=value connection-string value.
func dsnQuote(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
