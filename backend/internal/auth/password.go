package auth

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrPasswordTooShort    = errors.New("password must be at least 12 characters")
	ErrPasswordNoUpper     = errors.New("password must contain at least one uppercase letter")
	ErrPasswordNoLower     = errors.New("password must contain at least one lowercase letter")
	ErrPasswordNoDigit     = errors.New("password must contain at least one digit")
	ErrPasswordNoSpecial   = errors.New("password must contain at least one special character")
	ErrPasswordReused      = errors.New("password was recently used, choose a different one")
)

// HashPassword creates a bcrypt hash of the password.
// Used for admin logins and the customer self-service portal. It does NOT protect
// RADIUS network passwords: FreeRADIUS needs the cleartext (PAP/CHAP/MS-CHAP), so
// handlers also write the plain password to radcheck (Cleartext-Password).
func HashPassword(password string) (string, error) {
	cost := getBcryptCost()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword compares a bcrypt hash with a plaintext password.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// ValidatePasswordComplexity checks that a password meets all complexity rules.
// Applies to admin/operator accounts only; RADIUS (customer) passwords only need
// 6 characters (models.CreateRadiusUserRequest). The length check counts bytes, not
// characters, so a short non-ASCII password can pass.
func ValidatePasswordComplexity(password string) error {
	minLen := getMinPasswordLength()
	if len(password) < minLen {
		return ErrPasswordTooShort
	}

	var hasUpper, hasLower, hasDigit, hasSpecial bool
	specialChars := regexp.MustCompile(`[!@#$%^&*()_+\-=\[\]{};':"\\|,.<>\/?]`)

	for _, c := range password {
		switch {
		case unicode.IsUpper(c):
			hasUpper = true
		case unicode.IsLower(c):
			hasLower = true
		case unicode.IsDigit(c):
			hasDigit = true
		}
	}
	if specialChars.MatchString(password) {
		hasSpecial = true
	}

	if !hasUpper {
		return ErrPasswordNoUpper
	}
	if !hasLower {
		return ErrPasswordNoLower
	}
	if !hasDigit {
		return ErrPasswordNoDigit
	}
	if !hasSpecial {
		return ErrPasswordNoSpecial
	}
	return nil
}

// CheckPasswordHistory verifies a new password hasn't been used recently.
// previousHashes is a slice of bcrypt hashes from password history.
func CheckPasswordHistory(newPassword string, previousHashes []string) error {
	for _, hash := range previousHashes {
		if CheckPassword(hash, newPassword) {
			return ErrPasswordReused
		}
	}
	return nil
}

func getBcryptCost() int {
	raw := os.Getenv("BCRYPT_COST")
	if raw == "" {
		return 12
	}
	cost, err := strconv.Atoi(raw)
	if err != nil || cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return 12
	}
	return cost
}

func getMinPasswordLength() int {
	raw := os.Getenv("PASSWORD_MIN_LENGTH")
	if raw == "" {
		return 12
	}
	l, err := strconv.Atoi(raw)
	if err != nil || l < 8 {
		return 12
	}
	return l
}
