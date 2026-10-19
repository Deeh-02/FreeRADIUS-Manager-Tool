package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims contains JWT payload fields.
type Claims struct {
	UserID    int    `json:"user_id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	SessionID string `json:"session_id"`
	jwt.RegisteredClaims
}

var (
	ErrTokenExpired   = errors.New("token expired")
	ErrTokenInvalid   = errors.New("token invalid")
	ErrTokenMalformed = errors.New("token malformed")
)

// GenerateAccessToken creates a short-lived JWT for API access.
func GenerateAccessToken(userID int, username, role, sessionID string) (string, error) {
	secret := getJWTSecret()
	expiry := getAccessExpiry()

	claims := Claims{
		UserID:    userID,
		Username:  username,
		Role:      role,
		SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "radius-manager",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// GenerateRefreshToken creates an opaque refresh token and its hash for storage.
// Only the SHA-256 hash is stored (refresh_tokens.token_hash), so a database leak does
// not hand out usable refresh tokens. A fast hash is fine here: the token is 256 bits
// of randomness, unlike a human password.
func GenerateRefreshToken() (token string, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	token = hex.EncodeToString(b)
	h := sha256.Sum256([]byte(token))
	hash = hex.EncodeToString(h[:])
	return
}

// HashRefreshToken returns the SHA-256 hash of a refresh token.
func HashRefreshToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// ValidateAccessToken parses and validates a JWT, returning the claims.
func ValidateAccessToken(tokenString string) (*Claims, error) {
	secret := getJWTSecret()

	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrTokenMalformed
		}
		return []byte(secret), nil
	})

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrTokenInvalid
	}

	return claims, nil
}

// RefreshExpiry returns the refresh token duration.
func RefreshExpiry() time.Duration {
	raw := os.Getenv("JWT_REFRESH_EXPIRY")
	if raw == "" {
		return 168 * time.Hour
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 168 * time.Hour
	}
	return d
}

// knownWeakSecrets are values that have shipped in this repository's examples and defaults.
// Anyone who knows the signing key can forge an admin token, so these are refused.
var knownWeakSecrets = map[string]bool{
	"change-this-in-production-very-insecure":                        true,
	"change-this-super-secret-jwt-key-in-production":                 true,
	"CHANGE_ME_GENERATE_WITH_openssl_rand_base64_48":                 true,
	"your-super-secret-jwt-key-change-in-production":                 true,
	"change-this-to-a-very-long-random-secret-key-at-least-64-chars": true,
	"changeme": true,
	"secret":   true,
}

// CheckJWTSecret reports why JWT_SECRET is unusable, or nil. main() refuses to start on an
// error unless APP_ENV=development.
func CheckJWTSecret() error {
	s := os.Getenv("JWT_SECRET")
	switch {
	case s == "":
		return errors.New("JWT_SECRET is not set")
	case knownWeakSecrets[s], strings.HasPrefix(strings.ToUpper(s), "CHANGE_ME"):
		return errors.New("JWT_SECRET is a published example value")
	case len(s) < 32:
		return errors.New("JWT_SECRET must be at least 32 characters")
	}
	return nil
}

// getJWTSecret returns JWT_SECRET. In development only (CheckJWTSecret is skipped there) an
// unset secret falls back to a per-process random key, so tokens die on restart instead of
// being forgeable.
func getJWTSecret() string {
	if s := os.Getenv("JWT_SECRET"); s != "" {
		return s
	}
	devSecretOnce.Do(func() {
		b := make([]byte, 32)
		rand.Read(b)
		devSecret = hex.EncodeToString(b)
	})
	return devSecret
}

var (
	devSecret     string
	devSecretOnce sync.Once
)

// AccessExpiry returns the access token duration.
func AccessExpiry() time.Duration {
	return getAccessExpiry()
}

func getAccessExpiry() time.Duration {
	raw := os.Getenv("JWT_ACCESS_EXPIRY")
	if raw == "" {
		return time.Hour
	}
	// Support both duration strings and plain minutes
	d, err := time.ParseDuration(raw)
	if err != nil {
		mins, err2 := strconv.Atoi(raw)
		if err2 != nil {
			return time.Hour
		}
		return time.Duration(mins) * time.Minute
	}
	return d
}
