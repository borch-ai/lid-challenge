package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHashAndVerifyPassword(t *testing.T) {
	password := "CorrectHorseBatteryStaple!"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("unexpected error hashing password: %v", err)
	}

	if hash == password {
		t.Fatal("hash should not match plain password")
	}

	// Verify with correct password
	if err := CheckPassword(hash, password); err != nil {
		t.Errorf("expected valid password verification, got: %v", err)
	}

	// Verify with wrong password
	if err := CheckPassword(hash, "WrongPassword"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword, got: %v", err)
	}

	// Test empty password hashing
	if _, err := HashPassword(""); !errors.Is(err, ErrEmptyPassword) {
		t.Errorf("expected ErrEmptyPassword, got: %v", err)
	}

	// Test empty password check
	if err := CheckPassword(hash, ""); !errors.Is(err, ErrEmptyPassword) {
		t.Errorf("expected ErrEmptyPassword, got: %v", err)
	}

	// Test malformed hash
	if err := CheckPassword("not-a-valid-hash", "somepass"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword for malformed hash, got: %v", err)
	}
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		expected string
	}{
		{"valid header", "Bearer my-secret-token", "my-secret-token"},
		{"with spaces", "Bearer   spaced-token  ", "spaced-token"},
		{"missing bearer prefix", "Token my-secret-token", ""},
		{"lowercase bearer prefix", "bearer my-secret-token", "my-secret-token"},
		{"uppercase bearer prefix", "BEARER my-secret-token", "my-secret-token"},
		{"padded bearer prefix", "   Bearer my-secret-token   ", "my-secret-token"},
		{"short header", "Bear", ""},
		{"empty header", "", ""},
		{"bearer only", "Bearer ", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractBearerToken(tt.header)
			if got != tt.expected {
				t.Errorf("ExtractBearerToken(%q) = %q, want %q", tt.header, got, tt.expected)
			}
		})
	}
}

func TestVerifyToken(t *testing.T) {
	tests := []struct {
		name     string
		provided string
		expected string
		valid    bool
	}{
		{"matching tokens", "token123", "token123", true},
		{"different tokens", "token123", "token456", false},
		{"empty provided", "", "token123", false},
		{"empty expected", "token123", "", false},
		{"both empty", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifyToken(tt.provided, tt.expected); got != tt.valid {
				t.Errorf("VerifyToken(%q, %q) = %v, want %v", tt.provided, tt.expected, got, tt.valid)
			}
		})
	}
}

func TestUserToken(t *testing.T) {
	secret := "test-secret-key-12345"
	userID := "user-uuid-abc-123"
	username := "testuser"

	// 1. Valid token
	token, err := GenerateUserToken(userID, username, secret, 1*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error generating token: %v", err)
	}

	claims, err := VerifyUserToken(token, secret)
	if err != nil {
		t.Fatalf("unexpected error verifying token: %v", err)
	}

	if claims.UserID != userID {
		t.Errorf("expected userID %q, got %q", userID, claims.UserID)
	}
	if claims.Username != username {
		t.Errorf("expected username %q, got %q", username, claims.Username)
	}
	if claims.ExpiresAt <= time.Now().Unix() {
		t.Errorf("expected future expiry, got %v", claims.ExpiresAt)
	}

	// 2. Expired token
	expiredToken, err := GenerateUserToken(userID, username, secret, -1*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error generating expired token: %v", err)
	}
	if _, err := VerifyUserToken(expiredToken, secret); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("expected ErrTokenExpired, got: %v", err)
	}

	// 2b. Exact expiration boundary (exp == now must be expired)
	boundaryClaims := TokenClaims{
		UserID:    userID,
		Username:  username,
		ExpiresAt: time.Now().Unix(),
	}
	claimsBytes, _ := json.Marshal(boundaryClaims)
	payloadB64 := base64.RawURLEncoding.EncodeToString(claimsBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payloadB64))
	sigB64 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	boundaryToken := payloadB64 + "." + sigB64
	if _, err := VerifyUserToken(boundaryToken, secret); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("expected ErrTokenExpired for exact expiration second, got: %v", err)
	}

	// 3. Wrong secret
	if _, err := VerifyUserToken(token, "wrong-secret"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expected ErrInvalidToken for wrong secret, got: %v", err)
	}

	// 4. Tampered payload
	tamperedPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"usr":"hacker"}`))
	tamperedToken := tamperedPayload + "." + strings.Split(token, ".")[1]
	if _, err := VerifyUserToken(tamperedToken, secret); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expected ErrInvalidToken for tampered token, got: %v", err)
	}

	// 5. Malformed tokens
	malformedCases := []string{
		"",
		"not-a-token",
		"too.many.dots.here",
		"invalid-base64.signature",
		"payload.invalid-sig!",
	}
	for _, mc := range malformedCases {
		if _, err := VerifyUserToken(mc, secret); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("expected ErrInvalidToken for %q, got: %v", mc, err)
		}
	}

	// 6. Empty secret error on generate
	if _, err := GenerateUserToken(userID, username, "", time.Hour); err == nil {
		t.Error("expected error generating token with empty secret, got nil")
	}

	// 7. Oversized token / signature rejection
	oversizedToken := strings.Repeat("a", 4097)
	if _, err := VerifyUserToken(oversizedToken, secret); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expected ErrInvalidToken for oversized token, got: %v", err)
	}
	oversizedSig := "validpayload." + strings.Repeat("s", 129)
	if _, err := VerifyUserToken(oversizedSig, secret); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expected ErrInvalidToken for oversized signature, got: %v", err)
	}
}
