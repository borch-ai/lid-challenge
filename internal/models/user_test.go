package models

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestAddress_String(t *testing.T) {
	tests := []struct {
		name     string
		address  Address
		expected string
	}{
		{
			name: "full address",
			address: Address{
				StreetAddress: "123 Main St",
				Locality:      "Denver",
				Region:        "CO",
				PostalCode:    "80202",
				Country:       "USA",
			},
			expected: "123 Main St, Denver, CO, 80202, USA",
		},
		{
			name: "partial address",
			address: Address{
				Locality: "Denver",
				Country:  "USA",
			},
			expected: "Denver, USA",
		},
		{
			name:     "empty address",
			address:  Address{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.address.String(); got != tt.expected {
				t.Errorf("Address.String() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestMaskPhone(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"1234567890", "***-***-7890"},
		{"+1 (555) 123-4567", "***-***-4567"},
		{"1234", "****"},
		{"12", "****"},
		{"", "****"},
		{"+33 1 42 68 5555", "***-***-5555"},
		{"電話番号1234", "***-***-1234"},
	}

	for _, tt := range tests {
		if got := MaskPhone(tt.input); got != tt.expected {
			t.Errorf("MaskPhone(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestMaskString(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"abc", "***"},
		{"12", "***"},
		{"12345", "1***5"},
		{"80202", "8***2"},
		{"Élysée", "É****e"},
		{"東京都港区", "東***区"},
	}

	for _, tt := range tests {
		if got := maskString(tt.input); got != tt.expected {
			t.Errorf("maskString(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestLogValueMasking(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	now := time.Now()
	profile := UserProfile{
		ID:    "user-123",
		Name:  "Alice Smith",
		Phone: "3035551234",
		Address: Address{
			StreetAddress: "742 Evergreen Terrace",
			Locality:      "Springfield",
			Region:        "OR",
			PostalCode:    "97477",
			Country:       "USA",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	cred := UserCredential{
		UserID:       "user-123",
		Username:     "alicesmith",
		Method:       "bcrypt",
		PasswordHash: "$2a$10$supersecretstring",
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	idp := IdentityPII{
		Name:    "Alice Smith",
		Phone:   "3035551234",
		Address: profile.Address,
	}

	logger.Info("testing logs",
		slog.Any("profile", profile),
		slog.Any("credential", cred),
		slog.Any("identity", idp),
	)

	logOutput := buf.String()

	// Ensure secret password is never in log output
	if strings.Contains(logOutput, "supersecretstring") {
		t.Errorf("Log output contains plain password hash: %s", logOutput)
	}

	// Ensure phone is masked
	if strings.Contains(logOutput, "3035551234") {
		t.Errorf("Log output contains raw phone number: %s", logOutput)
	}
	if !strings.Contains(logOutput, "***-***-1234") {
		t.Errorf("Log output missing masked phone: %s", logOutput)
	}

	// Ensure street address is masked
	if strings.Contains(logOutput, "742 Evergreen Terrace") {
		t.Errorf("Log output contains raw street address: %s", logOutput)
	}

	// Ensure identity and profile names are masked
	if strings.Contains(logOutput, `"profile":{`+"\"id\":\"user-123\",\"name\":\"Alice Smith\"") {
		t.Errorf("Log output contains raw profile name: %s", logOutput)
	}
	if !strings.Contains(logOutput, `"profile":{`+"\"id\":\"user-123\",\"name\":\""+MaskString("Alice Smith")) {
		t.Errorf("Log output missing masked profile name: %s", logOutput)
	}
	if strings.Contains(logOutput, `"identity":{"name":"Alice Smith"`) {
		t.Errorf("Log output contains raw identity name: %s", logOutput)
	}
	if !strings.Contains(logOutput, `"identity":{"name":"`+MaskString("Alice Smith")) {
		t.Errorf("Log output missing masked identity name: %s", logOutput)
	}
}
