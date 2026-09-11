package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/borch-ai/lid-challenge/internal/models"
)

func intPtr(i int) *int {
	return &i
}

func TestConnector_AuthenticateAndGetIdentity(t *testing.T) {
	var authCalls int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&authCalls, 1)

		var req AuthRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Username == "good-user" && req.Password == "good-pass" {
			_ = json.NewEncoder(w).Encode(AuthResponse{
				AccessToken: "token-abc-12345",
				ExpiresIn:   intPtr(3600),
			})
			return
		}
		if req.Username == "server-err" {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	cfg := VendorConfig{
		ProviderName: "ABC",
		BaseURL:      server.URL,
		Username:     "good-user",
		Password:     "good-pass",
	}
	conn := NewBaseConnector(cfg, nil)

	// 1. Success
	token, err := conn.Authenticate(context.Background(), "good-user", "good-pass")
	if err != nil {
		t.Fatalf("unexpected auth error: %v", err)
	}
	if token != "token-abc-12345" {
		t.Errorf("got token %q, want 'token-abc-12345'", token)
	}

	// 2. Bad credentials
	_, err = conn.Authenticate(context.Background(), "bad-user", "bad-pass")
	if !errors.Is(err, ErrVendorAuthFailed) {
		t.Errorf("expected ErrVendorAuthFailed, got: %v", err)
	}

	// 3. Server error
	_, err = conn.Authenticate(context.Background(), "server-err", "pass")
	if !errors.Is(err, ErrVendorUnavailable) {
		t.Errorf("expected ErrVendorUnavailable, got: %v", err)
	}

	// 4. Empty input
	_, err = conn.Authenticate(context.Background(), "", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got: %v", err)
	}
}

func TestConnector_GetIdentity_WithTokenCachingAndRetry(t *testing.T) {
	var authCalls int32
	var identityCalls int32
	var validToken atomic.Pointer[string]
	initToken := "valid-token-initial"
	validToken.Store(&initToken)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth":
			atomic.AddInt32(&authCalls, 1)
			_ = json.NewEncoder(w).Encode(AuthResponse{
				AccessToken: *validToken.Load(),
				ExpiresIn:   intPtr(60),
			})
		case "/identity":
			atomic.AddInt32(&identityCalls, 1)
			authHeader := r.Header.Get("Authorization")
			current := *validToken.Load()
			if authHeader != "Bearer "+current {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			var req IdentityRequest
			_ = json.NewDecoder(r.Body).Decode(&req)

			if req.Name == "John Doe" && req.Phone == "3035551234" {
				_ = json.NewEncoder(w).Encode(IdentityResponse{
					Name:  "John Doe",
					Phone: "3035551234",
					Address: models.Address{
						StreetAddress: "123 Cherry St",
						Locality:      "Denver",
						Region:        "CO",
						PostalCode:    "80202",
						Country:       "USA",
					},
				})
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := VendorConfig{
		BaseURL:      server.URL,
		Username:     "client_user",
		Password:     "client_pass",
		ProviderName: "ABC",
		Timeout:      2 * time.Second,
	}
	conn := NewBaseConnector(cfg, nil)

	// 1. First call fetches token and identity
	res, err := conn.GetIdentity(context.Background(), "3035551234", "John Doe")
	if err != nil {
		t.Fatalf("expected successful GetIdentity, got: %v", err)
	}
	if res == nil || res.Name != "John Doe" {
		t.Errorf("unexpected identity result: %+v", res)
	}
	if atomic.LoadInt32(&authCalls) != 1 {
		t.Errorf("expected 1 auth call, got %d", authCalls)
	}

	// 2. Second call reuses cached token (no extra /auth call)
	_, err = conn.GetIdentity(context.Background(), "3035551234", "John Doe")
	if err != nil {
		t.Fatalf("unexpected second GetIdentity error: %v", err)
	}
	if atomic.LoadInt32(&authCalls) != 1 {
		t.Errorf("expected authCalls to remain 1 due to caching, got %d", authCalls)
	}

	// 3. Invalidate token on server side: client should see 401, re-auth, and succeed
	rotatedToken := "rotated-token-999"
	validToken.Store(&rotatedToken)
	res, err = conn.GetIdentity(context.Background(), "3035551234", "John Doe")
	if err != nil {
		t.Fatalf("expected retry after 401 to succeed, got: %v", err)
	}
	if res == nil || res.Name != "John Doe" {
		t.Errorf("unexpected res after retry: %+v", res)
	}
	if atomic.LoadInt32(&authCalls) != 2 {
		t.Errorf("expected 2 auth calls after 401 retry, got %d", authCalls)
	}

	// 4. Not found case
	_, err = conn.GetIdentity(context.Background(), "0000000000", "Ghost User")
	if !errors.Is(err, ErrVendorNotFound) {
		t.Errorf("expected ErrVendorNotFound, got: %v", err)
	}

	// 5. Input validation error
	_, err = conn.GetIdentity(context.Background(), "", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got: %v", err)
	}
}

func TestConnector_Singleflight(t *testing.T) {
	var authCalls int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			atomic.AddInt32(&authCalls, 1)
			// Simulate slight latency to test concurrency collapse
			time.Sleep(50 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(AuthResponse{
				AccessToken: "shared-token",
				ExpiresIn:   intPtr(300),
			})
			return
		}
		if r.URL.Path == "/identity" {
			_ = json.NewEncoder(w).Encode(IdentityResponse{
				Name:  "Test User",
				Phone: "1234567890",
				Address: models.Address{
					StreetAddress: "123 Cherry St",
					Locality:      "Denver",
					Region:        "CO",
					PostalCode:    "80202",
					Country:       "USA",
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	cfg := VendorConfig{
		ProviderName: "XYZ",
		BaseURL:      server.URL,
		Username:     "test",
		Password:     "test",
	}
	conn := NewBaseConnector(cfg, nil)

	// Launch 10 concurrent requests with empty cache
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = conn.GetIdentity(context.Background(), "1234567890", "Test User")
		}()
	}
	wg.Wait()

	// singleflight should deduplicate concurrent authentications into just 1 call
	if calls := atomic.LoadInt32(&authCalls); calls != 1 {
		t.Errorf("expected singleflight to collapse calls to 1, got %d", calls)
	}
}

func TestProviderFactory(t *testing.T) {
	cfg := VendorConfig{
		BaseURL:  "http://example.com",
		Username: "u",
		Password: "p",
	}

	abc, err := NewConnector("ABC", cfg, nil)
	if err != nil || abc.ProviderName() != "ABC" {
		t.Errorf("failed creating ABC connector: %v", err)
	}

	xyz, err := NewConnector("XYZ", cfg, nil)
	if err != nil || xyz.ProviderName() != "XYZ" {
		t.Errorf("failed creating XYZ connector: %v", err)
	}

	_, err = NewConnector("XYC", cfg, nil)
	if err == nil {
		t.Error("expected error for unknown provider XYC")
	}

	_, err = NewConnector("UNKNOWN", cfg, nil)
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestConnector_ErrorHandling(t *testing.T) {
	// 1. Empty token response from /auth
	emptyTokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") == "403" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(AuthResponse{AccessToken: ""})
	}))
	defer emptyTokenServer.Close()
	connEmpty := NewBaseConnector(VendorConfig{BaseURL: emptyTokenServer.URL}, nil)
	_, err := connEmpty.Authenticate(context.Background(), "u", "p")
	if !errors.Is(err, ErrVendorAuthFailed) {
		t.Errorf("expected ErrVendorAuthFailed for empty access_token, got: %v", err)
	}

	// 1b. 403 Forbidden on /auth
	reqCtx := context.Background()
	server403 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server403.Close()
	connForbidden := NewBaseConnector(VendorConfig{BaseURL: server403.URL}, nil)
	_, err = connForbidden.Authenticate(reqCtx, "u", "p")
	if !errors.Is(err, ErrVendorForbidden) {
		t.Errorf("expected ErrVendorForbidden for 403 status, got: %v", err)
	}

	// 2. Bad JSON from /auth
	badJSONServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{invalid-json"))
	}))
	defer badJSONServer.Close()
	connBad := NewBaseConnector(VendorConfig{BaseURL: badJSONServer.URL}, nil)
	_, err = connBad.Authenticate(context.Background(), "u", "p")
	if err == nil {
		t.Error("expected error for malformed auth JSON, got nil")
	}

	// 3. /identity 500 error
	err500Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			_ = json.NewEncoder(w).Encode(AuthResponse{AccessToken: "token"})
			return
		}
		http.Error(w, "crash", http.StatusInternalServerError)
	}))
	defer err500Server.Close()
	conn500 := NewBaseConnector(VendorConfig{BaseURL: err500Server.URL, Username: "u", Password: "p"}, nil)
	_, err = conn500.GetIdentity(context.Background(), "123", "name")
	if !errors.Is(err, ErrVendorUnavailable) {
		t.Errorf("expected ErrVendorUnavailable for 500 error, got: %v", err)
	}

	// 4. /identity bad JSON
	badJSONIdentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			_ = json.NewEncoder(w).Encode(AuthResponse{AccessToken: "token"})
			return
		}
		_, _ = w.Write([]byte("{invalid-json"))
	}))
	defer badJSONIdentServer.Close()
	connBadIdent := NewBaseConnector(VendorConfig{BaseURL: badJSONIdentServer.URL, Username: "u", Password: "p"}, nil)
	_, err = connBadIdent.GetIdentity(context.Background(), "123", "name")
	if err == nil {
		t.Error("expected error for malformed identity JSON, got nil")
	}

	// 5. /identity unexpected status (e.g. 418 Teapot)
	teapotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			_ = json.NewEncoder(w).Encode(AuthResponse{AccessToken: "token"})
			return
		}
		http.Error(w, "I'm a teapot", http.StatusTeapot)
	}))
	defer teapotServer.Close()
	connTeapot := NewBaseConnector(VendorConfig{BaseURL: teapotServer.URL, Username: "u", Password: "p"}, nil)
	_, err = connTeapot.GetIdentity(context.Background(), "123", "name")
	if !errors.Is(err, ErrVendorUnavailable) {
		t.Errorf("expected ErrVendorUnavailable for teapot status, got: %v", err)
	}

	// 6. /identity 403 Forbidden returns ErrVendorForbidden without retry loop
	forbiddenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			_ = json.NewEncoder(w).Encode(AuthResponse{AccessToken: "token"})
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer forbiddenServer.Close()
	conn403 := NewBaseConnector(VendorConfig{BaseURL: forbiddenServer.URL, Username: "u", Password: "p"}, nil)
	_, err = conn403.GetIdentity(context.Background(), "123", "name")
	if !errors.Is(err, ErrVendorForbidden) {
		t.Errorf("expected ErrVendorForbidden for 403 identity status, got: %v", err)
	}

	// 7. /identity missing required fields (e.g. empty JSON {})
	emptyIdentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			_ = json.NewEncoder(w).Encode(AuthResponse{AccessToken: "token"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer emptyIdentServer.Close()
	connEmptyIdent := NewBaseConnector(VendorConfig{BaseURL: emptyIdentServer.URL, Username: "u", Password: "p"}, nil)
	_, err = connEmptyIdent.GetIdentity(context.Background(), "123", "name")
	if !errors.Is(err, ErrVendorUnavailable) {
		t.Errorf("expected ErrVendorUnavailable for empty identity payload, got: %v", err)
	}
}

func TestConnector_GetIdentity_ContextCanceled(t *testing.T) {
	conn := NewBaseConnector(VendorConfig{
		BaseURL:  "http://example.test",
		Username: "u",
		Password: "p",
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := conn.GetIdentity(ctx, "123", "name")
	if err == nil {
		t.Error("expected error for canceled context")
	}
}

func TestConnector_DoubleCheckCache(t *testing.T) {
	conn := NewBaseConnector(VendorConfig{
		BaseURL:  "http://example.test",
		Username: "u",
		Password: "p",
	}, nil)
	conn.cachedToken = "cached-valid-token"
	conn.tokenExpiry = time.Now().Add(1 * time.Hour)

	token, err := conn.getValidToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "cached-valid-token" {
		t.Errorf("got %q, want 'cached-valid-token'", token)
	}
}

func TestConnector_GetValidToken_AuthFailure(t *testing.T) {
	conn := NewBaseConnector(VendorConfig{
		BaseURL:  "http://127.0.0.1:1",
		Username: "u",
		Password: "p",
		Timeout:  100 * time.Millisecond,
	}, nil)

	_, err := conn.getValidToken(context.Background())
	if err == nil {
		t.Error("expected error for failed upstream authentication")
	}
}

func TestConnector_ContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	conn := NewBaseConnector(VendorConfig{
		BaseURL:  server.URL,
		Username: "u",
		Password: "p",
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := conn.Authenticate(ctx, "u", "p")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in Authenticate, got: %v", err)
	}

	_, err = conn.executeIdentityRequest(ctx, "token", "1234567890", "John Doe")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in executeIdentityRequest, got: %v", err)
	}
}

func TestVendorConfig_JSONPasswordOmitted(t *testing.T) {
	cfg := VendorConfig{
		ProviderName: "ABC",
		BaseURL:      "https://example.test",
		Username:     "user",
		Password:     "super-secret-password",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal VendorConfig: %v", err)
	}
	if strings.Contains(string(data), "super-secret-password") {
		t.Errorf("expected password to be omitted from JSON, got: %s", string(data))
	}
}

func TestConnector_ExpiresInOverflowCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthResponse{
			AccessToken: "huge-exp-token",
			ExpiresIn:   intPtr(1 << 30), // huge value
		})
	}))
	defer server.Close()

	conn := NewBaseConnector(VendorConfig{
		BaseURL:  server.URL,
		Username: "user",
		Password: "pass",
	}, nil)

	token, err := conn.Authenticate(context.Background(), "user", "pass")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "huge-exp-token" {
		t.Errorf("got %q, want 'huge-exp-token'", token)
	}
	if conn.tokenExpiry.Before(time.Now()) {
		t.Error("token expiry wrapped negative or before now")
	}
}

func TestConnector_ExpiresInZeroOrOmitted(t *testing.T) {
	// 1. Test expires_in: 0 -> token is returned but NOT cached
	serverZero := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthResponse{
			AccessToken: "zero-exp-token",
			ExpiresIn:   intPtr(0),
		})
	}))
	defer serverZero.Close()

	connZero := NewBaseConnector(VendorConfig{
		BaseURL:  serverZero.URL,
		Username: "user",
		Password: "pass",
	}, nil)

	tokZero, err := connZero.Authenticate(context.Background(), "user", "pass")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokZero != "zero-exp-token" {
		t.Errorf("got %q, want 'zero-exp-token'", tokZero)
	}
	if connZero.cachedToken != "" {
		t.Errorf("expected cachedToken to be empty for expires_in=0, got %q", connZero.cachedToken)
	}

	// 2. Test expires_in omitted (nil) -> falls back to DefaultTokenTTL and caches
	serverOmitted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthResponse{
			AccessToken: "default-ttl-token",
			ExpiresIn:   nil,
		})
	}))
	defer serverOmitted.Close()

	connOmitted := NewBaseConnector(VendorConfig{
		BaseURL:         serverOmitted.URL,
		Username:        "user",
		Password:        "pass",
		DefaultTokenTTL: 10 * time.Minute,
	}, nil)

	tokOmitted, err := connOmitted.Authenticate(context.Background(), "user", "pass")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokOmitted != "default-ttl-token" {
		t.Errorf("got %q, want 'default-ttl-token'", tokOmitted)
	}
	if connOmitted.cachedToken != "default-ttl-token" {
		t.Errorf("expected cachedToken to be 'default-ttl-token', got %q", connOmitted.cachedToken)
	}
}

func TestConnector_IdentityAddressValidation(t *testing.T) {
	// Vendor returns identity with missing street_address
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			_ = json.NewEncoder(w).Encode(AuthResponse{
				AccessToken: "test-token",
				ExpiresIn:   intPtr(300),
			})
			return
		}
		if r.URL.Path == "/identity" {
			_ = json.NewEncoder(w).Encode(IdentityResponse{
				Name:  "Jane Doe",
				Phone: "555-1234",
				Address: models.Address{
					StreetAddress: "", // missing!
					Locality:      "Denver",
					Region:        "CO",
					PostalCode:    "80202",
					Country:       "USA",
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	conn := NewBaseConnector(VendorConfig{
		BaseURL:  server.URL,
		Username: "user",
		Password: "pass",
	}, nil)

	_, err := conn.GetIdentity(context.Background(), "555-1234", "Jane Doe")
	if err == nil {
		t.Fatal("expected error when vendor returns identity with missing address fields, got nil")
	}
	if !strings.Contains(err.Error(), "missing required address fields") {
		t.Errorf("expected error message mentioning missing address fields, got: %v", err)
	}
}

func TestConnector_NoFollowRedirects(t *testing.T) {
	redirectHit := false
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	conn := NewBaseConnector(VendorConfig{
		BaseURL:  server.URL,
		Username: "user",
		Password: "pass",
	}, nil)

	_, err := conn.Authenticate(context.Background(), "user", "pass")
	if err == nil {
		t.Fatal("expected error on 307 redirect when redirects are disabled, got nil")
	}
	if redirectHit {
		t.Error("redirect target was unexpectedly called; HTTP client followed redirect")
	}
}

func TestConnector_ZeroTTLInvalidatesCache(t *testing.T) {
	currentTTL := 60
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			_ = json.NewEncoder(w).Encode(AuthResponse{
				AccessToken: "active-token",
				ExpiresIn:   &currentTTL,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	conn := NewBaseConnector(VendorConfig{
		BaseURL:  server.URL,
		Username: "user",
		Password: "pass",
	}, nil)

	// First auth caches token
	tok, err := conn.Authenticate(context.Background(), "user", "pass")
	if err != nil || tok != "active-token" {
		t.Fatalf("first auth failed: %v", err)
	}
	if conn.cachedToken != "active-token" {
		t.Errorf("expected cached token to be 'active-token', got %q", conn.cachedToken)
	}

	// Vendor returns immediate expiry (0s): must invalidate cache
	currentTTL = 0
	tok2, err := conn.Authenticate(context.Background(), "user", "pass")
	if err != nil || tok2 != "active-token" {
		t.Fatalf("second auth failed: %v", err)
	}
	if conn.cachedToken != "" {
		t.Errorf("expected cached token to be cleared on 0s TTL, got %q", conn.cachedToken)
	}
}

func TestConnector_SchemeValidation(t *testing.T) {
	t.Setenv("APP_ENV", "production")

	// 1. Cleartext HTTP to external host in production must fail
	conn := NewBaseConnector(VendorConfig{
		BaseURL:  "http://insecure-vendor.com",
		Username: "user",
		Password: "pass",
	}, nil)

	if _, err := conn.Authenticate(context.Background(), "user", "pass"); err == nil {
		t.Fatal("expected error for cleartext HTTP vendor in production, got nil")
	}
	if _, err := conn.GetIdentity(context.Background(), "1234567890", "Test"); err == nil {
		t.Fatal("expected error for cleartext HTTP vendor in GetIdentity in production, got nil")
	}

	// 2. Subdomain or userinfo spoofing loopback must fail
	spoofedURLs := []string{
		"http://localhost.attacker.example",
		"http://localhost@attacker.example",
		"http://user:pass@localhost:8080",
		"https://user:password@vendor.example",
		"https://vendor.example/base?tenant=1",
		"https://vendor.example/base#fragment",
		"https:///path-without-host",
		"ftp://localhost",
		"",
	}
	for _, rawURL := range spoofedURLs {
		spoofedConn := NewBaseConnector(VendorConfig{
			BaseURL:  rawURL,
			Username: "user",
			Password: "pass",
		}, nil)
		if _, err := spoofedConn.Authenticate(context.Background(), "user", "pass"); err == nil {
			t.Errorf("expected error for unsafe URL %q in production, got nil", rawURL)
		}
		if _, err := spoofedConn.GetIdentity(context.Background(), "1234567890", "Test"); err == nil {
			t.Errorf("expected error for unsafe URL %q in GetIdentity, got nil", rawURL)
		}
	}

	// 3. Exact loopback hostnames (localhost, 127.0.0.1, [::1]) without userinfo are permitted in production
	loopbackURLs := []string{
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
		"https://vendor.example.com",
	}
	for _, u := range loopbackURLs {
		if err := validateVendorURL(u); err != nil {
			t.Errorf("expected URL %q to be valid in production, got %v", u, err)
		}
	}

	// 4. Whitespace trimming: BaseURL with leading/trailing whitespace
	paddedConn := NewBaseConnector(VendorConfig{
		BaseURL:  "  http://localhost:8080/  ",
		Username: "user",
		Password: "pass",
	}, nil)
	if paddedConn.cfg.BaseURL != "http://localhost:8080" {
		t.Errorf("expected trimmed BaseURL 'http://localhost:8080', got %q", paddedConn.cfg.BaseURL)
	}

	// 5. Cleartext HTTP in development should succeed
	t.Setenv("APP_ENV", "development")
	localServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthResponse{AccessToken: "tok"})
	}))
	defer localServer.Close()

	localConn := NewBaseConnector(VendorConfig{
		BaseURL:  localServer.URL,
		Username: "user",
		Password: "pass",
	}, nil)
	_, err := localConn.Authenticate(context.Background(), "user", "pass")
	if err != nil {
		t.Errorf("expected cleartext localhost to succeed in development, got: %v", err)
	}
}
