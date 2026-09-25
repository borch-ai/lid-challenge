// Package config loads and validates application configuration settings.
package config

import (
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/borch-ai/lid-challenge/internal/api"
	"github.com/borch-ai/lid-challenge/internal/connector"
)

// AppConfig contains all operational configuration parameters for the application.
type AppConfig struct {
	Server           api.Config
	DBDriver         string
	DBDSN            string
	Debug            bool
	MigrateOnStartup bool

	VendorABC connector.VendorConfig
	VendorXYZ connector.VendorConfig
}

// Load loads configuration from environment variables with sensible defaults.
func Load() (*AppConfig, error) {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	isDev := env == "development" || env == "dev" || env == "test"

	authSecret := os.Getenv("AUTH_SECRET")
	if authSecret == "" {
		if !isDev {
			return nil, fmt.Errorf("AUTH_SECRET must be explicitly provided (default fallback is only permitted when APP_ENV is explicitly set to development, dev, or test)")
		}
		authSecret = "dev-secret-token"
	}

	vendorABCPassword := os.Getenv("VENDOR_ABC_PASSWORD")
	if vendorABCPassword == "" {
		if !isDev {
			return nil, fmt.Errorf("VENDOR_ABC_PASSWORD must be explicitly provided in non-development environment")
		}
		vendorABCPassword = "secret_abc"
	}

	vendorXYZPassword := os.Getenv("VENDOR_XYZ_PASSWORD")
	if vendorXYZPassword == "" {
		if !isDev {
			return nil, fmt.Errorf("VENDOR_XYZ_PASSWORD must be explicitly provided in non-development environment")
		}
		vendorXYZPassword = "secret_xyz"
	}

	port, err := getEnvInt("SERVER_PORT", 8080)
	if err != nil {
		return nil, err
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("SERVER_PORT must be between 1 and 65535, got %d", port)
	}
	dbDriver := strings.ToLower(strings.TrimSpace(getEnv("DB_DRIVER", "sqlite")))
	dbDSN := getEnv("DB_DSN", "lid.db")
	rateLimitRPS, err := getEnvFloat("RATE_LIMIT_RPS", 50.0)
	if err != nil {
		return nil, err
	}
	rateLimitBurst, err := getEnvFloat("RATE_LIMIT_BURST", 100.0)
	if err != nil {
		return nil, err
	}

	trustedProxiesStr := getEnv("TRUSTED_PROXIES", "")
	var trustedProxies []string
	if strings.TrimSpace(trustedProxiesStr) != "" {
		for _, part := range strings.Split(trustedProxiesStr, ",") {
			p := strings.TrimSpace(part)
			if p != "" {
				cidr := p
				if !strings.Contains(p, "/") {
					ip := net.ParseIP(p)
					if ip == nil {
						return nil, fmt.Errorf("invalid TRUSTED_PROXIES IP %q: must be a valid IP address", p)
					}
					if ip.To4() != nil {
						cidr = p + "/32"
					} else {
						cidr = p + "/128"
					}
				}
				if _, _, err := net.ParseCIDR(cidr); err != nil {
					return nil, fmt.Errorf("invalid TRUSTED_PROXIES CIDR %q: %w", p, err)
				}
				trustedProxies = append(trustedProxies, p)
			}
		}
	}
	debug := getEnvBool("DEBUG", false)
	migrateOnStartup := getEnvBool("MIGRATE_ON_STARTUP", true)

	cfg := &AppConfig{
		Server: api.Config{
			Port:           port,
			AuthSecret:     authSecret,
			RateLimitRPS:   rateLimitRPS,
			RateLimitBurst: rateLimitBurst,
			TrustedProxies: trustedProxies,
		},
		DBDriver:         dbDriver,
		DBDSN:            dbDSN,
		Debug:            debug,
		MigrateOnStartup: migrateOnStartup,

		VendorABC: connector.VendorConfig{
			ProviderName:    "ABC",
			BaseURL:         getEnv("VENDOR_ABC_URL", "https://api.abc-identity.test"),
			Username:        getEnv("VENDOR_ABC_USERNAME", "client_abc"),
			Password:        vendorABCPassword,
			Timeout:         10 * time.Second,
			DefaultTokenTTL: 15 * time.Minute,
		},
		VendorXYZ: connector.VendorConfig{
			ProviderName:    "XYZ",
			BaseURL:         getEnv("VENDOR_XYZ_URL", "https://api.xyz-identity.test"),
			Username:        getEnv("VENDOR_XYZ_USERNAME", "client_xyz"),
			Password:        vendorXYZPassword,
			Timeout:         10 * time.Second,
			DefaultTokenTTL: 15 * time.Minute,
		},
	}

	return cfg, nil
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) (int, error) {
	if val := os.Getenv(key); val != "" {
		i, err := strconv.Atoi(val)
		if err != nil {
			return 0, fmt.Errorf("invalid integer for %s: %q", key, val)
		}
		return i, nil
	}
	return defaultVal, nil
}

func getEnvFloat(key string, defaultVal float64) (float64, error) {
	if val := os.Getenv(key); val != "" {
		f, err := strconv.ParseFloat(val, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			return 0, fmt.Errorf("invalid non-negative float for %s: %q", key, val)
		}
		return f, nil
	}
	return defaultVal, nil
}

func getEnvBool(key string, defaultVal bool) bool {
	if val := os.Getenv(key); val != "" {
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
	}
	return defaultVal
}
