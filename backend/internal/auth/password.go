package auth

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/freeradius-manager/backend/internal/settings"
	"golang.org/x/crypto/bcrypt"
)

var (
	errPasswordTooShort    = "password must be at least %d characters"
	ErrPasswordNoUpper     = errors.New("password must contain at least one uppercase letter")
	ErrPasswordNoLower     = errors.New("password must contain at least one lowercase letter")
	ErrPasswordNoDigit     = errors.New("password must contain at least one digit")
	ErrPasswordNoSpecial   = errors.New("password must contain at least one special character")
	ErrPasswordReused      = errors.New("password was recently used, choose a different one")
	// ErrPasswordTooLong: bcrypt takes at most 72 bytes (fewer characters if they are not
	// plain ASCII) and refuses longer input instead of hashing it.
	ErrPasswordTooLong = errors.New("password is too long (at most 72 bytes; non-English letters count as 2-4)")
)

// HashPassword creates a bcrypt hash of the password.
// Used for admin logins and the customer self-service portal. It does NOT protect
// RADIUS network passwords: FreeRADIUS needs the cleartext (PAP/CHAP/MS-CHAP), so
// handlers also write the plain password to radcheck (Cleartext-Password).
func HashPassword(password string) (string, error) {
	if len(password) > 72 {
		return "", ErrPasswordTooLong
	}
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
// 6 characters (models.CreateRadiusUserRequest).
func ValidatePasswordComplexity(password string) error {
	return ValidatePasswordWithMin(password, MinPasswordLength())
}

// ValidatePasswordWithMin is ValidatePasswordComplexity with an explicit minimum length
// (used by the setup wizard, which chooses the policy it is about to save).
func ValidatePasswordWithMin(password string, minLen int) error {
	if minLen < 8 {
		minLen = 8
	}
	if utf8.RuneCountInString(password) < minLen {
		return fmt.Errorf(errPasswordTooShort, minLen)
	}
	if len(password) > 72 {
		return ErrPasswordTooLong
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

// MinPasswordLength is the staff password minimum: Settings page "password_min_length",
// else PASSWORD_MIN_LENGTH, else 12. Never below 8.
func MinPasswordLength() int {
	l := settings.Int("password_min_length", "PASSWORD_MIN_LENGTH", 12)
	if l < 8 {
		return 8
	}
	return l
}
