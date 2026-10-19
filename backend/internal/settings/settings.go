// Package settings reads the editable system_settings table (Settings page / setup wizard).
//
// Lookup order for every key: the database row if it exists and parses, then the named
// environment variable (if any), then the built-in default. Values are cached for 30 seconds
// so per-request callers (session timeout, MFA requirement) do not hit the database each
// time; Invalidate() is called after the Settings page saves.
package settings

import (
	"database/sql"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ttl = 30 * time.Second

type entry struct {
	value   string
	found   bool
	fetched time.Time
}

var (
	db    *sql.DB
	mu    sync.Mutex
	cache = map[string]entry{}
)

// Init gives the package its database handle. Before Init, lookups use env/defaults only.
func Init(d *sql.DB) {
	mu.Lock()
	db = d
	cache = map[string]entry{}
	mu.Unlock()
}

// Invalidate drops cached values so the next lookup re-reads the table.
func Invalidate() {
	mu.Lock()
	cache = map[string]entry{}
	mu.Unlock()
}

func lookup(key string) (string, bool) {
	mu.Lock()
	e, ok := cache[key]
	d := db
	mu.Unlock()
	if ok && time.Since(e.fetched) < ttl {
		return e.value, e.found
	}
	if d == nil {
		return "", false
	}
	var v sql.NullString
	err := d.QueryRow(`SELECT value FROM system_settings WHERE key = $1`, key).Scan(&v)
	e = entry{value: strings.TrimSpace(v.String), found: err == nil && v.Valid, fetched: time.Now()}
	mu.Lock()
	cache[key] = e
	mu.Unlock()
	return e.value, e.found
}

// String returns the setting, or env var envName (may be ""), or def.
func String(key, envName, def string) string {
	if v, ok := lookup(key); ok && v != "" {
		return v
	}
	if envName != "" {
		if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
			return v
		}
	}
	return def
}

// Int is String parsed as an integer; unparsable values fall through to the next source.
func Int(key, envName string, def int) int {
	if v, ok := lookup(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if envName != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(envName))); err == nil {
			return n
		}
	}
	return def
}

// Bool accepts true/false/1/0/yes/no.
func Bool(key, envName string, def bool) bool {
	parse := func(s string) (bool, bool) {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
		return false, false
	}
	if v, ok := lookup(key); ok {
		if b, ok := parse(v); ok {
			return b
		}
	}
	if envName != "" {
		if b, ok := parse(os.Getenv(envName)); ok {
			return b
		}
	}
	return def
}
