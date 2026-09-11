package security

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// DefaultHashMethod is the standard hashing method used for credentials.
const DefaultHashMethod = "bcrypt"

var (
	// ErrEmptyPassword is returned when an empty password string is provided.
	ErrEmptyPassword = errors.New("password cannot be empty")
	// ErrInvalidPassword is returned when password verification fails.
	ErrInvalidPassword = errors.New("invalid password")
)

// HashPassword hashes a raw password string using bcrypt.
func HashPassword(password string) (string, error) {
	if len(password) == 0 {
		return "", ErrEmptyPassword
	}
	hashedBytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash password: %w", err)
	}
	return string(hashedBytes), nil
}

// CheckPassword verifies that a raw password matches the bcrypt hash.
func CheckPassword(hashedPassword, password string) error {
	if len(password) == 0 {
		return ErrEmptyPassword
	}
	err := bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(password))
	if err != nil {
		// Treat all comparison failures (mismatch, unsupported, or malformed hash)
		// as ErrInvalidPassword to prevent account enumeration and avoid 500 errors.
		return ErrInvalidPassword
	}
	return nil
}
