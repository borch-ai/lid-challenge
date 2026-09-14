// Package connector provides HTTP client integrations for external identity providers.
package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/borch-ai/lid-challenge/internal/models"
	"golang.org/x/sync/singleflight"
)

// VendorConfig holds connection parameters and credentials for an identity vendor.
type VendorConfig struct {
	ProviderName    string        `json:"provider_name"` // "ABC" or "XYZ"
	BaseURL         string        `json:"base_url"`
	Username        string        `json:"username"`
	Password        string        `json:"-"`
	Timeout         time.Duration `json:"timeout"`
	DefaultTokenTTL time.Duration `json:"default_token_ttl"`
}

// BaseConnector implements IdentityConnector with resilient HTTP transport,
// token caching, and singleflight synchronization to prevent thundering herds.
type BaseConnector struct {
	cfg        VendorConfig
	httpClient *http.Client
	sf         singleflight.Group
	logger     *slog.Logger

	mu          sync.RWMutex
	cachedToken string
	tokenExpiry time.Time
}

// validateVendorURL verifies that rawURL is a valid HTTP/HTTPS URL and restricts cleartext HTTP
// strictly to exact loopback hostnames (localhost, 127.0.0.1, ::1) without userinfo, unless in development/test.
func validateVendorURL(rawURL string) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return fmt.Errorf("%w: vendor BaseURL cannot be empty", ErrInvalidInput)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("%w: invalid vendor BaseURL: %v", ErrInvalidInput, err)
	}

	if u.User != nil {
		return fmt.Errorf("%w: vendor BaseURL must not contain userinfo", ErrInvalidInput)
	}

	if strings.TrimSpace(u.Host) == "" || strings.TrimSpace(u.Hostname()) == "" {
		return fmt.Errorf("%w: vendor BaseURL must include a valid host", ErrInvalidInput)
	}

	if u.RawQuery != "" {
		return fmt.Errorf("%w: vendor BaseURL must not contain query parameters", ErrInvalidInput)
	}

	if u.Fragment != "" {
		return fmt.Errorf("%w: vendor BaseURL must not contain fragments", ErrInvalidInput)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme == "https" {
		return nil
	}

	if scheme != "http" {
		return fmt.Errorf("%w: unsupported scheme %q in vendor BaseURL", ErrInvalidInput, u.Scheme)
	}

	env := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	if env == "development" || env == "dev" || env == "test" {
		return nil
	}

	hostname := strings.ToLower(u.Hostname())
	if hostname == "localhost" {
		return nil
	}
	if ip := net.ParseIP(hostname); ip != nil && ip.IsLoopback() {
		return nil
	}

	return fmt.Errorf("%w: cleartext HTTP vendor BaseURL is only permitted for loopback/localhost or in development/test environments", ErrInvalidInput)
}

// NewBaseConnector initializes a connector with connection pooling and timeouts.
func NewBaseConnector(cfg VendorConfig, logger *slog.Logger) *BaseConnector {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.DefaultTokenTTL <= 0 {
		cfg.DefaultTokenTTL = 15 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}

	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}

	return &BaseConnector{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger,
	}
}

// ProviderName returns the identifier of the identity vendor.
func (c *BaseConnector) ProviderName() string {
	return c.cfg.ProviderName
}

// Authenticate obtains an access_token from the vendor's /auth endpoint.
func (c *BaseConnector) Authenticate(ctx context.Context, username, password string) (string, error) {
	if strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
		return "", fmt.Errorf("%w: username and password cannot be empty", ErrInvalidInput)
	}

	if err := validateVendorURL(c.cfg.BaseURL); err != nil {
		return "", err
	}

	authURL := strings.TrimRight(strings.TrimSpace(c.cfg.BaseURL), "/") + "/auth"

	payload := AuthRequest{
		Username: username,
		Password: password,
	}
	jsonBody, err := json.Marshal(payload) //nolint:gosec // G117: outbound vendor authentication request requires marshaling credentials
	if err != nil {
		return "", fmt.Errorf("failed to encode auth request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to build auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	c.logger.Debug("requesting vendor auth token",
		slog.String("provider", c.cfg.ProviderName),
		slog.String("url", authURL),
		slog.String("username", username),
	)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w: %v", ErrVendorUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusUnauthorized {
		return "", ErrVendorAuthFailed
	}
	if resp.StatusCode == http.StatusForbidden {
		return "", ErrVendorForbidden
	}
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("%w: server responded with %d", ErrVendorUnavailable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: unexpected status %d", ErrVendorUnavailable, resp.StatusCode)
	}

	var authResp AuthResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&authResp); err != nil {
		return "", fmt.Errorf("failed to decode vendor auth response: %w", err)
	}

	if authResp.AccessToken == "" {
		return "", fmt.Errorf("%w: vendor response missing access_token", ErrVendorAuthFailed)
	}

	const maxTTLSeconds = 30 * 24 * 3600 // 30 days maximum to prevent integer overflow
	var shouldCache bool
	var ttl time.Duration

	if authResp.ExpiresIn == nil {
		// Omitted by vendor: fall back to default configured TTL
		ttl = c.cfg.DefaultTokenTTL
		shouldCache = true
	} else if *authResp.ExpiresIn > 0 {
		expSeconds := *authResp.ExpiresIn
		if expSeconds > maxTTLSeconds {
			expSeconds = maxTTLSeconds
		}
		ttl = time.Duration(expSeconds) * time.Second
		shouldCache = true
	} else {
		// Explicitly 0 or negative: vendor indicated token expires immediately; do not cache
		shouldCache = false
	}

	// Populate or invalidate shared connector cache for configured credentials
	if username == c.cfg.Username && password == c.cfg.Password {
		c.mu.Lock()
		if shouldCache {
			c.cachedToken = authResp.AccessToken
			// Subtract proportional safety margin (up to 30s, or half the TTL for short-lived tokens)
			margin := 30 * time.Second
			if ttl < 60*time.Second {
				margin = ttl / 2
			}
			c.tokenExpiry = time.Now().Add(ttl - margin)
		} else {
			// Vendor returned non-positive TTL: clear any existing cached token
			c.cachedToken = ""
			c.tokenExpiry = time.Time{}
		}
		c.mu.Unlock()
	}

	return authResp.AccessToken, nil
}

// getValidToken retrieves a non-expired token from cache, or acquires one under singleflight lock.
func (c *BaseConnector) getValidToken(ctx context.Context) (string, error) {
	c.mu.RLock()
	token := c.cachedToken
	valid := token != "" && time.Now().Before(c.tokenExpiry)
	c.mu.RUnlock()

	if valid {
		return token, nil
	}

	// Token missing or expired; use singleflight with a dedicated connector context
	// while allowing individual callers to abort if their context is canceled.
	timeout := c.cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	ch := c.sf.DoChan("auth", func() (any, error) {
		// Re-check cache inside singleflight callback to handle race where another caller
		// completed authentication right before this singleflight entry was registered.
		c.mu.RLock()
		if c.cachedToken != "" && time.Now().Before(c.tokenExpiry) {
			token := c.cachedToken
			c.mu.RUnlock()
			return token, nil
		}
		c.mu.RUnlock()

		authCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return c.Authenticate(authCtx, c.cfg.Username, c.cfg.Password)
	})

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return "", res.Err
		}
		return res.Val.(string), nil
	}
}

// invalidateTokenIfMatches clears the cached token only if it matches the rejected token.
func (c *BaseConnector) invalidateTokenIfMatches(rejectedToken string) {
	c.mu.Lock()
	if c.cachedToken == rejectedToken {
		c.cachedToken = ""
		c.tokenExpiry = time.Time{}
	}
	c.mu.Unlock()
}

// GetIdentity retrieves user PII from vendor's /identity endpoint.
func (c *BaseConnector) GetIdentity(ctx context.Context, phone, name string) (*models.IdentityPII, error) {
	if strings.TrimSpace(phone) == "" || strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: phone and name are required", ErrInvalidInput)
	}

	// Try with cached/new token
	token, err := c.getValidToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to obtain vendor access token: %w", err)
	}

	result, err := c.executeIdentityRequest(ctx, token, phone, name)
	if err != nil && errors.Is(err, ErrVendorAuthFailed) {
		// Invalidate token cache only if it matches the rejected token and retry once
		c.invalidateTokenIfMatches(token)
		c.logger.Warn("vendor token rejected (401), re-authenticating and retrying",
			slog.String("provider", c.cfg.ProviderName),
		)

		token, err = c.getValidToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("re-authentication failed: %w", err)
		}
		result, err = c.executeIdentityRequest(ctx, token, phone, name)
	}

	return result, err
}

// executeIdentityRequest sends the POST /identity request to the vendor.
func (c *BaseConnector) executeIdentityRequest(ctx context.Context, token, phone, name string) (*models.IdentityPII, error) {
	if err := validateVendorURL(c.cfg.BaseURL); err != nil {
		return nil, err
	}
	identityURL := strings.TrimRight(strings.TrimSpace(c.cfg.BaseURL), "/") + "/identity"

	payload := IdentityRequest{
		Phone: phone,
		Name:  name,
	}
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to encode identity request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, identityURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to build identity request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	c.logger.Debug("querying vendor identity endpoint",
		slog.String("provider", c.cfg.ProviderName),
		slog.String("phone", models.MaskPhone(phone)),
		slog.String("name", models.MaskString(name)),
	)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrVendorUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrVendorAuthFailed
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrVendorForbidden
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrVendorNotFound
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: server returned status %d", ErrVendorUnavailable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: server returned status %d", ErrVendorUnavailable, resp.StatusCode)
	}

	var identityResp IdentityResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&identityResp); err != nil {
		return nil, fmt.Errorf("failed to parse vendor identity response: %w", err)
	}

	addr := identityResp.Address
	if strings.TrimSpace(identityResp.Name) == "" || strings.TrimSpace(identityResp.Phone) == "" {
		return nil, fmt.Errorf("%w: vendor identity response missing required name or phone", ErrVendorUnavailable)
	}
	if strings.TrimSpace(addr.StreetAddress) == "" ||
		strings.TrimSpace(addr.Locality) == "" ||
		strings.TrimSpace(addr.Region) == "" ||
		strings.TrimSpace(addr.PostalCode) == "" ||
		strings.TrimSpace(addr.Country) == "" {
		return nil, fmt.Errorf("%w: vendor identity response missing required address fields", ErrVendorUnavailable)
	}

	return &models.IdentityPII{
		Name:    identityResp.Name,
		Phone:   identityResp.Phone,
		Address: identityResp.Address,
	}, nil
}
