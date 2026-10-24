package middleware

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"

	"github.com/freeradius-manager/backend/internal/database"
	_ "github.com/lib/pq"
)

// API keys: expiry is decided by the database's clock (the column has no zone; Go read it as
// UTC, 3 hours late in Kenya), and a key never does more than its creator may now.
func TestAPIKeyExpiryAndCreatorRole(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	raw, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	db := &database.DB{DB: raw}
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`DELETE FROM api_keys WHERE name = 'apikey-test'`)
	exec(`DELETE FROM app_users WHERE username = 'apikey-owner'`)
	var uid int
	if err := db.QueryRow(`INSERT INTO app_users (username, password_hash, email, role)
		VALUES ('apikey-owner', 'x', 'apikey-owner@example.com', 'super_admin') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	defer exec(`DELETE FROM app_users WHERE id = $1`, uid)
	key := "test-key-0123456789"
	sum := sha256.Sum256([]byte(key))
	exec(`INSERT INTO api_keys (name, key_hash, key_prefix, permissions, created_by, expires_at)
		VALUES ('apikey-test', $1, 'test', '{read,write}', $2, NOW() + INTERVAL '1 minute')`, hex.EncodeToString(sum[:]), uid)
	defer exec(`DELETE FROM api_keys WHERE name = 'apikey-test'`)

	c, readOnly, err := apiKeyClaims(db, key)
	if err != nil || readOnly || c.Role != "admin" {
		t.Fatalf("write key of a super admin: role %v readOnly %v err %v, want admin", c, readOnly, err)
	}
	exec(`UPDATE app_users SET role = 'operator' WHERE id = $1`, uid)
	if c, readOnly, err = apiKeyClaims(db, key); err != nil || !readOnly || c.Role != "operator" {
		t.Errorf("write key after its creator became an operator: role %v readOnly %v err %v, want read-only operator", c, readOnly, err)
	}
	exec(`UPDATE api_keys SET expires_at = NOW() - INTERVAL '1 minute' WHERE name = 'apikey-test'`)
	if _, _, err = apiKeyClaims(db, key); err != errKeyExpired {
		t.Errorf("key that expired a minute ago: %v, want expired", err)
	}
}
