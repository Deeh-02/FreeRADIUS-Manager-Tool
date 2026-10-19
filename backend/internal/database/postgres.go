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
