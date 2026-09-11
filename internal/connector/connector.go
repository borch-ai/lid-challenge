package connector

import (
	"context"
	"errors"

	"github.com/borch-ai/lid-challenge/internal/models"
)

var (
	// ErrVendorAuthFailed indicates failed authentication with the vendor (401).
	ErrVendorAuthFailed = errors.New("vendor authentication failed")
	// ErrVendorForbidden indicates forbidden access to vendor resources (403).
	ErrVendorForbidden = errors.New("vendor authorization forbidden (403)")
	// ErrVendorNotFound indicates the user was not found by the vendor.
	ErrVendorNotFound = errors.New("vendor identity not found")
	// ErrVendorUnavailable indicates a network or upstream server error.
	ErrVendorUnavailable = errors.New("vendor service unavailable")
	// ErrInvalidInput indicates missing or malformed query parameters.
	ErrInvalidInput = errors.New("invalid input parameters")
)

// IdentityConnector defines the contract for interacting with external
// 3rd party identity verification providers (e.g., ABC, XYZ).
type IdentityConnector interface {
	// Authenticate queries the vendor's /auth endpoint to obtain an access_token.
	Authenticate(ctx context.Context, username, password string) (string, error)

	// GetIdentity queries the vendor's /identity endpoint to fetch verified user PII.
	GetIdentity(ctx context.Context, phone, name string) (*models.IdentityPII, error)

	// ProviderName returns the identifier of the identity provider (e.g., "ABC", "XYZ").
	ProviderName() string
}

// AuthRequest represents the wire request body for vendor /auth.
type AuthRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// AuthResponse represents the wire response body for vendor /auth.
type AuthResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   *int   `json:"expires_in,omitempty"` // in seconds, optional
}

// IdentityRequest represents the wire request body for vendor /identity.
type IdentityRequest struct {
	Phone string `json:"phone"`
	Name  string `json:"name"`
}

// IdentityResponse represents the wire response body for vendor /identity.
type IdentityResponse struct {
	Name    string         `json:"name"`
	Phone   string         `json:"phone"`
	Address models.Address `json:"address"`
}
