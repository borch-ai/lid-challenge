package dao

import (
	"context"
	"errors"
	"testing"

	"github.com/borch-ai/lid-challenge/internal/models"
	"github.com/borch-ai/lid-challenge/internal/security"
)

func TestNoSQLDAO_Lifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("unexpected error creating NoSQLDAO: %v", err)
	}

	// Ping should succeed
	if err := store.Ping(ctx); err != nil {
		t.Fatalf("unexpected error pinging: %v", err)
	}

	// Migrate should succeed
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("unexpected error migrating: %v", err)
	}

	// MigratableDAO interface methods
	count, err := store.MigrateUp(ctx)
	if err != nil || count != 1 {
		t.Fatalf("expected MigrateUp to succeed with count 1, got %d, err: %v", count, err)
	}

	downCount, err := store.MigrateDown(ctx, 1)
	if err != nil || downCount != 0 {
		t.Fatalf("expected MigrateDown to succeed with 0, got %d, err: %v", downCount, err)
	}

	ver, err := store.MigrationVersion(ctx)
	if err != nil || ver != 1 {
		t.Fatalf("expected MigrationVersion 1, got %d, err: %v", ver, err)
	}

	statuses, err := store.MigrationStatus(ctx)
	if err != nil || len(statuses) != 1 || !statuses[0].Applied {
		t.Fatalf("expected 1 applied migration status, got %v, err: %v", statuses, err)
	}

	// Context cancellations
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Ping(canceledCtx); err == nil {
		t.Error("expected error with canceled context on Ping")
	}
	if err := store.Migrate(canceledCtx); err == nil {
		t.Error("expected error with canceled context on Migrate")
	}
	if _, err := store.MigrateDown(canceledCtx, 1); err == nil {
		t.Error("expected error with canceled context on MigrateDown")
	}
	if _, err := store.MigrationVersion(canceledCtx); err == nil {
		t.Error("expected error with canceled context on MigrationVersion")
	}
	if _, err := store.MigrationStatus(canceledCtx); err == nil {
		t.Error("expected error with canceled context on MigrationStatus")
	}

	// Close
	if err := store.Close(); err != nil {
		t.Fatalf("unexpected error closing store: %v", err)
	}

	// Closed store operations should fail
	if err := store.Ping(ctx); err == nil {
		t.Error("expected error pinging closed store")
	}
	if err := store.Migrate(ctx); err == nil {
		t.Error("expected error migrating closed store")
	}
	if _, err := store.MigrateUp(ctx); err == nil {
		t.Error("expected error migrating up closed store")
	}
	if _, err := store.MigrateDown(ctx, 1); err == nil {
		t.Error("expected error migrating down closed store")
	}
	if _, err := store.MigrationVersion(ctx); err == nil {
		t.Error("expected error getting version of closed store")
	}
	if _, err := store.MigrationStatus(ctx); err == nil {
		t.Error("expected error getting status of closed store")
	}
}

func TestNoSQLDAO_CreateAndRetrieve(t *testing.T) {
	ctx := context.Background()
	store, err := NewNoSQLDAO("memory://")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()

	hash, err := security.HashPassword("secure-password123")
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	profile := &models.UserProfile{
		ID:    "user-123",
		Name:  "Alice Wonderland",
		Phone: "+1-555-0100",
		Address: models.Address{
			StreetAddress: "123 Magic Way",
			Locality:      "Wonderland",
			Region:        "Fantasy",
			PostalCode:    "99999",
			Country:       "US",
		},
	}
	cred := &models.UserCredential{
		Username:     "alice_wonder",
		PasswordHash: hash,
	}

	// Test nil inputs
	if _, err := store.CreateUser(ctx, nil, cred); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
	if _, err := store.CreateUser(ctx, profile, nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
	if _, err := store.CreateUser(ctx, profile, &models.UserCredential{Username: "   "}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty username, got %v", err)
	}

	// Create user with explicit ID
	id, err := store.CreateUser(ctx, profile, cred)
	if err != nil {
		t.Fatalf("unexpected error creating user: %v", err)
	}
	if id != "user-123" {
		t.Errorf("expected ID 'user-123', got %q", id)
	}

	// Duplicate username should fail
	dupProfile := &models.UserProfile{Name: "Alice Twin"}
	if _, err := store.CreateUser(ctx, dupProfile, cred); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("expected ErrUsernameTaken, got %v", err)
	}

	// Create user with generated UUID
	prof2 := &models.UserProfile{Name: "Bob Builder"}
	cred2 := &models.UserCredential{Username: "bob_builder", PasswordHash: hash}
	id2, err := store.CreateUser(ctx, prof2, cred2)
	if err != nil {
		t.Fatalf("unexpected error creating second user: %v", err)
	}
	if id2 == "" {
		t.Errorf("expected non-empty generated ID")
	}

	// GetProfile tests
	retrieved, err := store.GetProfile(ctx, id)
	if err != nil {
		t.Fatalf("unexpected error retrieving profile: %v", err)
	}
	if retrieved.Name != "Alice Wonderland" || retrieved.Phone != "+1-555-0100" {
		t.Errorf("unexpected profile data: %+v", retrieved)
	}

	if _, err := store.GetProfile(ctx, "nonexistent"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound, got %v", err)
	}
	if _, err := store.GetProfile(ctx, ""); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound for empty ID, got %v", err)
	}

	// GetCredential tests
	credRetrieved, err := store.GetCredential(ctx, "alice_wonder")
	if err != nil {
		t.Fatalf("unexpected error retrieving credential: %v", err)
	}
	if credRetrieved.UserID != id {
		t.Errorf("expected userID %q, got %q", id, credRetrieved.UserID)
	}

	if _, err := store.GetCredential(ctx, "nonexistent"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound, got %v", err)
	}
	if _, err := store.GetCredential(ctx, "   "); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound for blank username, got %v", err)
	}

	// VerifyUserCredential tests
	authedProf, err := store.VerifyUserCredential(ctx, "alice_wonder", "secure-password123")
	if err != nil {
		t.Fatalf("unexpected authentication error: %v", err)
	}
	if authedProf.ID != id {
		t.Errorf("expected authenticated profile ID %q, got %q", id, authedProf.ID)
	}

	if _, err := store.VerifyUserCredential(ctx, "alice_wonder", "wrong-password"); !errors.Is(err, security.ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword, got %v", err)
	}
	if _, err := store.VerifyUserCredential(ctx, "nonexistent", "pass"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound, got %v", err)
	}
}

func TestNoSQLDAO_SearchProfiles(t *testing.T) {
	ctx := context.Background()
	store, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()

	hash, _ := security.HashPassword("pwd")

	users := []struct {
		p models.UserProfile
		u string
	}{
		{p: models.UserProfile{ID: "u1", Name: "Carol Danvers", Phone: "+111", Address: models.Address{Locality: "Boston", Region: "MA", Country: "USA"}}, u: "carol"},
		{p: models.UserProfile{ID: "u2", Name: "Clark Kent", Phone: "+222", Address: models.Address{Locality: "Metropolis", Region: "KS", Country: "USA"}}, u: "clark"},
		{p: models.UserProfile{ID: "u3", Name: "Diana Prince", Phone: "+333", Address: models.Address{Locality: "Themyscira", Region: "Isle", Country: "Greece"}}, u: "diana"},
		{p: models.UserProfile{ID: "u4", Name: "Bruce Wayne", Phone: "+444", Address: models.Address{Locality: "Gotham", Region: "NJ", Country: "USA"}}, u: "bruce"},
	}

	for _, u := range users {
		pCopy := u.p
		if _, err := store.CreateUser(ctx, &pCopy, &models.UserCredential{Username: u.u, PasswordHash: hash}); err != nil {
			t.Fatalf("failed creating user %s: %v", u.u, err)
		}
	}

	// Filter by Name
	res, err := store.SearchProfiles(ctx, models.SearchQuery{Name: "clark"})
	if err != nil || len(res) != 1 || res[0].Name != "Clark Kent" {
		t.Fatalf("expected 1 result for clark, got %v, err: %v", res, err)
	}

	// Filter by Phone
	res, err = store.SearchProfiles(ctx, models.SearchQuery{Phone: "333"})
	if err != nil || len(res) != 1 || res[0].Name != "Diana Prince" {
		t.Fatalf("expected Diana Prince for phone 333, got %v, err: %v", res, err)
	}

	// Filter by Locality
	res, err = store.SearchProfiles(ctx, models.SearchQuery{Locality: "Gotham"})
	if err != nil || len(res) != 1 || res[0].Name != "Bruce Wayne" {
		t.Fatalf("expected Bruce Wayne for Gotham, got %v, err: %v", res, err)
	}

	// Filter by Region
	res, err = store.SearchProfiles(ctx, models.SearchQuery{Region: "KS"})
	if err != nil || len(res) != 1 || res[0].Name != "Clark Kent" {
		t.Fatalf("expected Clark Kent for KS, got %v, err: %v", res, err)
	}

	// Filter by Country
	res, err = store.SearchProfiles(ctx, models.SearchQuery{Country: "USA"})
	if err != nil || len(res) != 3 {
		t.Fatalf("expected 3 USA results, got %d, err: %v", len(res), err)
	}

	// Pagination: Limit and Offset
	resPage1, err := store.SearchProfiles(ctx, models.SearchQuery{Limit: 2, Offset: 0})
	if err != nil || len(resPage1) != 2 {
		t.Fatalf("expected 2 results on page 1, got %d", len(resPage1))
	}
	resPage2, err := store.SearchProfiles(ctx, models.SearchQuery{Limit: 2, Offset: 2})
	if err != nil || len(resPage2) != 2 {
		t.Fatalf("expected 2 results on page 2, got %d", len(resPage2))
	}

	// Offset out of bounds returns empty slice
	resEmpty, err := store.SearchProfiles(ctx, models.SearchQuery{Offset: 100})
	if err != nil || len(resEmpty) != 0 {
		t.Fatalf("expected empty slice when offset out of range, got %v", resEmpty)
	}

	// Default limit handling (<= 0 defaults to 20, > 100 caps at 100)
	resDefLimit, err := store.SearchProfiles(ctx, models.SearchQuery{Limit: -1, Offset: -5})
	if err != nil || len(resDefLimit) != 4 {
		t.Fatalf("expected all 4 results with default limit, got %d", len(resDefLimit))
	}
	resCapLimit, err := store.SearchProfiles(ctx, models.SearchQuery{Limit: 200})
	if err != nil || len(resCapLimit) != 4 {
		t.Fatalf("expected all 4 results with capped limit, got %d", len(resCapLimit))
	}
}

func TestNoSQLDAO_ContextCancelledAndClosedErrors(t *testing.T) {
	ctx := context.Background()
	store, _ := NewNoSQLDAO("")

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	p := &models.UserProfile{ID: "u1", Name: "Test"}
	c := &models.UserCredential{Username: "test"}

	if _, err := store.CreateUser(canceledCtx, p, c); err == nil {
		t.Error("expected error for canceled context on CreateUser")
	}
	if _, err := store.GetProfile(canceledCtx, "u1"); err == nil {
		t.Error("expected error for canceled context on GetProfile")
	}
	if _, err := store.SearchProfiles(canceledCtx, models.SearchQuery{}); err == nil {
		t.Error("expected error for canceled context on SearchProfiles")
	}
	if _, err := store.GetCredential(canceledCtx, "test"); err == nil {
		t.Error("expected error for canceled context on GetCredential")
	}
	if _, err := store.VerifyUserCredential(canceledCtx, "test", "pw"); err == nil {
		t.Error("expected error for canceled context on VerifyUserCredential")
	}

	// Close store and check operations
	_ = store.Close()
	if _, err := store.CreateUser(ctx, p, c); err == nil {
		t.Error("expected error on closed CreateUser")
	}
	if _, err := store.GetProfile(ctx, "u1"); err == nil {
		t.Error("expected error on closed GetProfile")
	}
	if _, err := store.SearchProfiles(ctx, models.SearchQuery{}); err == nil {
		t.Error("expected error on closed SearchProfiles")
	}
	if _, err := store.GetCredential(ctx, "test"); err == nil {
		t.Error("expected error on closed GetCredential")
	}
}
