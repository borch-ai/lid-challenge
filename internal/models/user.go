package models

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Address represents a structured postal address.
type Address struct {
	StreetAddress string `json:"street_address"`
	Locality      string `json:"locality"`
	Region        string `json:"region"`
	PostalCode    string `json:"postal_code"`
	Country       string `json:"country"`
}

// String returns a single-line representation of the address.
func (a Address) String() string {
	parts := []string{}
	if a.StreetAddress != "" {
		parts = append(parts, a.StreetAddress)
	}
	if a.Locality != "" {
		parts = append(parts, a.Locality)
	}
	if a.Region != "" {
		parts = append(parts, a.Region)
	}
	if a.PostalCode != "" {
		parts = append(parts, a.PostalCode)
	}
	if a.Country != "" {
		parts = append(parts, a.Country)
	}
	return strings.Join(parts, ", ")
}

// LogValue masks the street address for PII-safe structured logging.
func (a Address) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("street_address", maskString(a.StreetAddress)),
		slog.String("locality", a.Locality),
		slog.String("region", a.Region),
		slog.String("postal_code", maskString(a.PostalCode)),
		slog.String("country", a.Country),
	)
}

// UserProfile represents a user's personal profile information.
type UserProfile struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Phone     string    `json:"phone"`
	Address   Address   `json:"address"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LogValue masks sensitive PII fields (name, phone, address) for safe structured logging.
func (u UserProfile) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", u.ID),
		slog.String("name", MaskString(u.Name)),
		slog.String("phone", MaskPhone(u.Phone)),
		slog.Any("address", u.Address),
		slog.Time("created_at", u.CreatedAt),
	)
}

// UserCredential represents the authentication credential for a user.
// PasswordHash is excluded from JSON serialization to prevent credential leaks.
type UserCredential struct {
	UserID       string    `json:"user_id"`
	Username     string    `json:"username"`
	Method       string    `json:"method"` // e.g. "bcrypt", "argon2id"
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// LogValue redacts credential details for safe structured logging.
func (c UserCredential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("user_id", c.UserID),
		slog.String("username", c.Username),
		slog.String("method", c.Method),
		slog.String("password", "[REDACTED]"),
	)
}

// SearchQuery defines criteria for filtering and paginating user profiles.
type SearchQuery struct {
	Name     string `json:"name"`
	Phone    string `json:"phone"`
	Locality string `json:"locality"`
	Region   string `json:"region"`
	Country  string `json:"country"`
	Limit    int    `json:"limit"`
	Offset   int    `json:"offset"`
}

// IdentityPII represents personal data returned by 3rd party identity providers.
type IdentityPII struct {
	Name    string  `json:"name"`
	Phone   string  `json:"phone"`
	Address Address `json:"address"`
}

// LogValue masks sensitive PII when logging identity provider responses.
func (i IdentityPII) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", MaskString(i.Name)),
		slog.String("phone", MaskPhone(i.Phone)),
		slog.Any("address", i.Address),
	)
}

// MaskPhone redacts all but the last 4 characters of a phone number.
func MaskPhone(phone string) string {
	cleaned := strings.TrimSpace(phone)
	runes := []rune(cleaned)
	if len(runes) <= 4 {
		return "****"
	}
	return fmt.Sprintf("***-***-%s", string(runes[len(runes)-4:]))
}

// MaskString redacts a string while showing the length or basic placeholder.
func MaskString(s string) string {
	trimmed := strings.TrimSpace(s)
	runes := []rune(trimmed)
	if len(runes) == 0 {
		return ""
	}
	if len(runes) <= 3 {
		return "***"
	}
	return string(runes[:1]) + strings.Repeat("*", len(runes)-2) + string(runes[len(runes)-1:])
}

func maskString(s string) string {
	return MaskString(s)
}
