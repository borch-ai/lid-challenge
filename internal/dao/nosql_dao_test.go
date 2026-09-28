package dao

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if _, err := store.GetProfile(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty ID, got %v", err)
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
	if _, err := store.GetCredential(ctx, "   "); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for blank username, got %v", err)
	}
	if _, err := store.GetCredential(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty username, got %v", err)
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
	if _, err := store.VerifyUserCredential(ctx, "   ", "pass"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for blank username, got %v", err)
	}
	if _, err := store.VerifyUserCredential(ctx, "alice_wonder", "   "); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for blank password, got %v", err)
	}

	// Credential method validation and defaulting
	unsupportedCred := &models.UserCredential{Username: "unsupported", PasswordHash: hash, Method: "md5"}
	unsupportedProf := &models.UserProfile{Name: "Unsupported", Phone: "+1-555-9999"}
	if _, err := store.CreateUser(ctx, unsupportedProf, unsupportedCred); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for unsupported credential method, got %v", err)
	}

	// Untrimmed username semantics matching SQL DAO
	untrimmedCred := &models.UserCredential{Username: " alice_untrimmed ", PasswordHash: hash}
	untrimmedProf := &models.UserProfile{Name: "Untrimmed", Phone: "+1-555-8888"}
	if _, err := store.CreateUser(ctx, untrimmedProf, untrimmedCred); err != nil {
		t.Fatalf("failed to create user with untrimmed username: %v", err)
	}
	if _, err := store.GetCredential(ctx, "alice_untrimmed"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound when querying trimmed variant of untrimmed username")
	}
	if fetched, err := store.GetCredential(ctx, " alice_untrimmed "); err != nil || fetched.Username != " alice_untrimmed " {
		t.Errorf("expected successful retrieval with exact untrimmed username, got %v, err %v", fetched, err)
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

	// Offset > 10000 clamped to 10000
	resClamped, err := store.SearchProfiles(ctx, models.SearchQuery{Offset: 20000})
	if err != nil || len(resClamped) != 0 {
		t.Fatalf("expected empty slice for clamped offset, got %v", resClamped)
	}

	// Whitespace preservation: query with whitespace does not match trimmed value in DB
	resWS, err := store.SearchProfiles(ctx, models.SearchQuery{Locality: " denver "})
	if err != nil || len(resWS) != 0 {
		t.Fatalf("expected 0 results when query has leading/trailing whitespace, got %d", len(resWS))
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

	// Verify credential verification succeeds after reload from disk
	authed, err := store2.VerifyUserCredential(ctx, "persist_user", "persistpass")
	if err != nil || authed.ID != id {
		t.Fatalf("expected successful credential verification from reloaded disk file, got: %v, err: %v", authed, err)
	}

	// Phase 3: Verify migration metadata persistence across restarts
	if _, err := store2.MigrateUp(ctx); err != nil {
		t.Fatalf("failed to migrate store2: %v", err)
	}
	_ = store2.Close()

	storeMig, err := NewNoSQLDAO(storeFile)
	if err != nil {
		t.Fatalf("failed to reload file-backed store for migration check: %v", err)
	}
	defer func() { _ = storeMig.Close() }()

	verMig, err := storeMig.MigrationVersion(ctx)
	if err != nil || verMig != 1 {
		t.Fatalf("expected reloaded migration version 1, got %d, err: %v", verMig, err)
	}
	statuses1, err := storeMig.MigrationStatus(ctx)
	if err != nil || len(statuses1) == 0 || !statuses1[0].Applied || statuses1[0].AppliedAt == nil {
		t.Fatalf("expected applied migration status after reload, got: %+v, err: %v", statuses1, err)
	}
	// Calling MigrationStatus again returns the same recorded timestamp
	statuses2, _ := storeMig.MigrationStatus(ctx)
	if !statuses1[0].AppliedAt.Equal(*statuses2[0].AppliedAt) {
		t.Errorf("expected stable AppliedAt timestamp, got %v vs %v", statuses1[0].AppliedAt, statuses2[0].AppliedAt)
	}

	// Phase 4: Preserve caller-provided CreatedAt timestamp
	customCreated := time.Date(2025, time.January, 15, 10, 0, 0, 0, time.UTC)
	profCustom := &models.UserProfile{ID: "custom-created", Name: "Custom Time", Phone: "+1-555-0999", CreatedAt: customCreated}
	credCustom := &models.UserCredential{Username: "custom_created", PasswordHash: hash, CreatedAt: customCreated}
	if _, err := storeMig.CreateUser(ctx, profCustom, credCustom); err != nil {
		t.Fatalf("failed to create user with custom created_at: %v", err)
	}
	fetchedCustom, err := storeMig.GetProfile(ctx, "custom-created")
	if err != nil || !fetchedCustom.CreatedAt.Equal(customCreated) {
		t.Errorf("expected preserved CreatedAt %v, got %v, err %v", customCreated, fetchedCustom.CreatedAt, err)
	}

	// Phase 5: Reject malformed file on reload in persistLocked without overwriting
	if err := os.WriteFile(storeFile, []byte("{invalid-corrupt-json"), 0600); err != nil {
		t.Fatalf("failed to corrupt file: %v", err)
	}
	profFail := &models.UserProfile{ID: "fail-doc", Name: "Fail", Phone: "+1-555-0000"}
	credFail := &models.UserCredential{Username: "fail_doc", PasswordHash: hash}
	if _, err := storeMig.CreateUser(ctx, profFail, credFail); err == nil {
		t.Error("expected error writing when disk file is corrupted, got nil")
	}

	// Phase 6: Reject null JSON decoded content
	nullFile := filepath.Join(tmpDir, "null.json")
	if err := os.WriteFile(nullFile, []byte("null"), 0600); err != nil {
		t.Fatalf("failed to write null file: %v", err)
	}
	if _, err := NewNoSQLDAO(nullFile); err == nil {
		t.Error("expected error loading JSON file decoding to null map, got nil")
	}

	// Phase 7: Reject null document entries within document map
	nullDocFile := filepath.Join(tmpDir, "nulldoc.json")
	if err := os.WriteFile(nullDocFile, []byte(`{"user1": null}`), 0600); err != nil {
		t.Fatalf("failed to write nulldoc file: %v", err)
	}
	if _, err := NewNoSQLDAO(nullDocFile); err == nil {
		t.Error("expected error loading JSON file containing null document, got nil")
	}

	// Phase 8: Reject duplicate usernames when loading file store
	dupUserJSON := `{
		"user1": {"id": "user1", "profile": {"name": "U1", "phone": "123"}, "credential": {"username": "dupuser", "password_hash": "h1"}},
		"user2": {"id": "user2", "profile": {"name": "U2", "phone": "456"}, "credential": {"username": "dupuser", "password_hash": "h2"}}
	}`
	dupFile := filepath.Join(tmpDir, "dupuser.json")
	if err := os.WriteFile(dupFile, []byte(dupUserJSON), 0600); err != nil {
		t.Fatalf("failed to write dupuser file: %v", err)
	}
	if _, err := NewNoSQLDAO(dupFile); err == nil {
		t.Error("expected error loading file with duplicate usernames in raw map, got nil")
	}

	dupPayloadJSON := `{
		"version": 1,
		"documents": {
			"user1": {"id": "user1", "profile": {"name": "U1", "phone": "123"}, "credential": {"username": "dupuser2", "password_hash": "h1"}},
			"user2": {"id": "user2", "profile": {"name": "U2", "phone": "456"}, "credential": {"username": "dupuser2", "password_hash": "h2"}}
		}
	}`
	dupPayloadFile := filepath.Join(tmpDir, "dup_payload.json")
	if err := os.WriteFile(dupPayloadFile, []byte(dupPayloadJSON), 0600); err != nil {
		t.Fatalf("failed to write dup payload file: %v", err)
	}
	if _, err := NewNoSQLDAO(dupPayloadFile); err == nil {
		t.Error("expected error loading file with duplicate usernames in structured payload, got nil")
	}

	// Phase 9: Restore migration state when persistence fails in Migrate()
	corruptMigFile := filepath.Join(tmpDir, "corrupt_mig.json")
	if err := os.WriteFile(corruptMigFile, []byte(`{}`), 0600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	corruptMigStore, err := NewNoSQLDAO(corruptMigFile)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer func() { _ = corruptMigStore.Close() }()

	// Corrupt file on disk so persistLocked fails on reload
	if err := os.WriteFile(corruptMigFile, []byte("bad-json"), 0600); err != nil {
		t.Fatalf("failed to corrupt file: %v", err)
	}
	if err := corruptMigStore.Migrate(ctx); err == nil {
		t.Error("expected Migrate to fail when disk file is corrupted, got nil")
	}
	// Verify state was restored: migrated should still be false and version 0
	verAfterFail, err := corruptMigStore.MigrationVersion(ctx)
	if err != nil || verAfterFail != 0 {
		t.Errorf("expected version 0 after failed Migrate, got %d, err %v", verAfterFail, err)
	}

	// Phase 10: Reject duplicate usernames during reload merge in persistLocked
	diskReloadFile := filepath.Join(tmpDir, "reload_dup.json")
	initialJSON := `{
		"user1": {"id": "user1", "profile": {"name": "U1", "phone": "123"}, "credential": {"username": "unique1", "password_hash": "h1"}}
	}`
	if err := os.WriteFile(diskReloadFile, []byte(initialJSON), 0600); err != nil {
		t.Fatalf("failed to write reload file: %v", err)
	}
	reloadStore, err := NewNoSQLDAO(diskReloadFile)
	if err != nil {
		t.Fatalf("failed to open reload store: %v", err)
	}
	defer func() { _ = reloadStore.Close() }()

	// Create user with username "conflict" in store
	if _, err := reloadStore.CreateUser(ctx, &models.UserProfile{ID: "local1", Name: "Local", Phone: "111"}, &models.UserCredential{Username: "conflict", PasswordHash: hash}); err != nil {
		t.Fatalf("failed to create local user: %v", err)
	}
	// Simulate external process writing a different user ID with the same username "conflict" to disk
	externalJSON := `{
		"user1": {"id": "user1", "profile": {"name": "U1", "phone": "123"}, "credential": {"username": "unique1", "password_hash": "h1"}},
		"external1": {"id": "external1", "profile": {"name": "Ext", "phone": "222"}, "credential": {"username": "conflict", "password_hash": "h2"}}
	}`
	if err := os.WriteFile(diskReloadFile, []byte(externalJSON), 0600); err != nil {
		t.Fatalf("failed to write external JSON: %v", err)
	}
	// Another local write triggers reload and must detect username collision with local1
	if _, err := reloadStore.CreateUser(ctx, &models.UserProfile{ID: "local2", Name: "Local 2", Phone: "333"}, &models.UserCredential{Username: "local2", PasswordHash: hash}); err == nil {
		t.Error("expected error when disk reload introduces duplicate username for another ID, got nil")
	}

	// Phase 11: Verify replaced document's old username is removed from index on reload
	updateUserJSON := `{
		"user1": {"id": "user1", "profile": {"name": "U1", "phone": "123"}, "credential": {"username": "new_alice", "password_hash": "h1"}, "updated_at": "2099-01-01T00:00:00Z"}
	}`
	if err := os.WriteFile(diskReloadFile, []byte(updateUserJSON), 0600); err != nil {
		t.Fatalf("failed to write updateUserJSON: %v", err)
	}
	reloadStore2, err := NewNoSQLDAO(diskReloadFile)
	if err != nil {
		t.Fatalf("failed to open reload store 2: %v", err)
	}
	defer func() { _ = reloadStore2.Close() }()
	// "unique1" was replaced by "new_alice" for user1; old username must no longer resolve
	if _, err := reloadStore2.GetCredential(ctx, "unique1"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound for replaced username, got %v", err)
	}
	if cred, err := reloadStore2.GetCredential(ctx, "new_alice"); err != nil || cred.UserID != "user1" {
		t.Errorf("expected new_alice to resolve to user1, got cred %v, err %v", cred, err)
	}

	// Phase 12: Preserving caller-provided ID without trimming
	untrimmedID := "  untrimmed-id  "
	pUntrimmed := &models.UserProfile{ID: untrimmedID, Name: "Untrimmed", Phone: "123"}
	cUntrimmed := &models.UserCredential{Username: "untrimmed_user", PasswordHash: hash}
	returnedID, err := reloadStore2.CreateUser(ctx, pUntrimmed, cUntrimmed)
	if err != nil {
		t.Fatalf("failed to create user with untrimmed ID: %v", err)
	}
	if returnedID != untrimmedID {
		t.Errorf("expected preserved ID %q, got %q", untrimmedID, returnedID)
	}
	profFetched, err := reloadStore2.GetProfile(ctx, untrimmedID)
	if err != nil || profFetched.ID != untrimmedID {
		t.Errorf("expected profile with ID %q, got %+v, err %v", untrimmedID, profFetched, err)
	}

	// Phase 13: Blank user ID in GetProfile returns ErrInvalidInput
	if _, err := reloadStore2.GetProfile(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty userID in GetProfile, got: %v", err)
	}
	if _, err := reloadStore2.GetProfile(ctx, "   "); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for whitespace-only userID in GetProfile, got: %v", err)
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

func TestNoSQLDAO_MalformedFileDSN(t *testing.T) {
	for _, badDSN := range []string{"file://", "file://  ", "file://\t"} {
		if _, err := NewNoSQLDAO(badDSN); err == nil {
			t.Errorf("expected error for malformed file DSN %q, got nil", badDSN)
		}
	}
}

func TestNoSQLDAO_PingHealthCheck(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// 1. Inaccessible directory
	subDir := filepath.Join(tmpDir, "sub")
	if err := os.Mkdir(subDir, 0700); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}
	filePath := filepath.Join(subDir, "health.json")
	storeFile, err := NewNoSQLDAO(filePath)
	if err != nil {
		t.Fatalf("unexpected error creating DAO pointing to file: %v", err)
	}
	if err := storeFile.Ping(ctx); err != nil {
		t.Errorf("expected successful Ping for valid file path, got: %v", err)
	}

	// Remove subdir to make path inaccessible
	_ = os.RemoveAll(subDir)
	if err := storeFile.Ping(ctx); err == nil {
		t.Error("expected Ping error when parent directory is removed, got nil")
	}

	// 2. Closed store
	_ = storeFile.Close()
	if err := storeFile.Ping(ctx); err == nil {
		t.Error("expected error on Ping for closed store, got nil")
	}
}

func TestNoSQLDAO_FileBackedMigrateDownRollback(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "migrate_down_test.json")

	store, err := NewNoSQLDAO(filePath)
	if err != nil {
		t.Fatalf("failed to create nosql dao: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Initially version 0
	ver, err := store.MigrationVersion(ctx)
	if err != nil || ver != 0 {
		t.Fatalf("expected initial version 0, got %d, err %v", ver, err)
	}

	// MigrateUp -> version 1
	upCount, err := store.MigrateUp(ctx)
	if err != nil || upCount != 1 {
		t.Fatalf("expected MigrateUp to return 1, got %d, err %v", upCount, err)
	}
	ver, err = store.MigrationVersion(ctx)
	if err != nil || ver != 1 {
		t.Fatalf("expected version 1 after MigrateUp, got %d, err %v", ver, err)
	}
	status, err := store.MigrationStatus(ctx)
	if err != nil || len(status) == 0 || !status[0].Applied {
		t.Fatalf("expected status applied true, got %+v, err %v", status, err)
	}

	// MigrateDown -> version 0
	downCount, err := store.MigrateDown(ctx, 1)
	if err != nil || downCount != 1 {
		t.Fatalf("expected MigrateDown to return 1, got %d, err %v", downCount, err)
	}
	ver, err = store.MigrationVersion(ctx)
	if err != nil || ver != 0 {
		t.Fatalf("expected version 0 after MigrateDown, got %d, err %v", ver, err)
	}
	status, err = store.MigrationStatus(ctx)
	if err != nil || len(status) == 0 || status[0].Applied {
		t.Fatalf("expected status applied false, got %+v, err %v", status, err)
	}

	// Reopen file in a new DAO instance to verify disk persistence of rollback
	reopened, err := NewNoSQLDAO(filePath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	reopenedVer, err := reopened.MigrationVersion(ctx)
	if err != nil || reopenedVer != 0 {
		t.Fatalf("expected reopened store to have version 0, got %d, err %v", reopenedVer, err)
	}
	reopenedStatus, err := reopened.MigrationStatus(ctx)
	if err != nil || len(reopenedStatus) == 0 || reopenedStatus[0].Applied {
		t.Fatalf("expected reopened status applied false, got %+v, err %v", reopenedStatus, err)
	}

	// Can be migrated up again
	upAgain, err := reopened.MigrateUp(ctx)
	if err != nil || upAgain != 1 {
		t.Fatalf("expected MigrateUp on reopened store to succeed with 1, got %d, err %v", upAgain, err)
	}
}

func TestNoSQLDAO_CandidateMergeIntegrity(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "merge_integrity.json")

	hash, _ := security.HashPassword("secret")

	// Create initial file with user1 ("alice") and user2 ("bob")
	initStore, err := NewNoSQLDAO(filePath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	if _, err := initStore.CreateUser(ctx, &models.UserProfile{ID: "user1", Name: "Alice", Phone: "+1-555-0101"}, &models.UserCredential{Username: "alice", PasswordHash: hash}); err != nil {
		t.Fatalf("failed to create user1: %v", err)
	}
	if _, err := initStore.CreateUser(ctx, &models.UserProfile{ID: "user2", Name: "Bob", Phone: "+1-555-0102"}, &models.UserCredential{Username: "bob", PasswordHash: hash}); err != nil {
		t.Fatalf("failed to create user2: %v", err)
	}
	_ = initStore.Close()

	// Open store in second instance
	store, err := NewNoSQLDAO(filePath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	// External process replaces user1's username on disk with "bob" (which collides with user2)
	externalConflictJSON := `{
		"version": 1,
		"documents": {
			"user1": {"id": "user1", "profile": {"name": "Alice", "phone": "+1-555-0101"}, "credential": {"username": "bob", "password_hash": "h"}, "updated_at": "2099-01-01T00:00:00Z"},
			"user2": {"id": "user2", "profile": {"name": "Bob", "phone": "+1-555-0102"}, "credential": {"username": "bob", "password_hash": "h"}, "updated_at": "2099-01-01T00:00:00Z"}
		}
	}`
	if err := os.WriteFile(filePath, []byte(externalConflictJSON), 0600); err != nil {
		t.Fatalf("failed to write external conflict: %v", err)
	}

	// Local write triggers persistLocked which reloads and detects the conflict during candidate validation
	_, createErr := store.CreateUser(ctx, &models.UserProfile{ID: "user3", Name: "Charlie", Phone: "+1-555-0103"}, &models.UserCredential{Username: "charlie", PasswordHash: hash})
	if createErr == nil {
		t.Fatal("expected CreateUser to fail due to external username collision during reload merge")
	}

	// Verify in-memory username index was NOT corrupted by the failed merge:
	// "alice" must still exist and map to user1, and user1's profile must still be accessible
	cred, err := store.GetCredential(ctx, "alice")
	if err != nil || cred.UserID != "user1" {
		t.Fatalf("expected 'alice' mapping to be preserved in byUsername index, got cred %+v, err %v", cred, err)
	}
}

func TestNoSQLDAO_DocumentKeyIdentityMismatch(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Map key "user1" but document ID "user2"
	mismatchIDJSON := `{
		"version": 1,
		"documents": {
			"user1": {"id": "user2", "profile": {"id": "user2", "name": "Mismatched", "phone": "123"}, "credential": {"username": "user1", "user_id": "user2"}}
		}
	}`
	mismatchFile := filepath.Join(tmpDir, "mismatch_id.json")
	if err := os.WriteFile(mismatchFile, []byte(mismatchIDJSON), 0600); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := NewNoSQLDAO(mismatchFile); err == nil {
		t.Error("expected error loading document with mismatched document ID vs map key, got nil")
	}

	// 2. Profile ID mismatch
	mismatchProfileJSON := `{
		"version": 1,
		"documents": {
			"user1": {"id": "user1", "profile": {"id": "user2", "name": "Mismatched", "phone": "123"}, "credential": {"username": "user1", "user_id": "user1"}}
		}
	}`
	mismatchProfileFile := filepath.Join(tmpDir, "mismatch_profile.json")
	if err := os.WriteFile(mismatchProfileFile, []byte(mismatchProfileJSON), 0600); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := NewNoSQLDAO(mismatchProfileFile); err == nil {
		t.Error("expected error loading document with mismatched profile ID vs map key, got nil")
	}

	// 3. Credential UserID mismatch
	mismatchUserIDJSON := `{
		"version": 1,
		"documents": {
			"user1": {"id": "user1", "profile": {"id": "user1", "name": "Mismatched", "phone": "123"}, "credential": {"username": "user1", "user_id": "user2"}}
		}
	}`
	mismatchUserIDFile := filepath.Join(tmpDir, "mismatch_user_id.json")
	if err := os.WriteFile(mismatchUserIDFile, []byte(mismatchUserIDJSON), 0600); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := NewNoSQLDAO(mismatchUserIDFile); err == nil {
		t.Error("expected error loading document with mismatched credential user_id vs map key, got nil")
	}
}

func TestNoSQLDAO_CreateUser_DiskIDCollisionRejection(t *testing.T) {
	ctx := context.Background()
	hash, err := security.HashPassword("secretpass")
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "shared_store.json")

	store1, err := NewNoSQLDAO(filePath)
	if err != nil {
		t.Fatalf("failed to create store1: %v", err)
	}
	defer func() { _ = store1.Close() }()

	store2, err := NewNoSQLDAO(filePath)
	if err != nil {
		t.Fatalf("failed to create store2: %v", err)
	}
	defer func() { _ = store2.Close() }()

	// Store 1 creates a user with caller-supplied ID
	origProfile := &models.UserProfile{ID: "shared-user-1", Name: "Original User", Phone: "555-1111"}
	origCred := &models.UserCredential{Username: "orig_user", PasswordHash: hash}
	if _, err := store1.CreateUser(ctx, origProfile, origCred); err != nil {
		t.Fatalf("store1 failed to create user: %v", err)
	}

	// Store 2 attempts to create a user with the same caller-supplied ID
	collidingProfile := &models.UserProfile{ID: "shared-user-1", Name: "Colliding User", Phone: "555-2222"}
	collidingCred := &models.UserCredential{Username: "colliding_user", PasswordHash: hash}
	if _, err := store2.CreateUser(ctx, collidingProfile, collidingCred); err == nil {
		t.Fatal("expected store2.CreateUser to fail with duplicate ID conflict from disk, got nil")
	}

	// Verify store 2 did not overwrite the document on disk or corrupt username indices
	profOnDisk, err := store1.GetProfile(ctx, "shared-user-1")
	if err != nil {
		t.Fatalf("store1 failed to get profile: %v", err)
	}
	if profOnDisk.Name != "Original User" {
		t.Errorf("expected original profile name %q, got %q (document was overwritten!)", "Original User", profOnDisk.Name)
	}

	// Verify colliding username was not indexed
	if _, err := store2.GetCredential(ctx, "colliding_user"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound for colliding username that failed creation, got %v", err)
	}
}

func TestNoSQLDAO_PersistLocked_DiskMergeIDCollisionRejection(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "merge_collision.json")

	initialJSON := `{
		"version": 1,
		"documents": {
			"existing-1": {"id": "existing-1", "profile": {"name": "Ex", "phone": "123"}, "credential": {"username": "ex", "password_hash": "h"}, "updated_at": "2020-01-01T00:00:00Z"}
		}
	}`
	if err := os.WriteFile(filePath, []byte(initialJSON), 0600); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
	}

	store, err := NewNoSQLDAO(filePath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()

	// External process writes "new-doc-1" to disk
	externalJSON := `{
		"version": 1,
		"documents": {
			"existing-1": {"id": "existing-1", "profile": {"name": "Ex", "phone": "123"}, "credential": {"username": "ex", "password_hash": "h"}, "updated_at": "2020-01-01T00:00:00Z"},
			"new-doc-1": {"id": "new-doc-1", "profile": {"name": "External Doc", "phone": "456"}, "credential": {"username": "ext_doc", "password_hash": "h"}, "updated_at": "2020-01-01T00:00:00Z"}
		}
	}`
	if err := os.WriteFile(filePath, []byte(externalJSON), 0600); err != nil {
		t.Fatalf("failed to write external file: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// persistLocked with newlyCreatedID="new-doc-1" must reject collision
	err = store.persistLocked(false, "new-doc-1")
	if err == nil {
		t.Fatal("expected persistLocked to reject collision for newly created ID existing on disk, got nil")
	}
	if !strings.Contains(err.Error(), "already exists on disk") {
		t.Errorf("expected error containing 'already exists on disk', got: %v", err)
	}
}

func TestNoSQLDAO_UnsupportedSchemaVersion(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Future version (version 2) on initial load
	v2JSON := `{
		"version": 2,
		"documents": {}
	}`
	v2File := filepath.Join(tmpDir, "v2.json")
	if err := os.WriteFile(v2File, []byte(v2JSON), 0600); err != nil {
		t.Fatalf("failed to write v2 file: %v", err)
	}
	if _, err := NewNoSQLDAO(v2File); err == nil {
		t.Error("expected error loading document store with version 2, got nil")
	}

	// 2. Negative version on initial load
	negJSON := `{
		"version": -1,
		"documents": {}
	}`
	negFile := filepath.Join(tmpDir, "neg.json")
	if err := os.WriteFile(negFile, []byte(negJSON), 0600); err != nil {
		t.Fatalf("failed to write neg file: %v", err)
	}
	if _, err := NewNoSQLDAO(negFile); err == nil {
		t.Error("expected error loading document store with negative version, got nil")
	}

	// 3. Future version introduced on disk during reload merge
	v1File := filepath.Join(tmpDir, "v1_to_v2.json")
	if err := os.WriteFile(v1File, []byte(`{"version": 1, "documents": {}}`), 0600); err != nil {
		t.Fatalf("failed to write v1 file: %v", err)
	}
	store, err := NewNoSQLDAO(v1File)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	// External process updates disk to version 2
	if err := os.WriteFile(v1File, []byte(`{"version": 2, "documents": {}}`), 0600); err != nil {
		t.Fatalf("failed to update to v2: %v", err)
	}
	ctx := context.Background()
	hash, _ := security.HashPassword("pass")
	_, err = store.CreateUser(ctx, &models.UserProfile{Name: "Test", Phone: "123"}, &models.UserCredential{Username: "test", PasswordHash: hash})
	if err == nil {
		t.Error("expected CreateUser to fail when disk file has unsupported schema version 2, got nil")
	}
}

func TestNoSQLDAO_CreateUser_PopulatesCallerObjects(t *testing.T) {
	ctx := context.Background()
	store, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()

	hash, _ := security.HashPassword("pass")
	p := &models.UserProfile{
		Name:  "Alice",
		Phone: "123",
	}
	c := &models.UserCredential{
		Username:     "alice",
		PasswordHash: hash,
	}

	id, err := store.CreateUser(ctx, p, c)
	if err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	if p.ID != id || p.ID == "" {
		t.Errorf("expected p.ID to be populated with %q, got %q", id, p.ID)
	}
	if p.CreatedAt.IsZero() {
		t.Errorf("expected p.CreatedAt to be non-zero")
	}
	if p.UpdatedAt.IsZero() {
		t.Errorf("expected p.UpdatedAt to be non-zero")
	}
	if c.UserID != id {
		t.Errorf("expected c.UserID to be populated with %q, got %q", id, c.UserID)
	}
	if c.CreatedAt.IsZero() {
		t.Errorf("expected c.CreatedAt to be non-zero")
	}
	if c.UpdatedAt.IsZero() {
		t.Errorf("expected c.UpdatedAt to be non-zero")
	}
}

func TestNoSQLDAO_MigrateDown_TearsDownCollectionAndData(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "teardown_test.json")

	store, err := NewNoSQLDAO(filePath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Apply migration 1
	if _, err := store.MigrateUp(ctx); err != nil {
		t.Fatalf("failed to migrate up: %v", err)
	}

	hash, _ := security.HashPassword("pass")
	p := &models.UserProfile{Name: "Bob", Phone: "456"}
	c := &models.UserCredential{Username: "bob", PasswordHash: hash}
	id, err := store.CreateUser(ctx, p, c)
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Verify profile is accessible before rollback
	if _, err := store.GetProfile(ctx, id); err != nil {
		t.Fatalf("expected profile to exist before rollback, got: %v", err)
	}

	// Roll back migration 1 -> tears down collection
	downCount, err := store.MigrateDown(ctx, 1)
	if err != nil || downCount != 1 {
		t.Fatalf("expected MigrateDown to return 1, got %d, err %v", downCount, err)
	}

	// Verify profile is no longer in store
	if _, err := store.GetProfile(ctx, id); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound after MigrateDown rollback, got %v", err)
	}
	if _, err := store.GetCredential(ctx, "bob"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound for credential after MigrateDown rollback, got %v", err)
	}

	// Reopen file in separate DAO instance: must be version 0 and empty
	reopened, err := NewNoSQLDAO(filePath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	ver, err := reopened.MigrationVersion(ctx)
	if err != nil || ver != 0 {
		t.Fatalf("expected version 0 on reopened store, got %d, err %v", ver, err)
	}
	if _, err := reopened.GetProfile(ctx, id); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound on reopened store after rollback, got %v", err)
	}
}



