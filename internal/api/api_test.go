// Package api provides HTTP router, middleware, and handler implementations.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/borch-ai/lid-challenge/internal/dao"
	"github.com/borch-ai/lid-challenge/internal/models"
	"github.com/borch-ai/lid-challenge/internal/security"
)

const testAuthSecret = "test-bearer-secret-xyz"

func setupTestServer(t *testing.T) (*Server, *dao.SQLDAO) {
	t.Helper()

	testDAO, err := dao.NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite dao: %v", err)
	}

	if err := testDAO.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}

	t.Cleanup(func() {
		_ = testDAO.Close()
	})

	cfg := Config{
		Port:           8080,
		AuthSecret:     testAuthSecret,
		RateLimitRPS:   100,
		RateLimitBurst: 200,
	}

	srv, err := NewServer(testDAO, cfg, nil)
	if err != nil {
		t.Fatalf("failed to create test server: %v", err)
	}
	return srv, testDAO
}

func TestAPI_Health(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/health", nil)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got: %d", rec.Code)
	}

	var resp HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse health response: %v", err)
	}
	if resp.Status != "healthy" {
		t.Errorf("expected status 'healthy', got %q", resp.Status)
	}
}

func TestAPI_Ready(t *testing.T) {
	srv, testDAO := setupTestServer(t)

	// 1. Healthy readiness check
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/ready", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK for ready probe, got: %d", rec.Code)
	}

	var resp ReadyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse ready response: %v", err)
	}
	if resp.Status != "ready" || resp.Database != "healthy" {
		t.Errorf("unexpected ready response: %+v", resp)
	}

	// 2. Unhealthy readiness check when database is closed
	_ = testDAO.Close()
	req2 := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/ready", nil)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 Service Unavailable for unready probe, got: %d", rec2.Code)
	}
	var unreadyResp ReadyResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &unreadyResp); err != nil {
		t.Fatalf("failed to parse unready response: %v", err)
	}
	if unreadyResp.Status != "unready" || unreadyResp.Database != "unreachable" {
		t.Errorf("unexpected unready response: %+v", unreadyResp)
	}
}

func TestAPI_OpenAPI(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/openapi.yaml", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got: %d", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("openapi: 3.1.0")) {
		t.Errorf("expected openapi spec in response body")
	}
}

func TestAPI_CreateUserAndLogin(t *testing.T) {
	srv, _ := setupTestServer(t)

	// 1. Create valid user
	userPayload := CreateUserRequest{
		Name:     "Grace Hopper",
		Phone:    "2025550199",
		Username: "ghopper",
		Password: "CompilerPioneer!",
		Address: models.Address{
			StreetAddress: "1 Admiral Way",
			Locality:      "Arlington",
			Region:        "VA",
			PostalCode:    "22202",
			Country:       "USA",
		},
	}
	body, _ := json.Marshal(userPayload) //nolint:gosec // G117: test fixture payload marshaling
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got: %d, body: %s", rec.Code, rec.Body.String())
	}

	var createResp CreateUserResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}
	if createResp.UserID == "" {
		t.Error("expected non-empty user_id in create response")
	}

	// 2. Duplicate user conflict
	reqDup := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader(body))
	recDup := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recDup, reqDup)

	if recDup.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict for duplicate username, got: %d", recDup.Code)
	}

	// 3. Login with correct password
	loginPayload := LoginRequest{
		Username: "ghopper",
		Password: "CompilerPioneer!",
	}
	loginBody, _ := json.Marshal(loginPayload) //nolint:gosec // G117: test fixture payload marshaling
	reqLogin := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/auth/login", bytes.NewReader(loginBody))
	recLogin := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recLogin, reqLogin)

	if recLogin.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for valid login, got: %d", recLogin.Code)
	}

	var loginResp LoginResponse
	if err := json.Unmarshal(recLogin.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("failed to decode login response: %v", err)
	}
	claims, err := security.VerifyUserToken(loginResp.AccessToken, testAuthSecret)
	if err != nil {
		t.Fatalf("expected valid signed user token, got error: %v", err)
	}
	if claims.Username != "ghopper" {
		t.Errorf("expected username 'ghopper' in claims, got: %q", claims.Username)
	}

	// 4. Login with incorrect password
	badLoginPayload := LoginRequest{
		Username: "ghopper",
		Password: "WrongPassword",
	}
	badLoginBody, _ := json.Marshal(badLoginPayload) //nolint:gosec // G117: test fixture payload marshaling
	reqBadLogin := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/auth/login", bytes.NewReader(badLoginBody))
	recBadLogin := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBadLogin, reqBadLogin)

	if recBadLogin.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for bad login, got: %d", recBadLogin.Code)
	}

	// 5. Login with nonexistent username (timing mitigation test)
	noUserPayload := LoginRequest{
		Username: "nonexistent_user",
		Password: "SomePassword123!",
	}
	noUserBody, _ := json.Marshal(noUserPayload) //nolint:gosec // G117: test fixture payload marshaling
	reqNoUser := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/auth/login", bytes.NewReader(noUserBody))
	recNoUser := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recNoUser, reqNoUser)
	if recNoUser.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for nonexistent user, got: %d", recNoUser.Code)
	}
}

func TestAPI_GetProfile_AuthenticationAndRetrieval(t *testing.T) {
	srv, d := setupTestServer(t)

	// Seed a profile
	hash, _ := security.HashPassword("pass")
	p := &models.UserProfile{
		Name:  "Ada Lovelace",
		Phone: "4420123456",
		Address: models.Address{
			Locality: "London",
			Country:  "UK",
		},
	}
	c := &models.UserCredential{
		Username:     "ada",
		PasswordHash: hash,
	}
	userID, err := d.CreateUser(context.Background(), p, c)
	if err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}

	// 1. Missing Authorization header
	reqNoAuth := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles/"+userID, nil)
	recNoAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recNoAuth, reqNoAuth)

	if recNoAuth.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for missing auth header, got: %d", recNoAuth.Code)
	}

	// 2. Invalid Bearer token
	reqBadAuth := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles/"+userID, nil)
	reqBadAuth.Header.Set("Authorization", "Bearer wrong-token")
	recBadAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBadAuth, reqBadAuth)

	if recBadAuth.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for invalid token, got: %d", recBadAuth.Code)
	}

	// 3. Valid Bearer token
	reqValid := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles/"+userID, nil)
	reqValid.Header.Set("Authorization", "Bearer "+testAuthSecret)
	recValid := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recValid, reqValid)

	if recValid.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for authenticated profile fetch, got: %d", recValid.Code)
	}

	var got models.UserProfile
	if err := json.Unmarshal(recValid.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode profile: %v", err)
	}
	if got.Name != "Ada Lovelace" {
		t.Errorf("expected 'Ada Lovelace', got %q", got.Name)
	}

	// 4. Non-existent profile ID with valid token
	reqNotFound := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles/non-existent", nil)
	reqNotFound.Header.Set("Authorization", "Bearer "+testAuthSecret)
	recNotFound := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recNotFound, reqNotFound)

	if recNotFound.Code != http.StatusNotFound {
		t.Errorf("expected 404 Not Found for non-existent profile, got: %d", recNotFound.Code)
	}

	// 5. Blank profile ID
	reqBlank := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles/%20", nil)
	reqBlank.Header.Set("Authorization", "Bearer "+testAuthSecret)
	recBlank := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBlank, reqBlank)

	if recBlank.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for blank profile id, got: %d", recBlank.Code)
	}
}

func TestAPI_SearchProfiles(t *testing.T) {
	srv, d := setupTestServer(t)

	// Seed users
	hash, _ := security.HashPassword("pass")
	users := []struct {
		name     string
		phone    string
		locality string
		country  string
		username string
	}{
		{"Nikola Tesla", "1234567890", "New York", "USA", "tesla"},
		{"Thomas Edison", "9876543210", "West Orange", "USA", "edison"},
	}

	for _, u := range users {
		p := &models.UserProfile{
			Name:  u.name,
			Phone: u.phone,
			Address: models.Address{
				Locality: u.locality,
				Country:  u.country,
			},
		}
		c := &models.UserCredential{Username: u.username, PasswordHash: hash}
		_, _ = d.CreateUser(context.Background(), p, c)
	}

	// Search by name
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles?name=Tesla&limit=10&offset=0", nil)
	req.Header.Set("Authorization", "Bearer "+testAuthSecret)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for search, got: %d", rec.Code)
	}

	var searchResp SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &searchResp); err != nil {
		t.Fatalf("failed to decode search response: %v", err)
	}
	if searchResp.Count != 1 || searchResp.Data[0].Name != "Nikola Tesla" {
		t.Errorf("unexpected search response: %+v", searchResp)
	}

	// Search by phone and locality with offset
	reqPhone := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles?phone=9876&locality=West%20Orange&limit=5&offset=0", nil)
	reqPhone.Header.Set("Authorization", "Bearer "+testAuthSecret)
	recPhone := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recPhone, reqPhone)

	if recPhone.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for phone search, got: %d", recPhone.Code)
	}

	// Search with limit > 100 is clamped to 100
	reqClamped := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles?limit=500", nil)
	reqClamped.Header.Set("Authorization", "Bearer "+testAuthSecret)
	recClamped := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recClamped, reqClamped)

	if recClamped.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for clamped limit search, got: %d", recClamped.Code)
	}
	var clampedResp SearchResponse
	_ = json.Unmarshal(recClamped.Body.Bytes(), &clampedResp)
	if clampedResp.Limit != 100 {
		t.Errorf("expected limit to be clamped to 100, got: %d", clampedResp.Limit)
	}
}

func TestAPI_ValidationErrorsAndPanics(t *testing.T) {
	srv, _ := setupTestServer(t)

	// 1. Invalid JSON body
	reqBadJSON := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader([]byte("{invalid-json")))
	recBadJSON := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBadJSON, reqBadJSON)

	if recBadJSON.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for malformed JSON, got: %d", recBadJSON.Code)
	}

	// 2. Missing fields in user creation
	reqEmptyFields := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader([]byte("{}")))
	recEmptyFields := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recEmptyFields, reqEmptyFields)

	if recEmptyFields.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for missing fields, got: %d", recEmptyFields.Code)
	}

	// 2b. Missing address fields
	reqMissingAddr := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader([]byte(`{"name":"A","phone":"1","username":"u","password":"p"}`)))
	recMissingAddr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recMissingAddr, reqMissingAddr)
	if recMissingAddr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for missing address, got: %d", recMissingAddr.Code)
	}

	// 3. Rate limiter rejection
	strictConfig := Config{
		Port:           8080,
		RateLimitRPS:   1,
		RateLimitBurst: 1,
	}
	strictSrv, err := NewServer(nil, strictConfig, nil)
	if err != nil {
		t.Fatalf("unexpected error creating strict server: %v", err)
	}
	strictReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/openapi.yaml", nil)
	strictReq.RemoteAddr = "192.168.1.100:12345"

	// First request succeeds
	rec1 := httptest.NewRecorder()
	strictSrv.Handler().ServeHTTP(rec1, strictReq)
	if rec1.Code != http.StatusOK {
		t.Errorf("expected 200 for first request, got: %d", rec1.Code)
	}

	// Second immediate request gets rate limited
	rec2 := httptest.NewRecorder()
	strictSrv.Handler().ServeHTTP(rec2, strictReq)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 Too Many Requests for rate limited request, got: %d", rec2.Code)
	}

	// Health liveness probe is exempt from rate limiting even after client tokens are exhausted
	healthReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/health", nil)
	healthReq.RemoteAddr = "192.168.1.100:12345"
	healthRec := httptest.NewRecorder()
	strictSrv.Handler().ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Errorf("expected 200 OK for health probe even when client rate limit is exhausted, got: %d", healthRec.Code)
	}

	// Readiness probe hits the database and is subject to rate limiting for external IPs to prevent database DoS
	readyReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/ready", nil)
	readyReq.RemoteAddr = "192.168.1.100:12345"
	readyRec := httptest.NewRecorder()
	strictSrv.Handler().ServeHTTP(readyRec, readyReq)
	if readyRec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 for readiness probe when client tokens are exhausted, got: %d", readyRec.Code)
	}

	// Readiness probe for local loopback (e.g. Docker container healthcheck) is exempt from rate limiting
	localReadyReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/ready", nil)
	localReadyReq.RemoteAddr = "127.0.0.1:54321"
	localReadyRec := httptest.NewRecorder()
	strictSrv.Handler().ServeHTTP(localReadyRec, localReadyReq)
	if localReadyRec.Code == http.StatusTooManyRequests {
		t.Errorf("expected local readiness probe to be exempt from rate limiting, got 429")
	}

	// Verify dynamic Retry-After header with low RPS
	lowRPSSrv, err := NewServer(nil, Config{
		Port:           8080,
		RateLimitRPS:   0.1, // 1 token per 10 seconds
		RateLimitBurst: 1,
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error creating low RPS server: %v", err)
	}
	lowRPSReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/openapi.yaml", nil)
	lowRPSReq.RemoteAddr = "10.0.0.1:12345"
	recLow1 := httptest.NewRecorder()
	lowRPSSrv.Handler().ServeHTTP(recLow1, lowRPSReq)
	if recLow1.Code != http.StatusOK {
		t.Fatalf("expected 200 for initial request, got: %d", recLow1.Code)
	}
	recLow2 := httptest.NewRecorder()
	lowRPSSrv.Handler().ServeHTTP(recLow2, lowRPSReq)
	if recLow2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for rate limited request, got: %d", recLow2.Code)
	}
	retryAfterStr := recLow2.Header().Get("Retry-After")
	retryAfterVal, err := strconv.Atoi(retryAfterStr)
	if err != nil || retryAfterVal < 9 || retryAfterVal > 11 {
		t.Errorf("expected Retry-After to be approx 10 seconds for 0.1 RPS, got: %q", retryAfterStr)
	}

	// 3b. Rate limiter burst normalization (burst < 1 normalized to at least 1)
	subZeroLimiter := NewRateLimiter(10, 0)
	if subZeroLimiter.burst < 1 {
		t.Errorf("expected burst to be normalized to >= 1, got %f", subZeroLimiter.burst)
	}
	if !subZeroLimiter.Allow("1.2.3.4") {
		t.Error("expected first request with normalized burst to be allowed")
	}

	// 3c. Rate limiter LRU bounded eviction on maxClients capacity
	lruLimiter := NewRateLimiter(10, 5)
	for i := 0; i < maxClients; i++ {
		lruLimiter.Allow(fmt.Sprintf("client-%d", i))
	}
	// Re-access client-0 to move it to front, making client-1 the oldest
	lruLimiter.Allow("client-0")

	// Verify that adding a new client at capacity evicts the oldest client-1
	if !lruLimiter.Allow("new-client") {
		t.Error("expected new client to be allowed via LRU eviction when at max capacity")
	}
	if _, exists := lruLimiter.clients["client-1"]; exists {
		t.Error("expected oldest client-1 to have been evicted")
	}
	if _, exists := lruLimiter.clients["client-0"]; !exists {
		t.Error("expected re-accessed client-0 to remain in limiter")
	}
	if _, exists := lruLimiter.clients["new-client"]; !exists {
		t.Error("expected new-client to be present in limiter map")
	}
	if len(lruLimiter.clients) > maxClients {
		t.Errorf("limiter map exceeded maxClients: %d", len(lruLimiter.clients))
	}

	// Test responseWriterInterceptor Write without prior WriteHeader
	recDirect := httptest.NewRecorder()
	directInterceptor := &responseWriterInterceptor{ResponseWriter: recDirect}
	n, err := directInterceptor.Write([]byte("direct body"))
	if err != nil || n != 11 {
		t.Errorf("unexpected write result: %d, %v", n, err)
	}
	if directInterceptor.statusCode != http.StatusOK {
		t.Errorf("expected 200 default status code, got: %d", directInterceptor.statusCode)
	}

	// Test NewServer TrustedProxies parsing for single IPs and invalid CIDRs
	ipParseCfg := Config{
		Port:           8080,
		TrustedProxies: []string{"192.168.1.1", "::1"},
	}
	if _, err := NewServer(nil, ipParseCfg, nil); err != nil {
		t.Fatalf("expected valid server for single IPs, got err: %v", err)
	}

	invalidParseCfg := Config{
		Port:           8080,
		TrustedProxies: []string{"invalid-cidr"},
	}
	if _, err := NewServer(nil, invalidParseCfg, nil); err == nil {
		t.Error("expected error for invalid CIDR in TrustedProxies, got nil")
	}

	invalidIPCfg := Config{
		Port:           8080,
		TrustedProxies: []string{"999.999.999.999"},
	}
	if _, err := NewServer(nil, invalidIPCfg, nil); err == nil {
		t.Error("expected error for invalid IP in TrustedProxies, got nil")
	}

	// 3d. TrustedProxies rate limiting test
	proxyCfg := Config{
		Port:           8080,
		AuthSecret:     "secret",
		RateLimitRPS:   1,
		RateLimitBurst: 1,
		TrustedProxies: []string{"10.0.0.0/8"},
	}
	proxySrv, err := NewServer(nil, proxyCfg, nil)
	if err != nil {
		t.Fatalf("unexpected error creating proxy server: %v", err)
	}
	proxyReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/openapi.yaml", nil)
	proxyReq.RemoteAddr = "10.0.0.1:12345" // proxy peer IP inside trusted 10.0.0.0/8
	proxyReq.Header.Set("X-Forwarded-For", "203.0.113.195, 10.0.0.1")

	recP1 := httptest.NewRecorder()
	proxySrv.Handler().ServeHTTP(recP1, proxyReq)
	if recP1.Code != http.StatusOK {
		t.Errorf("expected 200 for first proxy request, got: %d", recP1.Code)
	}

	// Different originating client behind same proxy IP should have its own bucket
	proxyReq2 := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/openapi.yaml", nil)
	proxyReq2.RemoteAddr = "10.0.0.1:12345"
	proxyReq2.Header.Set("X-Forwarded-For", "198.51.100.22, 10.0.0.1")

	recP2 := httptest.NewRecorder()
	proxySrv.Handler().ServeHTTP(recP2, proxyReq2)
	if recP2.Code != http.StatusOK {
		t.Errorf("expected 200 for distinct client behind same proxy, got: %d", recP2.Code)
	}

	// Untrusted peer sending spoofed X-Forwarded-For must NOT be trusted
	untrustedReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/openapi.yaml", nil)
	untrustedReq.RemoteAddr = "192.0.2.1:12345" // not in 10.0.0.0/8
	untrustedReq.Header.Set("X-Forwarded-For", "203.0.113.99")
	if client := proxySrv.clientIP(untrustedReq); client != "192.0.2.1" {
		t.Errorf("expected untrusted peer IP '192.0.2.1' ignoring spoofed header, got %q", client)
	}

	// X-Real-IP behind trusted proxy
	realIPReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/openapi.yaml", nil)
	realIPReq.RemoteAddr = "10.0.0.1:12345"
	realIPReq.Header.Set("X-Real-IP", "198.51.100.99")
	if client := proxySrv.clientIP(realIPReq); client != "198.51.100.99" {
		t.Errorf("expected client IP '198.51.100.99' from X-Real-IP, got %q", client)
	}

	// X-Real-IP with invalid format should be ignored and fall back to peer IP
	invalidRealIPReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/openapi.yaml", nil)
	invalidRealIPReq.RemoteAddr = "10.0.0.1:12345"
	invalidRealIPReq.Header.Set("X-Real-IP", "not-a-valid-ip")
	if client := proxySrv.clientIP(invalidRealIPReq); client != "10.0.0.1" {
		t.Errorf("expected fallback to peer IP '10.0.0.1' for invalid X-Real-IP, got %q", client)
	}

	// X-Forwarded-For right-to-left traversal: ignore spoofed left hops
	// Format: <attacker_spoofed>, <real_client>, <intermediate_trusted_proxy>
	// RemoteAddr: <edge_trusted_proxy> (10.0.0.1)
	spoofedXFFReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/openapi.yaml", nil)
	spoofedXFFReq.RemoteAddr = "10.0.0.1:12345"
	spoofedXFFReq.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.195, 10.0.0.2")
	if client := proxySrv.clientIP(spoofedXFFReq); client != "203.0.113.195" {
		t.Errorf("expected first untrusted right-to-left IP '203.0.113.195', got %q", client)
	}

	// RateLimiter idle pruning during Allow
	idleLimiter := NewRateLimiter(10, 5)
	idleLimiter.Allow("idle-client")
	idleLimiter.lastCleanup = time.Now().Add(-2 * time.Minute)
	idleLimiter.clients["idle-client"].lastUpdate = time.Now().Add(-10 * time.Minute)
	idleLimiter.Allow("active-client")
	if _, exists := idleLimiter.clients["idle-client"]; exists {
		t.Error("expected idle-client to be pruned during periodic cleanup")
	}

	// 4. Panic recovery test
	panicHandler := srv.WithRecovery(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("simulated fatal bug")
	}))
	panicReq := httptest.NewRequestWithContext(context.Background(), "GET", "/panic", nil)
	panicRec := httptest.NewRecorder()
	panicHandler.ServeHTTP(panicRec, panicReq)

	if panicRec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 Internal Server Error after panic recovery, got: %d", panicRec.Code)
	}

	// 5. Dev mode auth (explicit DevAuthBypass)
	devConfig := Config{Port: 8080, AuthSecret: "", DevAuthBypass: true}
	devSrv, err := NewServer(nil, devConfig, nil)
	if err != nil {
		t.Fatalf("unexpected error creating dev server: %v", err)
	}
	devHandler := devSrv.WithAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	devReq := httptest.NewRequestWithContext(context.Background(), "GET", "/test", nil)
	devRec := httptest.NewRecorder()
	devHandler.ServeHTTP(devRec, devReq)
	if devRec.Code != http.StatusOK {
		t.Errorf("expected 200 OK in dev mode with DevAuthBypass, got: %d", devRec.Code)
	}

	// 5b. Fail-closed: empty AuthSecret WITHOUT DevAuthBypass must reject unauthenticated requests
	failClosedConfig := Config{Port: 8080, AuthSecret: "", DevAuthBypass: false}
	failClosedSrv, err := NewServer(nil, failClosedConfig, nil)
	if err != nil {
		t.Fatalf("unexpected error creating fail-closed server: %v", err)
	}
	failClosedHandler := failClosedSrv.WithAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	failClosedReq := httptest.NewRequestWithContext(context.Background(), "GET", "/test", nil)
	failClosedRec := httptest.NewRecorder()
	failClosedHandler.ServeHTTP(failClosedRec, failClosedReq)
	if failClosedRec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for empty secret without DevAuthBypass, got: %d", failClosedRec.Code)
	}

	// 6. Context timeout middleware
	timeoutMw := WithContextTimeout(10 * time.Millisecond)
	timeoutHandler := timeoutMw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok || deadline.IsZero() {
			t.Error("expected deadline set in request context")
		}
		w.WriteHeader(http.StatusOK)
	}))
	toReq := httptest.NewRequestWithContext(context.Background(), "GET", "/timeout", nil)
	toRec := httptest.NewRecorder()
	timeoutHandler.ServeHTTP(toRec, toReq)
	if toRec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got: %d", toRec.Code)
	}

	// 7. handleLogin validation edge cases
	badLoginJSON := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/auth/login", bytes.NewReader([]byte("{bad-json")))
	badLoginRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(badLoginRec, badLoginJSON)
	if badLoginRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad login JSON, got: %d", badLoginRec.Code)
	}

	emptyLoginFields := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/auth/login", bytes.NewReader([]byte("{}")))
	emptyLoginRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(emptyLoginRec, emptyLoginFields)
	if emptyLoginRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty login fields, got: %d", emptyLoginRec.Code)
	}

	// 8. handleCreateUser partial validation fields
	missingUserPass := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader([]byte(`{"name":"A","phone":"123"}`)))
	missingUserPassRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(missingUserPassRec, missingUserPass)
	if missingUserPassRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing user/pass, got: %d", missingUserPassRec.Code)
	}

	missingPhone := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader([]byte(`{"name":"A","username":"u","password":"p"}`)))
	missingPhoneRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(missingPhoneRec, missingPhone)
	if missingPhoneRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing phone, got: %d", missingPhoneRec.Code)
	}
}

func TestRateLimiter_Eviction(t *testing.T) {
	rl := NewRateLimiter(10, 10)
	// Seed client with old timestamp
	rl.clients["old_ip"] = &clientLimiter{
		tokens:     5,
		lastUpdate: time.Now().Add(-10 * time.Minute),
	}
	rl.lastCleanup = time.Now().Add(-2 * time.Minute)

	// Calling Allow triggers eviction of idle clients
	if !rl.Allow("new_ip") {
		t.Error("expected new_ip to be allowed")
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()
	if _, exists := rl.clients["old_ip"]; exists {
		t.Error("expected old_ip to be evicted after 10m idle")
	}
}

func TestOpenAPISpecInSync(t *testing.T) {
	canonical, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("failed to read canonical docs/openapi.yaml: %v", err)
	}
	if string(canonical) != string(openAPISpec) {
		t.Error("internal/api/openapi.yaml is out of sync with docs/openapi.yaml; run 'make sync-openapi'")
	}
}

func TestAPI_PasswordLimits(t *testing.T) {
	srv, _ := setupTestServer(t)

	overlongPass := strings.Repeat("A", 73)

	// 1. Registration with overlong password
	regPayload := map[string]any{
		"name":  "Long Pass",
		"phone": "3035559999",
		"address": map[string]string{
			"street_address": "123 Main St",
			"locality":       "Denver",
			"region":         "CO",
			"postal_code":    "80202",
			"country":        "USA",
		},
		"username": "longpassuser",
		"password": overlongPass,
	}
	body, _ := json.Marshal(regPayload)
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for overlong password in registration, got: %d", rec.Code)
	}

	// 2. Login with overlong password
	loginPayload := map[string]string{
		"username": "someuser",
		"password": overlongPass,
	}
	loginBody, _ := json.Marshal(loginPayload)
	loginReq := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/auth/login", bytes.NewReader(loginBody))
	loginRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(loginRec, loginReq)

	if loginRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for overlong password in login, got: %d", loginRec.Code)
	}
}

func TestAPI_FieldLengthLimits(t *testing.T) {
	srv, _ := setupTestServer(t)

	basePayload := func() map[string]any {
		return map[string]any{
			"name":  "Valid Name",
			"phone": "3035559999",
			"address": map[string]string{
				"street_address": "123 Main St",
				"locality":       "Denver",
				"region":         "CO",
				"postal_code":    "80202",
				"country":        "USA",
			},
			"username": "validuser",
			"password": "validpassword",
		}
	}

	// 1. Username > 128 characters
	p1 := basePayload()
	p1["username"] = strings.Repeat("u", 129)
	b1, _ := json.Marshal(p1)
	req1 := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader(b1))
	rec1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for username > 128 chars, got: %d", rec1.Code)
	}

	// 2. Name > 255 characters
	p2 := basePayload()
	p2["name"] = strings.Repeat("n", 256)
	b2, _ := json.Marshal(p2)
	req2 := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader(b2))
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for name > 255 chars, got: %d", rec2.Code)
	}

	// 3. Phone > 64 characters
	p3 := basePayload()
	p3["phone"] = strings.Repeat("1", 65)
	b3, _ := json.Marshal(p3)
	req3 := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader(b3))
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for phone > 64 chars, got: %d", rec3.Code)
	}

	// 3b. Address field length limits (e.g. locality > 128)
	p3b := basePayload()
	addr := p3b["address"].(map[string]string)
	addr["locality"] = strings.Repeat("L", 129)
	b3b, _ := json.Marshal(p3b)
	req3b := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader(b3b))
	rec3b := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3b, req3b)
	if rec3b.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for locality > 128 chars, got: %d", rec3b.Code)
	}

	// 4. Offset > 10000 clamped
	reqSearch := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles?offset=999999", nil)
	reqSearch.Header.Set("Authorization", "Bearer "+testAuthSecret)
	recSearch := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recSearch, reqSearch)
	if recSearch.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for search with offset, got: %d", recSearch.Code)
	}
	var searchResp SearchResponse
	_ = json.NewDecoder(recSearch.Body).Decode(&searchResp)
	if searchResp.Offset != 10000 {
		t.Errorf("expected offset to be clamped to 10000, got: %d", searchResp.Offset)
	}

	// 5. Non-ASCII characters (e.g. 100 Japanese characters = 300 bytes, which exceeds 128 bytes but <= 128 runes for locality)
	p5 := basePayload()
	p5["username"] = "unicode_user_test"
	p5["address"].(map[string]string)["locality"] = strings.Repeat("東", 100)
	b5, _ := json.Marshal(p5)
	req5 := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader(b5))
	rec5 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec5, req5)
	if rec5.Code != http.StatusCreated {
		t.Errorf("expected 201 Created for 100-character multi-byte locality (300 bytes), got: %d", rec5.Code)
	}
}

func TestAPI_MaxRequestBodySize(t *testing.T) {
	srv, _ := setupTestServer(t)

	// Create an oversized body (> 1MB)
	oversized := make([]byte, 1<<20+1024)
	for i := range oversized {
		oversized[i] = 'a'
	}

	req := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader(oversized))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for oversized payload in registration, got: %d", rec.Code)
	}

	loginReq := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/auth/login", bytes.NewReader(oversized))
	loginRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(loginRec, loginReq)

	if loginRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for oversized payload in login, got: %d", loginRec.Code)
	}
}

func TestAPI_UserTokenAuthentication(t *testing.T) {
	srv, _ := setupTestServer(t)

	// 1. Create a user
	regPayload := map[string]any{
		"name":  "Token User",
		"phone": "3035558888",
		"address": map[string]string{
			"street_address": "456 Test Ave",
			"locality":       "Boulder",
			"region":         "CO",
			"postal_code":    "80302",
			"country":        "USA",
		},
		"username": "tokenuser",
		"password": "ValidPassword123!",
	}
	body, _ := json.Marshal(regPayload)
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("failed to create user: %d", rec.Code)
	}

	var regResp CreateUserResponse
	_ = json.NewDecoder(rec.Body).Decode(&regResp)
	userID := regResp.UserID

	// 2. Login and get per-user token
	loginPayload := map[string]string{
		"username": "tokenuser",
		"password": "ValidPassword123!",
	}
	loginBody, _ := json.Marshal(loginPayload)
	loginReq := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/auth/login", bytes.NewReader(loginBody))
	loginRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(loginRec, loginReq)

	if loginRec.Code != http.StatusOK {
		t.Fatalf("failed to login: %d", loginRec.Code)
	}

	var loginResp LoginResponse
	_ = json.NewDecoder(loginRec.Body).Decode(&loginResp)
	userToken := loginResp.AccessToken

	// Verify token is NOT the master secret
	if userToken == testAuthSecret {
		t.Error("expected user token to be uniquely signed, not the server master secret")
	}

	// 3. Access profile with user token
	profileReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles/"+userID, nil)
	profileReq.Header.Set("Authorization", "Bearer "+userToken)
	profileRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(profileRec, profileReq)

	if profileRec.Code != http.StatusOK {
		t.Errorf("expected 200 OK using user token, got: %d", profileRec.Code)
	}

	// 4. Access profile with master service secret (fallback)
	masterReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles/"+userID, nil)
	masterReq.Header.Set("Authorization", "Bearer "+testAuthSecret)
	masterRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(masterRec, masterReq)

	if masterRec.Code != http.StatusOK {
		t.Errorf("expected 200 OK using master secret, got: %d", masterRec.Code)
	}

	// 5. Access profile with expired token
	expiredToken, _ := security.GenerateUserToken(userID, "tokenuser", testAuthSecret, -1*time.Minute)
	expiredReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles/"+userID, nil)
	expiredReq.Header.Set("Authorization", "Bearer "+expiredToken)
	expiredRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(expiredRec, expiredReq)

	if expiredRec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for expired token, got: %d", expiredRec.Code)
	}
}

func TestAPI_CreateUser_AddressValidation(t *testing.T) {
	srv, _ := setupTestServer(t)

	// Missing street_address
	payload := map[string]any{
		"name":  "Address Tester",
		"phone": "3035550000",
		"address": map[string]string{
			"street_address": "",
			"locality":       "Denver",
			"region":         "CO",
			"postal_code":    "80202",
			"country":        "USA",
		},
		"username": "addresstester",
		"password": "ValidPassword123!",
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for missing street_address, got: %d", rec.Code)
	}

	// Blank login fields
	loginPayload := map[string]string{
		"username": "",
		"password": "",
	}
	loginBody, _ := json.Marshal(loginPayload)
	loginReq := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/auth/login", bytes.NewReader(loginBody))
	loginRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(loginRec, loginReq)

	if loginRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for blank login fields, got: %d", loginRec.Code)
	}
}

func TestAPI_SearchProfiles_LimitClamping(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles?limit=500&offset=5", nil)
	req.Header.Set("Authorization", "Bearer "+testAuthSecret)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got: %d", rec.Code)
	}

	var resp SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Limit != 100 {
		t.Errorf("expected limit clamped to 100, got: %d", resp.Limit)
	}
	if resp.Offset != 5 {
		t.Errorf("expected offset 5, got: %d", resp.Offset)
	}

	// Test lower bound clamping: limit=0 or limit=-5 should clamp to 1
	for _, lowLimit := range []string{"0", "-5"} {
		reqLow := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles?limit="+lowLimit, nil)
		reqLow.Header.Set("Authorization", "Bearer "+testAuthSecret)
		recLow := httptest.NewRecorder()
		srv.Handler().ServeHTTP(recLow, reqLow)

		if recLow.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for limit=%s, got: %d", lowLimit, recLow.Code)
		}
		var lowResp SearchResponse
		if err := json.Unmarshal(recLow.Body.Bytes(), &lowResp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if lowResp.Limit != 1 {
			t.Errorf("expected limit clamped to 1 for limit=%s, got: %d", lowLimit, lowResp.Limit)
		}
	}
}

func TestAPI_TrailingPayloadContent(t *testing.T) {
	srv, _ := setupTestServer(t)

	// 1. Trailing content on registration
	trailingUserJSON := `{"name":"A","phone":"1","address":{"street_address":"1","locality":"L","region":"R","postal_code":"1","country":"C"},"username":"u","password":"p"} extra_junk`
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", strings.NewReader(trailingUserJSON))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for trailing data in registration, got: %d", rec.Code)
	}

	// 2. Trailing content on login
	trailingLoginJSON := `{"username":"u","password":"p"} extra_junk`
	loginReq := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/auth/login", strings.NewReader(trailingLoginJSON))
	loginRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(loginRec, loginReq)

	if loginRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for trailing data in login, got: %d", loginRec.Code)
	}

	// 3. Second JSON object on registration
	secondObjUserJSON := `{"name":"A","phone":"1","address":{"street_address":"1","locality":"L","region":"R","postal_code":"1","country":"C"},"username":"u2","password":"p"}{"extra":1}`
	req2 := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/users", strings.NewReader(secondObjUserJSON))
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for second JSON object in registration, got: %d", rec2.Code)
	}

	// 4. Second JSON object on login
	secondObjLoginJSON := `{"username":"u","password":"p"}{"extra":1}`
	loginReq2 := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/auth/login", strings.NewReader(secondObjLoginJSON))
	loginRec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(loginRec2, loginReq2)
	if loginRec2.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for second JSON object in login, got: %d", loginRec2.Code)
	}
}

func TestAPI_CrossUserAuthorization(t *testing.T) {
	srv, d := setupTestServer(t)

	// Create User 1 (Alice)
	p1 := &models.UserProfile{
		Name:    "Alice",
		Phone:   "1111111111",
		Address: models.Address{StreetAddress: "1 St", Locality: "L", Region: "R", PostalCode: "1", Country: "USA"},
	}
	c1 := &models.UserCredential{Username: "alice", PasswordHash: "h"}
	id1, err := d.CreateUser(context.Background(), p1, c1)
	if err != nil {
		t.Fatalf("failed to create user 1: %v", err)
	}

	// Create User 2 (Bob)
	p2 := &models.UserProfile{
		Name:    "Bob",
		Phone:   "2222222222",
		Address: models.Address{StreetAddress: "2 St", Locality: "L", Region: "R", PostalCode: "2", Country: "USA"},
	}
	c2 := &models.UserCredential{Username: "bob", PasswordHash: "h"}
	id2, err := d.CreateUser(context.Background(), p2, c2)
	if err != nil {
		t.Fatalf("failed to create user 2: %v", err)
	}

	// Generate user token for Alice
	aliceToken, err := security.GenerateUserToken(id1, "alice", testAuthSecret, time.Hour)
	if err != nil {
		t.Fatalf("failed to generate user token: %v", err)
	}

	// Alice accesses her own profile -> 200 OK
	ownReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles/"+id1, nil)
	ownReq.Header.Set("Authorization", "Bearer "+aliceToken)
	ownRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(ownRec, ownReq)
	if ownRec.Code != http.StatusOK {
		t.Errorf("expected 200 OK for own profile access, got: %d", ownRec.Code)
	}

	// Alice attempts to access Bob's profile -> 403 Forbidden
	otherReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles/"+id2, nil)
	otherReq.Header.Set("Authorization", "Bearer "+aliceToken)
	otherRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(otherRec, otherReq)
	if otherRec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for cross-user profile access, got: %d", otherRec.Code)
	}

	// Alice attempts directory search -> 403 Forbidden
	searchReq := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/profiles?name=Bob", nil)
	searchReq.Header.Set("Authorization", "Bearer "+aliceToken)
	searchRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(searchRec, searchReq)
	if searchRec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for user token search, got: %d", searchRec.Code)
	}
}
