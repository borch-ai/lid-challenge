package config

import (
	"fmt"
	"testing"
)

func TestConfigLoad_Defaults(t *testing.T) {
	// Clear any overrides and set development environment
	t.Setenv("APP_ENV", "development")
	t.Setenv("SERVER_PORT", "")
	t.Setenv("AUTH_SECRET", "")
	t.Setenv("DB_DRIVER", "")
	t.Setenv("RATE_LIMIT_RPS", "")
	t.Setenv("DEBUG", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error loading config: %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.Server.Port)
	}
	if cfg.Server.AuthSecret != "dev-secret-token" {
		t.Errorf("expected default auth secret, got %q", cfg.Server.AuthSecret)
	}
	if cfg.DBDriver != "sqlite" {
		t.Errorf("expected default db driver 'sqlite', got %q", cfg.DBDriver)
	}
	if cfg.Debug != false {
		t.Errorf("expected default debug false, got %v", cfg.Debug)
	}
	if cfg.MigrateOnStartup != true {
		t.Errorf("expected default migrate on startup true, got %v", cfg.MigrateOnStartup)
	}
}

func TestConfigLoad_Overrides(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("SERVER_PORT", "9090")
	t.Setenv("AUTH_SECRET", "custom-secret")
	t.Setenv("DB_DRIVER", "postgres")
	t.Setenv("RATE_LIMIT_RPS", "123.45")
	t.Setenv("DEBUG", "true")
	t.Setenv("MIGRATE_ON_STARTUP", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error loading config: %v", err)
	}

	if cfg.MigrateOnStartup != false {
		t.Errorf("expected migrate on startup false, got %v", cfg.MigrateOnStartup)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Server.Port)
	}
	if cfg.Server.AuthSecret != "custom-secret" {
		t.Errorf("expected 'custom-secret', got %q", cfg.Server.AuthSecret)
	}
	if cfg.DBDriver != "postgres" {
		t.Errorf("expected 'postgres', got %q", cfg.DBDriver)
	}
	if cfg.Server.RateLimitRPS != 123.45 {
		t.Errorf("expected 123.45, got %f", cfg.Server.RateLimitRPS)
	}
	if cfg.Debug != true {
		t.Errorf("expected debug true, got %v", cfg.Debug)
	}
}

func TestConfigLoad_ProductionRequiresSecret(t *testing.T) {
	cases := []string{"production", "PRODUCTION", "Prod", "  production  ", "PROD"}
	for _, envVal := range cases {
		t.Run(envVal, func(t *testing.T) {
			t.Setenv("APP_ENV", envVal)
			t.Setenv("AUTH_SECRET", "")

			_, err := Load()
			if err == nil {
				t.Fatalf("expected error when AUTH_SECRET is omitted in %q", envVal)
			}
		})
	}
}

func TestConfigLoad_OmittedAppEnvFailsClosed(t *testing.T) {
	t.Setenv("APP_ENV", "")
	t.Setenv("AUTH_SECRET", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error when APP_ENV is omitted and AUTH_SECRET is not set")
	}
}

func TestConfigLoad_DBDriverNormalization(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	cases := []struct {
		input    string
		expected string
	}{
		{"POSTGRES", "postgres"},
		{"  PostgreSQL  ", "postgresql"},
		{"SQLite3  ", "sqlite3"},
		{"  CockroachDB", "cockroachdb"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Setenv("DB_DRIVER", tc.input)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.DBDriver != tc.expected {
				t.Errorf("expected DBDriver %q, got %q", tc.expected, cfg.DBDriver)
			}
		})
	}
}

func TestConfigLoad_RateLimitValidation(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	invalidFloats := []string{"NaN", "nan", "+Inf", "-Inf", "Inf", "-5.0", "-0.1", "abc"}
	for _, val := range invalidFloats {
		t.Run(val, func(t *testing.T) {
			t.Setenv("RATE_LIMIT_RPS", val)
			_, err := Load()
			if err == nil {
				t.Errorf("expected error for invalid rate limit %q, got nil", val)
			}
		})
	}

	t.Run("invalid_burst", func(t *testing.T) {
		t.Setenv("RATE_LIMIT_BURST", "bad_burst")
		_, err := Load()
		if err == nil {
			t.Error("expected error for invalid RATE_LIMIT_BURST, got nil")
		}
	})

	t.Run("valid_float", func(t *testing.T) {
		t.Setenv("RATE_LIMIT_RPS", "75.5")
		t.Setenv("RATE_LIMIT_BURST", "150.0")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error for valid float: %v", err)
		}
		if cfg.Server.RateLimitRPS != 75.5 {
			t.Errorf("expected 75.5, got %f", cfg.Server.RateLimitRPS)
		}
		if cfg.Server.RateLimitBurst != 150.0 {
			t.Errorf("expected 150.0, got %f", cfg.Server.RateLimitBurst)
		}
	})
}

func TestConfigLoad_NonDevRequiresVendorPasswords(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	t.Setenv("AUTH_SECRET", "staging-secret")

	// Missing VENDOR_ABC_PASSWORD
	t.Setenv("VENDOR_ABC_PASSWORD", "")
	t.Setenv("VENDOR_XYZ_PASSWORD", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error when VENDOR_ABC_PASSWORD is missing in staging")
	}

	// Missing VENDOR_XYZ_PASSWORD
	t.Setenv("VENDOR_ABC_PASSWORD", "abc-secret")
	_, err = Load()
	if err == nil {
		t.Fatal("expected error when VENDOR_XYZ_PASSWORD is missing in staging")
	}

	// All provided
	t.Setenv("VENDOR_XYZ_PASSWORD", "xyz-secret")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error when all secrets are provided in staging: %v", err)
	}
	if cfg.VendorABC.Password != "abc-secret" || cfg.VendorXYZ.Password != "xyz-secret" {
		t.Error("vendor passwords did not match provided values")
	}
}

func TestConfigLoad_PortValidation(t *testing.T) {
	t.Setenv("APP_ENV", "development")

	invalidPorts := []string{"0", "-1", "-8080", "65536", "70000", "99999", "abc", "8080abc", "!"}
	for _, p := range invalidPorts {
		t.Run("invalid_port_"+p, func(t *testing.T) {
			t.Setenv("SERVER_PORT", p)
			_, err := Load()
			if err == nil {
				t.Errorf("expected error for invalid port %s, got nil", p)
			}
		})
	}

	validPorts := []string{"1", "80", "8080", "65535"}
	for _, p := range validPorts {
		t.Run("valid_port_"+p, func(t *testing.T) {
			t.Setenv("SERVER_PORT", p)
			cfg, err := Load()
			if err != nil {
				t.Errorf("expected valid port %s to load without error, got: %v", p, err)
			}
			if fmt.Sprintf("%d", cfg.Server.Port) != p {
				t.Errorf("expected port %s, got %d", p, cfg.Server.Port)
			}
		})
	}
}

func TestConfigLoad_TrustedProxies(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("TRUSTED_PROXIES", "127.0.0.1/32, 10.0.0.0/8, 172.16.0.0/12 ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error loading config with trusted proxies: %v", err)
	}

	expected := []string{"127.0.0.1/32", "10.0.0.0/8", "172.16.0.0/12"}
	if len(cfg.Server.TrustedProxies) != len(expected) {
		t.Fatalf("expected %d trusted proxies, got %d", len(expected), len(cfg.Server.TrustedProxies))
	}
	for i, exp := range expected {
		if cfg.Server.TrustedProxies[i] != exp {
			t.Errorf("expected proxy %q at index %d, got %q", exp, i, cfg.Server.TrustedProxies[i])
		}
	}

	t.Run("invalid_ip", func(t *testing.T) {
		t.Setenv("APP_ENV", "development")
		t.Setenv("TRUSTED_PROXIES", "not-an-ip")
		if _, err := Load(); err == nil {
			t.Error("expected error for invalid IP in TRUSTED_PROXIES, got nil")
		}
	})

	t.Run("invalid_cidr", func(t *testing.T) {
		t.Setenv("APP_ENV", "development")
		t.Setenv("TRUSTED_PROXIES", "10.0.0.1/999")
		if _, err := Load(); err == nil {
			t.Error("expected error for invalid CIDR in TRUSTED_PROXIES, got nil")
		}
	})
}
