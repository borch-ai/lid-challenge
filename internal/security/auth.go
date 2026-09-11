// Package security provides authentication, hashing, and token issuance utilities.
package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrInvalidToken is returned when a bearer token signature or format is invalid.
	ErrInvalidToken = errors.New("invalid authentication token")
	// ErrTokenExpired is returned when a bearer token's exp claim is in the past.
	ErrTokenExpired = errors.New("authentication token has expired")
)

// TokenClaims represents the claims stored inside a per-user signed bearer token.
type TokenClaims struct {
	UserID    string `json:"sub"`
	Username  string `json:"usr"`
	ExpiresAt int64  `json:"exp"`
}

// GenerateUserToken generates a signed per-user Bearer token with an expiration timestamp.
// Format: base64url(json(claims)).base64url(hmac_sha256(payload, secret))
func GenerateUserToken(userID, username, secret string, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", errors.New("cannot generate user token with empty secret")
	}

	claims := TokenClaims{
		UserID:    userID,
		Username:  username,
		ExpiresAt: time.Now().Add(ttl).Unix(),
	}

	payloadBytes, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("failed to marshal token claims: %w", err)
	}

	payloadPart := base64.RawURLEncoding.EncodeToString(payloadBytes)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payloadPart))
	sigPart := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return payloadPart + "." + sigPart, nil
}

// VerifyUserToken validates the HMAC-SHA256 signature and expiration timestamp of a token.
func VerifyUserToken(tokenStr, secret string) (*TokenClaims, error) {
	if secret == "" || tokenStr == "" {
		return nil, ErrInvalidToken
	}

	parts := strings.Split(tokenStr, ".")
	if len(parts) != 2 {
		return nil, ErrInvalidToken
	}

	payloadPart, sigPart := parts[0], parts[1]

	// Compute expected HMAC
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payloadPart))
	expectedSig := mac.Sum(nil)

	actualSig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil {
		return nil, ErrInvalidToken
	}

	// Constant-time signature comparison using fixed-size digests to prevent length-based timing leaks
	hSig1 := sha256.Sum256(actualSig)
	hSig2 := sha256.Sum256(expectedSig)
	if subtle.ConstantTimeCompare(hSig1[:], hSig2[:]) != 1 {
		return nil, ErrInvalidToken
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return nil, ErrInvalidToken
	}

	var claims TokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, ErrInvalidToken
	}

	if time.Now().Unix() >= claims.ExpiresAt {
		return nil, ErrTokenExpired
	}

	return &claims, nil
}

// ExtractBearerToken parses the Bearer token from an Authorization header value.
// It matches the 'Bearer ' scheme case-insensitively per RFC 7235 and returns an empty string
// if the header is missing, too short, or improperly formatted.
func ExtractBearerToken(header string) string {
	trimmed := strings.TrimSpace(header)
	if len(trimmed) < 7 {
		return ""
	}
	if !strings.EqualFold(trimmed[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(trimmed[7:])
}

// VerifyToken compares the provided token with the expected secret in constant time
// to protect against timing side-channel attacks.
// Comparing fixed-size SHA-256 digests ensures subtle.ConstantTimeCompare always receives
// equal-length 32-byte inputs, preventing early return timing leaks when input lengths differ.
func VerifyToken(provided, expected string) bool {
	if provided == "" || expected == "" {
		return false
	}
	h1 := sha256.Sum256([]byte(provided))
	h2 := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(h1[:], h2[:]) == 1
}
