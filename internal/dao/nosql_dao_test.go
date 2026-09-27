package dao

import (
	"context"
	"errors"
	"path/filepath"
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

	// Fresh store has migration version 0
	ver0, err := store.MigrationVersion(ctx)
	if err != nil || ver0 != 0 {
		t.Fatalf("expected initial MigrationVersion 0, got %d, err: %v", ver0, err)
	}

	// First MigrateUp should succeed with count 1
	count, err := store.MigrateUp(ctx)
	if err != nil || count != 1 {
		t.Fatalf("expected MigrateUp to succeed with count 1, got %d, err: %v", count, err)
	}

	// Repeated MigrateUp should report 0 applied migrations
	repeatCount, err := store.MigrateUp(ctx)
	if err != nil || repeatCount != 0 {
		t.Fatalf("expected repeated MigrateUp to return 0, got %d, err: %v", repeatCount, err)
	}

	ver, err := store.MigrationVersion(ctx)
	if err != nil || ver != 1 {
		t.Fatalf("expected MigrationVersion 1, got %d, err: %v", ver, err)
	}

	statuses, err := store.MigrationStatus(ctx)
	if err != nil || len(statuses) != 1 || !statuses[0].Applied {
		t.Fatalf("expected 1 applied migration status, got %v, err: %v", statuses, err)
	}

	// MigrateDown 1 step should succeed with count 1
	downCount, err := store.MigrateDown(ctx, 1)
	if err != nil || downCount != 1 {
		t.Fatalf("expected MigrateDown to succeed with 1, got %d, err: %v", downCount, err)
	}

	// Repeated MigrateDown should return 0
	repeatDown, err := store.MigrateDown(ctx, 1)
	if err != nil || repeatDown != 0 {
		t.Fatalf("expected repeated MigrateDown to return 0, got %d, err: %v", repeatDown, err)
	}

	verDown, err := store.MigrationVersion(ctx)
	if err != nil || verDown != 0 {
		t.Fatalf("expected MigrationVersion 0 after rollback, got %d, err: %v", verDown, err)
	}

	// Migrate resets state to migrated
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("unexpected error migrating: %v", err)
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
	if _, err := store.MigrateUp(canceledCtx); err == nil {
		t.Error("expected error with canceled context on MigrateUp")
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
	if _, err := store.CreateUser(ctx, &models.UserProfile{Name: "", Phone: "+1"}, cred); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty name, got %v", err)
	}
	if _, err := store.CreateUser(ctx, &models.UserProfile{Name: "Alice", Phone: ""}, cred); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty phone, got %v", err)
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
	dupProfile := &models.UserProfile{Name: "Alice Twin", Phone: "+1-555-0101"}
	if _, err := store.CreateUser(ctx, dupProfile, cred); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("expected ErrUsernameTaken, got %v", err)
	}

	// Duplicate caller-supplied ID should fail and not corrupt indices
	dupIDProfile := &models.UserProfile{ID: "user-123", Name: "Impostor Alice", Phone: "+1-555-0102"}
	dupIDCred := &models.UserCredential{Username: "impostor_alice", PasswordHash: hash}
	if _, err := store.CreateUser(ctx, dupIDProfile, dupIDCred); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for duplicate user ID, got %v", err)
	}
	// Verify original user is intact and impostor username was not registered
	origCred, err := store.GetCredential(ctx, "alice_wonder")
	if err != nil || origCred.UserID != "user-123" {
		t.Errorf("original user credential was corrupted: %v, %v", origCred, err)
	}
	if _, err := store.GetCredential(ctx, "impostor_alice"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound for rejected impostor username, got %v", err)
	}

	// Create user with generated UUID
	prof2 := &models.UserProfile{Name: "Bob Builder", Phone: "+1-555-0200"}
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

func TestNoSQLDAO_SearchProfiles_ExactMatchSemantics(t *testing.T) {
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
		{p: models.UserProfile{ID: "u1", Name: "Carol Danvers", Phone: "+111", Address: models.Address{Locality: "Denver", Region: "CO", Country: "US"}}, u: "carol"},
		{p: models.UserProfile{ID: "u2", Name: "Clark Kent", Phone: "+222", Address: models.Address{Locality: "Denver City", Region: "CO North", Country: "USA"}}, u: "clark"},
		{p: models.UserProfile{ID: "u3", Name: "Diana Prince", Phone: "+333", Address: models.Address{Locality: "Themyscira", Region: "Isle", Country: "Greece"}}, u: "diana"},
		{p: models.UserProfile{ID: "u4", Name: "Bruce Wayne", Phone: "+444", Address: models.Address{Locality: "Gotham", Region: "NJ", Country: "USA"}}, u: "bruce"},
	}

	for _, u := range users {
		pCopy := u.p
		if _, err := store.CreateUser(ctx, &pCopy, &models.UserCredential{Username: u.u, PasswordHash: hash}); err != nil {
			t.Fatalf("failed creating user %s: %v", u.u, err)
		}
	}

	// Filter by Name: substring matching
	res, err := store.SearchProfiles(ctx, models.SearchQuery{Name: "clark"})
	if err != nil || len(res) != 1 || res[0].Name != "Clark Kent" {
		t.Fatalf("expected 1 result for clark, got %v, err: %v", res, err)
	}

	// Filter by Phone: substring matching
	res, err = store.SearchProfiles(ctx, models.SearchQuery{Phone: "333"})
	if err != nil || len(res) != 1 || res[0].Name != "Diana Prince" {
		t.Fatalf("expected Diana Prince for phone 333, got %v, err: %v", res, err)
	}

	// Filter by Locality: EXACT case-insensitive match (Denver should match u1, NOT Denver City u2)
	res, err = store.SearchProfiles(ctx, models.SearchQuery{Locality: "denver"})
	if err != nil || len(res) != 1 || res[0].ID != "u1" {
		t.Fatalf("expected only Denver (u1), got %d results, err: %v", len(res), err)
	}

	// Filter by Region: EXACT case-insensitive match (CO should match u1, NOT CO North u2)
	res, err = store.SearchProfiles(ctx, models.SearchQuery{Region: "co"})
	if err != nil || len(res) != 1 || res[0].ID != "u1" {
		t.Fatalf("expected only CO (u1), got %d results, err: %v", len(res), err)
	}

	// Filter by Country: EXACT case-insensitive match (US should match u1, NOT USA u2 or u4)
	res, err = store.SearchProfiles(ctx, models.SearchQuery{Country: "us"})
	if err != nil || len(res) != 1 || res[0].ID != "u1" {
		t.Fatalf("expected only US (u1), got %d results, err: %v", len(res), err)
	}

	resUSA, err := store.SearchProfiles(ctx, models.SearchQuery{Country: "usa"})
	if err != nil || len(resUSA) != 2 {
		t.Fatalf("expected 2 USA results, got %d, err: %v", len(resUSA), err)
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
}

func TestNoSQLDAO_FilePersistence(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	storeFile := filepath.Join(tmpDir, "store.json")

	// Phase 1: Create store, persist data
	store1, err := NewNoSQLDAO("file://" + storeFile)
	if err != nil {
		t.Fatalf("failed to create file-backed store: %v", err)
	}

	hash, _ := security.HashPassword("persistpass")
	p := &models.UserProfile{ID: "persist-1", Name: "Persistent User", Phone: "+1-555-0300"}
	c := &models.UserCredential{Username: "persist_user", PasswordHash: hash}

	id, err := store1.CreateUser(ctx, p, c)
	if err != nil {
		t.Fatalf("failed to create user in file store: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("failed to close store1: %v", err)
	}

	// Phase 2: Reopen store from same file
	store2, err := NewNoSQLDAO(storeFile)
	if err != nil {
		t.Fatalf("failed to reload file-backed store: %v", err)
	}
	defer func() { _ = store2.Close() }()

	reloadedProfile, err := store2.GetProfile(ctx, id)
	if err != nil || reloadedProfile.Name != "Persistent User" {
		t.Fatalf("expected reloaded profile from disk, got: %+v, err: %v", reloadedProfile, err)
	}

	reloadedCred, err := store2.GetCredential(ctx, "persist_user")
	if err != nil || reloadedCred.UserID != id {
		t.Fatalf("expected reloaded credential from disk, got: %+v, err: %v", reloadedCred, err)
	}
}

func TestNoSQLDAO_ContextCancelledAndClosedErrors(t *testing.T) {
	ctx := context.Background()
	store, _ := NewNoSQLDAO("")

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	p := &models.UserProfile{ID: "u1", Name: "Test", Phone: "+1-555-0400"}
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
