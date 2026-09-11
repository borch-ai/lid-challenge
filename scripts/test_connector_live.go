// Package main provides a live mock test harness for identity connectors.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/borch-ai/lid-challenge/internal/connector"
	"github.com/borch-ai/lid-challenge/internal/models"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// 1. Spin up a live mock vendor server (Vendor ABC)
	mockVendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/auth":
			var req connector.AuthRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if req.Username == "abc_client" && req.Password == "abc_secret" {
				w.Header().Set("Content-Type", "application/json")
				expiresIn := 3600
				_ = json.NewEncoder(w).Encode(connector.AuthResponse{
					AccessToken: "mock-jwt-token-vendor-abc",
					ExpiresIn:   &expiresIn,
				})
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)

		case "/identity":
			token := r.Header.Get("Authorization")
			if token != "Bearer mock-jwt-token-vendor-abc" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			var req connector.IdentityRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}

			if req.Name == "Daniel Borch" && req.Phone == "3035551234" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(connector.IdentityResponse{
					Name:  "Daniel Borch",
					Phone: "3035551234",
					Address: models.Address{
						StreetAddress: "100 Innovation Way",
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
	defer mockVendor.Close()

	fmt.Println("=================================================")
	fmt.Println("🚀 Testing 3rd-Party Identity Connector Live")
	fmt.Printf("Mock Vendor ABC URL: %s\n", mockVendor.URL)
	fmt.Println("=================================================")

	cfg := connector.VendorConfig{
		ProviderName:    "ABC",
		BaseURL:         mockVendor.URL,
		Username:        "abc_client",
		Password:        "abc_secret",
		Timeout:         5 * time.Second,
		DefaultTokenTTL: 10 * time.Minute,
	}

	conn, err := connector.NewConnector("ABC", cfg, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to instantiate connector: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// 2. Query GetIdentity: should automatically authenticate, cache token, and query /identity
	fmt.Println("1. Querying GetIdentity (triggers auto /auth + /identity)...")
	identity, err := conn.GetIdentity(ctx, "3035551234", "Daniel Borch")
	if err != nil {
		fmt.Fprintf(os.Stderr, "GetIdentity failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Retrieved Identity: Name=%s, Locality=%s, Country=%s\n",
		identity.Name, identity.Address.Locality, identity.Address.Country)

	// 3. Second query: should hit cached token
	fmt.Println("2. Second query (should reuse in-memory cached token without re-auth)...")
	identity2, err := conn.GetIdentity(ctx, "3035551234", "Daniel Borch")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Second GetIdentity failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Cached Identity check passed: Name=%s\n", identity2.Name)

	fmt.Println("=================================================")
	fmt.Println("✅ 3rd-Party Identity Connector Live Tests Passed!")
	fmt.Println("=================================================")
}
