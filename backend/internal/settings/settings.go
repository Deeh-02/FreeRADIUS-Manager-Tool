package settings

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Package settings reads the editable system_settings table (Settings page / setup wizard).
//
// Lookup order for every key: the database row if it exists and parses, then the named
// environment variable (if any), then the built-in default.
//
// The whole table is held in memory and refreshed in the background every few seconds, so a
// lookup never touches the database. That matters: billing code calls settings while it
// already holds a database connection inside a transaction, and a lookup that needed a
// second connection could leave every pooled connection waiting for another one (a deadlock
// under load). Invalidate() reloads at once after the Settings page saves.

const refreshEvery = 15 * time.Second

var (
	mu     sync.RWMutex
	values = map[string]string{}
	db     *sql.DB
	gen    int // bumped by Init so an old refresher goroutine exits
)

// Init gives the package its database handle, loads the table, and keeps it fresh. Before
// Init, lookups use env/defaults only.
func Init(d *sql.DB) {
	mu.Lock()
	db = d
	gen++
	g := gen
	values = map[string]string{}
	mu.Unlock()

	reload(d)
	go func() {
		t := time.NewTicker(refreshEvery)
		defer t.Stop()
		for range t.C {
			mu.RLock()
			current := g == gen
			mu.RUnlock()
			if !current {
				return
			}
			reload(d)
		}
	}()
}

// Invalidate reloads the table now so a just-saved setting takes effect immediately.
func Invalidate() {
	mu.RLock()
	d := db
	mu.RUnlock()
	if d != nil {
		reload(d)
	}
}

// reload replaces the in-memory copy. On a database error the previous copy is kept.
func reload(d *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := d.QueryContext(ctx, `SELECT key, value FROM system_settings`)
	if err != nil {
		return
	}
	defer rows.Close()
	fresh := map[string]string{}
	for rows.Next() {
		var k string
		var v sql.NullString
		if rows.Scan(&k, &v) == nil && v.Valid {
			fresh[k] = strings.TrimSpace(v.String)
		}
	}
	if rows.Err() != nil {
		return
	}
	mu.Lock()
	if d == db {
		values = fresh
	}
	mu.Unlock()
}

func lookup(key string) (string, bool) {
	mu.RLock()
	defer mu.RUnlock()
	v, ok := values[key]
	return v, ok
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
