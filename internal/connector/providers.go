package connector

import (
	"fmt"
	"log/slog"
	"strings"
)

// ProviderType represents supported vendor types.
type ProviderType string

const (
	// ProviderABC identifies the ABC Identity Verification provider.
	ProviderABC ProviderType = "ABC"
	// ProviderXYZ identifies the XYZ Identity Verification provider.
	ProviderXYZ ProviderType = "XYZ"
)

// NewABCConnector constructs an IdentityConnector configured specifically for vendor ABC.
func NewABCConnector(cfg VendorConfig, logger *slog.Logger) IdentityConnector {
	cfg.ProviderName = string(ProviderABC)
	return NewBaseConnector(cfg, logger)
}

// NewXYZConnector constructs an IdentityConnector configured specifically for vendor XYZ.
func NewXYZConnector(cfg VendorConfig, logger *slog.Logger) IdentityConnector {
	cfg.ProviderName = string(ProviderXYZ)
	return NewBaseConnector(cfg, logger)
}

// NewConnector creates an identity provider connector based on the provider string name.
func NewConnector(provider string, cfg VendorConfig, logger *slog.Logger) (IdentityConnector, error) {
	switch strings.ToUpper(strings.TrimSpace(provider)) {
	case string(ProviderABC):
		return NewABCConnector(cfg, logger), nil
	case string(ProviderXYZ):
		return NewXYZConnector(cfg, logger), nil
	default:
		return nil, fmt.Errorf("unknown identity provider %q: supported providers are ABC, XYZ", provider)
	}
}
