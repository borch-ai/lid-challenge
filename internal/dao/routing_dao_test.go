package dao

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/borch-ai/lid-challenge/internal/models"
	"github.com/borch-ai/lid-challenge/internal/security"
)

func TestParsePersistenceMode(t *testing.T) {
	cases := []struct {
		input    string
		expected PersistenceMode
		wantErr  bool
	}{
		{"sql_only", PersistenceModeSQLOnly, false},
		{"SQL_ONLY", PersistenceModeSQLOnly, false},
		{"sql", PersistenceModeSQLOnly, false},
		{"", PersistenceModeSQLOnly, false},
		{"  ", PersistenceModeSQLOnly, false},
		{"dual_write", PersistenceModeDualWrite, false},
		{"dualwrite", PersistenceModeDualWrite, false},
		{"dual-write", PersistenceModeDualWrite, false},
		{"dual_write_nosql_primary", PersistenceModeDualWriteNoSQLPrimary, false},
		{"dualwrite_nosql", PersistenceModeDualWriteNoSQLPrimary, false},
		{"dual-write-nosql", PersistenceModeDualWriteNoSQLPrimary, false},
		{"nosql_only", PersistenceModeNoSQLOnly, false},
		{"nosql", PersistenceModeNoSQLOnly, false},
		{"document", PersistenceModeNoSQLOnly, false},
		{"invalid_mode", "", true},
		{"unknown", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			mode, err := ParsePersistenceMode(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got mode %q", tc.input, mode)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error for %q: %v", tc.input, err)
				}
				if mode != tc.expected {
					t.Errorf("expected %q, got %q", tc.expected, mode)
				}
			}
		})
	}
}

func TestNewRoutingDAO_Validation(t *testing.T) {
	sqlDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite: %v", err)
	}
	defer func() { _ = sqlDAO.Close() }()

	nosqlDAO, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create nosql: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()

	// Invalid mode
	if _, err := NewRoutingDAO("invalid", sqlDAO, nosqlDAO, nil); err == nil {
		t.Error("expected error for invalid mode")
	}

	// Nil sqlDAO in sql_only mode
	if _, err := NewRoutingDAO(PersistenceModeSQLOnly, nil, nosqlDAO, nil); err == nil {
		t.Error("expected error for nil sqlDAO in sql_only mode")
	}

	// Nil nosqlDAO in nosql_only mode
	if _, err := NewRoutingDAO(PersistenceModeNoSQLOnly, sqlDAO, nil, nil); err == nil {
		t.Error("expected error for nil nosqlDAO in nosql_only mode")
	}

	// DualWrite with nil secondary
	if _, err := NewRoutingDAO(PersistenceModeDualWrite, sqlDAO, nil, nil); err == nil {
		t.Error("expected error for dual write with nil nosqlDAO")
	}
	if _, err := NewRoutingDAO(PersistenceModeDualWriteNoSQLPrimary, nil, nosqlDAO, nil); err == nil {
		t.Error("expected error for dual write nosql primary with nil sqlDAO")
	}

	// Valid with nil logger
	r, err := NewRoutingDAO(PersistenceModeSQLOnly, sqlDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("unexpected error creating routing DAO: %v", err)
	}
	if r.Mode() != PersistenceModeSQLOnly {
		t.Errorf("expected mode %q, got %q", PersistenceModeSQLOnly, r.Mode())
	}
	if r.SQLDAO() != sqlDAO {
		t.Error("expected sqlDAO instance to match")
	}
	if r.NoSQLDAO() != nosqlDAO {
		t.Error("expected nosqlDAO instance to match")
	}
	if r.Primary() != sqlDAO {
		t.Error("expected active primary in sql_only to be sqlDAO")
	}
	if r.Secondary() != nil {
		t.Error("expected active secondary in sql_only to be nil")
	}

	// Mode switching
	if err := r.SetMode("invalid"); err == nil {
		t.Error("expected error setting invalid mode")
	}

	// Standalone SQL-only router without nosqlDAO
	rSQLOnly, err := NewRoutingDAO(PersistenceModeSQLOnly, sqlDAO, nil, nil)
	if err != nil {
		t.Fatalf("failed creating sql-only router: %v", err)
	}
	if err := rSQLOnly.SetMode(PersistenceModeDualWrite); err == nil {
		t.Error("expected error switching to dual write without nosqlDAO")
	}
	if err := rSQLOnly.SetMode(PersistenceModeNoSQLOnly); err == nil {
		t.Error("expected error switching to nosql_only without nosqlDAO")
	}

	// Standalone NoSQL-only router without sqlDAO
	rNoSQLOnly, err := NewRoutingDAO(PersistenceModeNoSQLOnly, nil, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed creating nosql-only router: %v", err)
	}
	if err := rNoSQLOnly.SetMode(PersistenceModeSQLOnly); err == nil {
		t.Error("expected error switching to sql_only without sqlDAO")
	}
}

// mockErrorDAO is a UserDAO stub that produces errors for testing resilience and error paths.
type mockErrorDAO struct {
	UserDAO
	createErr  error
	migrateErr error
	pingErr    error
	closeErr   error
	searchErr  error
	getProfErr error
	getCredErr error
	verifyErr  error
	upCount    int
	downCount  int
	versionErr error
}

func (m *mockErrorDAO) CreateUser(ctx context.Context, profile *models.UserProfile, cred *models.UserCredential) (string, error) {
	if m.createErr != nil {
		return "", m.createErr
	}
	return "mock-id", nil
}

func (m *mockErrorDAO) SearchProfiles(ctx context.Context, query models.SearchQuery) ([]*models.UserProfile, error) {
	if m.searchErr != nil {
		return nil, m.searchErr
	}
	return nil, nil
}

func (m *mockErrorDAO) GetProfile(ctx context.Context, userID string) (*models.UserProfile, error) {
	if m.getProfErr != nil {
		return nil, m.getProfErr
	}
	return nil, ErrUserNotFound
}

func (m *mockErrorDAO) GetCredential(ctx context.Context, username string) (*models.UserCredential, error) {
	if m.getCredErr != nil {
		return nil, m.getCredErr
	}
	return nil, ErrUserNotFound
}

func (m *mockErrorDAO) VerifyUserCredential(ctx context.Context, username, password string) (*models.UserProfile, error) {
	if m.verifyErr != nil {
		return nil, m.verifyErr
	}
	return nil, ErrUserNotFound
}

func (m *mockErrorDAO) Migrate(ctx context.Context) error {
	return m.migrateErr
}

func (m *mockErrorDAO) Ping(ctx context.Context) error {
	return m.pingErr
}

func (m *mockErrorDAO) Close() error {
	return m.closeErr
}

func (m *mockErrorDAO) MigrateUp(ctx context.Context) (int, error) {
	return m.upCount, m.migrateErr
}

func (m *mockErrorDAO) MigrateDown(ctx context.Context, steps int) (int, error) {
	return m.downCount, m.migrateErr
}

func (m *mockErrorDAO) MigrationVersion(ctx context.Context) (int64, error) {
	if m.versionErr != nil {
		return 0, m.versionErr
	}
	return 1, nil
}

func (m *mockErrorDAO) MigrationStatus(ctx context.Context) ([]MigrationStatus, error) {
	return nil, nil
}

func TestRoutingDAO_DualWrite_WithRealSQLiteAndNoSQL(t *testing.T) {
	ctx := context.Background()

	// 1. Initialize real in-memory SQLite DAO and migrate its schema
	sqliteDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite DAO: %v", err)
	}
	defer func() { _ = sqliteDAO.Close() }()
	if err := sqliteDAO.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate sqlite DAO: %v", err)
	}

	// 2. Initialize real in-memory NoSQL DAO
	nosqlDAO, err := NewNoSQLDAO("memory://")
	if err != nil {
		t.Fatalf("failed to create nosql DAO: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()
	if err := nosqlDAO.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate nosql DAO: %v", err)
	}

	// 3. Test DualWrite mode: SQLite primary, NoSQL secondary
	logger := slog.Default()
	r, err := NewRoutingDAO(PersistenceModeDualWrite, sqliteDAO, nosqlDAO, logger)
	if err != nil {
		t.Fatalf("failed to create routing DAO: %v", err)
	}

	if r.Primary() != sqliteDAO {
		t.Error("expected active primary in dual_write to be sqliteDAO")
	}
	if r.Secondary() != nosqlDAO {
		t.Error("expected active secondary in dual_write to be nosqlDAO")
	}

	hash, _ := security.HashPassword("secretpass")
	prof := &models.UserProfile{
		Name:  "David Copperfield",
		Phone: "+1-303-555-0199",
		Address: models.Address{
			StreetAddress: "777 Illusion Way",
			Locality:      "Denver",
			Region:        "CO",
			PostalCode:    "80202",
			Country:       "USA",
		},
	}
	cred := &models.UserCredential{Username: "david_c", PasswordHash: hash}

	// Test nil inputs
	if _, err := r.CreateUser(ctx, nil, cred); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
	if _, err := r.CreateUser(ctx, prof, nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}

	// Test canceled context
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.CreateUser(canceledCtx, prof, cred); err == nil {
		t.Error("expected error for canceled context")
	}

	// Create user in dual-write mode
	id, err := r.CreateUser(ctx, prof, cred)
	if err != nil {
		t.Fatalf("failed to create user in dual-write mode: %v", err)
	}

	// Verify SQL datastore received the write
	sqlProf, err := sqliteDAO.GetProfile(ctx, id)
	if err != nil || sqlProf.Name != "David Copperfield" {
		t.Fatalf("expected profile in sqlite datastore: %v, err: %v", sqlProf, err)
	}

	// Verify NoSQL datastore received the replicated write
	noSQLProf, err := nosqlDAO.GetProfile(ctx, id)
	if err != nil || noSQLProf.Name != "David Copperfield" {
		t.Fatalf("expected profile in nosql datastore: %v, err: %v", noSQLProf, err)
	}

	// Reads should delegate cleanly to primary (SQLite)
	readProf, err := r.GetProfile(ctx, id)
	if err != nil || readProf.Name != "David Copperfield" {
		t.Fatalf("expected GetProfile to read from primary: %v, err: %v", readProf, err)
	}

	searchRes, err := r.SearchProfiles(ctx, models.SearchQuery{Locality: "Denver"})
	if err != nil || len(searchRes) != 1 {
		t.Fatalf("expected SearchProfiles to read from primary: %v, err: %v", searchRes, err)
	}

	credRetrieved, err := r.GetCredential(ctx, "david_c")
	if err != nil || credRetrieved.UserID != id {
		t.Fatalf("expected GetCredential to read from primary: %v, err: %v", credRetrieved, err)
	}

	authProf, err := r.VerifyUserCredential(ctx, "david_c", "secretpass")
	if err != nil || authProf.ID != id {
		t.Fatalf("expected VerifyUserCredential to verify against primary: %v, err: %v", authProf, err)
	}
}

func TestRoutingDAO_DualWriteNoSQLPrimary_WithRealSQLiteAndNoSQL(t *testing.T) {
	ctx := context.Background()

	sqliteDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite: %v", err)
	}
	defer func() { _ = sqliteDAO.Close() }()
	if err := sqliteDAO.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate sqlite: %v", err)
	}

	nosqlDAO, err := NewNoSQLDAO("memory://")
	if err != nil {
		t.Fatalf("failed to create nosql: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()
	_ = nosqlDAO.Migrate(ctx)

	r, err := NewRoutingDAO(PersistenceModeDualWriteNoSQLPrimary, sqliteDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed to create routing DAO: %v", err)
	}

	if r.Primary() != nosqlDAO {
		t.Error("expected active primary in dual_write_nosql_primary to be nosqlDAO")
	}
	if r.Secondary() != sqliteDAO {
		t.Error("expected active secondary in dual_write_nosql_primary to be sqliteDAO")
	}

	hash, _ := security.HashPassword("pw")
	prof := &models.UserProfile{
		Name:  "NoSQL Primary User",
		Phone: "+1-555-0123",
		Address: models.Address{
			StreetAddress: "123 Cloud Way",
			Locality:      "Denver",
			Region:        "CO",
			PostalCode:    "80202",
			Country:       "USA",
		},
	}
	cred := &models.UserCredential{Username: "nosql_prim", PasswordHash: hash}

	id, err := r.CreateUser(ctx, prof, cred)
	if err != nil {
		t.Fatalf("unexpected error creating user: %v", err)
	}

	// Verify record in NoSQL primary
	if p, err := nosqlDAO.GetProfile(ctx, id); err != nil || p.Name != "NoSQL Primary User" {
		t.Errorf("expected user in nosql primary: %v, err: %v", p, err)
	}
	// Verify record in SQL secondary
	if p, err := sqliteDAO.GetProfile(ctx, id); err != nil || p.Name != "NoSQL Primary User" {
		t.Errorf("expected user in sql secondary: %v, err: %v", p, err)
	}

	// Reads should delegate to NoSQL primary
	p, err := r.GetProfile(ctx, id)
	if err != nil || p.Name != "NoSQL Primary User" {
		t.Errorf("expected GetProfile to read from NoSQL primary: %v, err: %v", p, err)
	}
}

func TestRoutingDAO_DynamicModeSwitching(t *testing.T) {
	ctx := context.Background()

	sqliteDAO, _ := NewSQLiteDAO("file::memory:?cache=shared")
	defer func() { _ = sqliteDAO.Close() }()
	_ = sqliteDAO.Migrate(ctx)

	nosqlDAO, _ := NewNoSQLDAO("memory://")
	defer func() { _ = nosqlDAO.Close() }()
	_ = nosqlDAO.Migrate(ctx)

	r, err := NewRoutingDAO(PersistenceModeSQLOnly, sqliteDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed to create routing DAO: %v", err)
	}

	hash, _ := security.HashPassword("pw")

	// Phase 1: SQLOnly mode
	id1, err := r.CreateUser(ctx, &models.UserProfile{Name: "User 1", Phone: "+1-555-0001"}, &models.UserCredential{Username: "u1", PasswordHash: hash})
	if err != nil {
		t.Fatalf("failed writing in sql_only: %v", err)
	}
	if _, err := sqliteDAO.GetProfile(ctx, id1); err != nil {
		t.Errorf("expected user1 in sqlite")
	}
	if _, err := nosqlDAO.GetProfile(ctx, id1); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected user1 NOT in nosql in sql_only mode")
	}

	// Phase 2: Switch to NoSQLOnly mode
	if err := r.SetMode(PersistenceModeNoSQLOnly); err != nil {
		t.Fatalf("failed switching to nosql_only: %v", err)
	}
	if r.Primary() != nosqlDAO {
		t.Error("expected active primary to be nosqlDAO after cutover")
	}

	id2, err := r.CreateUser(ctx, &models.UserProfile{Name: "User 2", Phone: "+1-555-0002"}, &models.UserCredential{Username: "u2", PasswordHash: hash})
	if err != nil {
		t.Fatalf("failed writing in nosql_only: %v", err)
	}
	if _, err := nosqlDAO.GetProfile(ctx, id2); err != nil {
		t.Errorf("expected user2 in nosql")
	}
	if _, err := sqliteDAO.GetProfile(ctx, id2); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected user2 NOT in sqlite in nosql_only mode")
	}

	// Reads now serve from NoSQL
	p2, err := r.GetProfile(ctx, id2)
	if err != nil || p2.Name != "User 2" {
		t.Errorf("expected GetProfile to serve from NoSQL after cutover: %v, err: %v", p2, err)
	}
}

func TestRoutingDAO_SecondaryWriteFailureResilience(t *testing.T) {
	ctx := context.Background()
	primary, _ := NewSQLiteDAO("file::memory:?cache=shared")
	defer func() { _ = primary.Close() }()
	_ = primary.Migrate(ctx)

	failingSecondary := &mockErrorDAO{createErr: errors.New("secondary datastore timeout")}

	r, err := NewRoutingDAO(PersistenceModeDualWrite, primary, failingSecondary, nil)
	if err != nil {
		t.Fatalf("failed to create routing DAO: %v", err)
	}

	var secondaryErrorCount atomic.Int32
	r.SetOnSecondaryError(func(op string, err error) {
		if op == "CreateUser" {
			secondaryErrorCount.Add(1)
		}
	})

	hash, _ := security.HashPassword("secretpass")
	prof := &models.UserProfile{Name: "Resilient User", Phone: "+1-555-0003"}
	cred := &models.UserCredential{Username: "resilient_user", PasswordHash: hash}

	// When secondary fails, primary write should still succeed without returning an error
	id, err := r.CreateUser(ctx, prof, cred)
	if err != nil {
		t.Fatalf("expected primary write to succeed despite secondary error, got: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty generated ID")
	}

	if secondaryErrorCount.Load() != 1 {
		t.Errorf("expected 1 secondary error callback, got %d", secondaryErrorCount.Load())
	}
}

func TestRoutingDAO_PrimaryWriteFailure(t *testing.T) {
	ctx := context.Background()
	failingPrimary := &mockErrorDAO{createErr: errors.New("primary db down")}
	secondary, _ := NewNoSQLDAO("")

	r, err := NewRoutingDAO(PersistenceModeDualWrite, failingPrimary, secondary, nil)
	if err != nil {
		t.Fatalf("failed to create routing DAO: %v", err)
	}

	prof := &models.UserProfile{Name: "Fail User", Phone: "+1-555-0004"}
	cred := &models.UserCredential{Username: "fail_user"}

	_, err = r.CreateUser(ctx, prof, cred)
	if err == nil || !errors.Is(err, failingPrimary.createErr) {
		t.Fatalf("expected error from failing primary, got: %v", err)
	}
}

func TestRoutingDAO_MigratePingClose(t *testing.T) {
	ctx := context.Background()
	sqliteDAO, _ := NewSQLiteDAO("file::memory:?cache=shared")
	nosqlDAO, _ := NewNoSQLDAO("")

	r, err := NewRoutingDAO(PersistenceModeDualWrite, sqliteDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed to create routing DAO: %v", err)
	}

	if err := r.Migrate(ctx); err != nil {
		t.Fatalf("expected successful Migrate: %v", err)
	}
	if err := r.Ping(ctx); err != nil {
		t.Fatalf("expected successful Ping: %v", err)
	}

	// MigratableDAO methods
	count, err := r.MigrateUp(ctx)
	if err != nil || count < 0 {
		t.Fatalf("expected MigrateUp to succeed, got %d, err: %v", count, err)
	}

	downCount, err := r.MigrateDown(ctx, 1)
	if err != nil || downCount < 0 {
		t.Fatalf("expected MigrateDown to succeed, got %d, err: %v", downCount, err)
	}

	ver, err := r.MigrationVersion(ctx)
	if err != nil || ver < 0 {
		t.Fatalf("expected non-negative MigrationVersion, got %d, err: %v", ver, err)
	}

	statuses, err := r.MigrationStatus(ctx)
	if err != nil || len(statuses) == 0 {
		t.Fatalf("expected non-empty migration statuses, got %v, err: %v", statuses, err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("expected successful Close: %v", err)
	}
}

func TestRoutingDAO_ErrorDelegation(t *testing.T) {
	ctx := context.Background()

	// Primary migrate failure
	pFailMig := &mockErrorDAO{migrateErr: errors.New("migrate primary failed")}
	r1, _ := NewRoutingDAO(PersistenceModeDualWrite, pFailMig, &mockErrorDAO{}, nil)
	if err := r1.Migrate(ctx); err == nil {
		t.Error("expected error on primary migrate failure")
	}
	if _, err := r1.MigrateUp(ctx); err == nil {
		t.Error("expected error on primary migrate up failure")
	}

	// Secondary migrate failure
	sFailMig := &mockErrorDAO{migrateErr: errors.New("migrate sec failed")}
	pOk := &mockErrorDAO{}
	r2, _ := NewRoutingDAO(PersistenceModeDualWrite, pOk, sFailMig, nil)
	if err := r2.Migrate(ctx); err == nil {
		t.Error("expected error on secondary migrate failure")
	}
	if _, err := r2.MigrateUp(ctx); err == nil {
		t.Error("expected error on secondary migrate up failure")
	}

	// Migrate down failures
	if _, err := r1.MigrateDown(ctx, 1); err == nil {
		t.Error("expected error on primary migrate down failure")
	}
	if _, err := r2.MigrateDown(ctx, 1); err == nil {
		t.Error("expected error on secondary migrate down failure")
	}

	// Ping failures
	pFailPing := &mockErrorDAO{pingErr: errors.New("ping primary failed")}
	r3, _ := NewRoutingDAO(PersistenceModeDualWrite, pFailPing, &mockErrorDAO{}, nil)
	if err := r3.Ping(ctx); err == nil {
		t.Error("expected error on primary ping failure")
	}

	sFailPing := &mockErrorDAO{pingErr: errors.New("ping sec failed")}
	r4, _ := NewRoutingDAO(PersistenceModeDualWrite, &mockErrorDAO{}, sFailPing, nil)
	if err := r4.Ping(ctx); err == nil {
		t.Error("expected error on secondary ping failure")
	}

	// Close failures
	pFailClose := &mockErrorDAO{closeErr: errors.New("close primary err")}
	sFailClose := &mockErrorDAO{closeErr: errors.New("close sec err")}
	r5, _ := NewRoutingDAO(PersistenceModeDualWrite, pFailClose, sFailClose, nil)
	if err := r5.Close(); err == nil {
		t.Error("expected error when both primary and secondary close fail")
	}
}

func TestRoutingDAO_Migratable_AccumulatesCounts(t *testing.T) {
	ctx := context.Background()
	primary := &mockErrorDAO{upCount: 2, downCount: 1}
	secondary := &mockErrorDAO{upCount: 3, downCount: 2}

	r, err := NewRoutingDAO(PersistenceModeDualWrite, primary, secondary, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	upCount, err := r.MigrateUp(ctx)
	if err != nil {
		t.Fatalf("MigrateUp failed: %v", err)
	}
	if upCount != 5 {
		t.Fatalf("expected upCount=5 (2+3), got %d", upCount)
	}

	downCount, err := r.MigrateDown(ctx, 1)
	if err != nil {
		t.Fatalf("MigrateDown failed: %v", err)
	}
	if downCount != 3 {
		t.Fatalf("expected downCount=3 (1+2), got %d", downCount)
	}
}

func TestRoutingDAO_Migratable_StandaloneWithSecondary(t *testing.T) {
	ctx := context.Background()
	sqlPrimary := &mockErrorDAO{upCount: 1, downCount: 1}
	nosqlSecondary := &mockErrorDAO{upCount: 2, downCount: 2}

	// In SQL-only mode with a secondary NoSQL datastore configured
	r, err := NewRoutingDAO(PersistenceModeSQLOnly, sqlPrimary, nosqlSecondary, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	upCount, err := r.MigrateUp(ctx)
	if err != nil {
		t.Fatalf("MigrateUp failed: %v", err)
	}
	if upCount != 3 {
		t.Fatalf("expected upCount=3 (1+2) migrating both configured datastores in standalone mode, got %d", upCount)
	}

	downCount, err := r.MigrateDown(ctx, 1)
	if err != nil {
		t.Fatalf("MigrateDown failed: %v", err)
	}
	if downCount != 3 {
		t.Fatalf("expected downCount=3 (1+2), got %d", downCount)
	}
}

func TestRoutingDAO_MigrateDown_CoordinatedRollback(t *testing.T) {
	ctx := context.Background()

	sqliteDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite dao: %v", err)
	}
	defer func() { _ = sqliteDAO.Close() }()

	nosqlDAO, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create nosql dao: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()

	r, err := NewRoutingDAO(PersistenceModeDualWrite, sqliteDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	// 1. Migrate both backends: SQLite -> v2, NoSQL -> v1
	if err := r.Migrate(ctx); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	sqlVer, _ := sqliteDAO.MigrationVersion(ctx)
	nosqlVer, _ := nosqlDAO.MigrationVersion(ctx)
	if sqlVer != 2 || nosqlVer != 1 {
		t.Fatalf("expected sqlVer=2, nosqlVer=1, got sqlVer=%d, nosqlVer=%d", sqlVer, nosqlVer)
	}

	// 2. Create user in dual-write mode
	prof := &models.UserProfile{
		Name:  "Rollback Tester",
		Phone: "+1-555-0100",
		Address: models.Address{
			StreetAddress: "123 Rollback St",
			Locality:      "Denver",
			Region:        "CO",
			PostalCode:    "80202",
			Country:       "USA",
		},
	}
	cred := &models.UserCredential{
		Username:     "rollbackuser",
		PasswordHash: "hashedpass",
	}
	userID, err := r.CreateUser(ctx, prof, cred)
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Verify user is in both stores
	if _, err := sqliteDAO.GetProfile(ctx, userID); err != nil {
		t.Fatalf("expected user in sqlite: %v", err)
	}
	if _, err := nosqlDAO.GetProfile(ctx, userID); err != nil {
		t.Fatalf("expected user in nosql: %v", err)
	}

	// 3. Roll down 1 step: only SQLite rolls down v2 -> v1 (dropping index).
	// NoSQL must NOT be rolled down (its sole migration is base collection v1 -> v0).
	downCount, err := r.MigrateDown(ctx, 1)
	if err != nil {
		t.Fatalf("MigrateDown(1) failed: %v", err)
	}
	if downCount != 1 {
		t.Fatalf("expected downCount=1 (only sql dropped secondary index), got %d", downCount)
	}

	sqlVer, _ = sqliteDAO.MigrationVersion(ctx)
	nosqlVer, _ = nosqlDAO.MigrationVersion(ctx)
	if sqlVer != 1 {
		t.Fatalf("expected sqlVer=1, got %d", sqlVer)
	}
	if nosqlVer != 1 {
		t.Fatalf("expected nosqlVer=1 (retained collection), got %d", nosqlVer)
	}

	// User must still exist in both datastores!
	if p, err := r.GetProfile(ctx, userID); err != nil || p == nil {
		t.Fatalf("expected user still accessible via routing dao after sql index rollback: %v", err)
	}
	if p, err := nosqlDAO.GetProfile(ctx, userID); err != nil || p == nil {
		t.Fatalf("expected user still in nosql after sql index rollback: %v", err)
	}

	// 4. Roll down 1 more step: SQL drops base tables (v1 -> v0), NoSQL drops collection (v1 -> v0).
	downCount, err = r.MigrateDown(ctx, 1)
	if err != nil {
		t.Fatalf("MigrateDown(1) second step failed: %v", err)
	}
	if downCount != 2 {
		t.Fatalf("expected downCount=2 (1 sql + 1 nosql), got %d", downCount)
	}

	sqlVer, _ = sqliteDAO.MigrationVersion(ctx)
	nosqlVer, _ = nosqlDAO.MigrationVersion(ctx)
	if sqlVer != 0 || nosqlVer != 0 {
		t.Fatalf("expected sqlVer=0 and nosqlVer=0, got sqlVer=%d, nosqlVer=%d", sqlVer, nosqlVer)
	}

	// 5. Roll down when already at v0 should return 0, nil
	downCount, err = r.MigrateDown(ctx, 1)
	if err != nil || downCount != 0 {
		t.Fatalf("expected downCount=0 when already at 0, got %d, err: %v", downCount, err)
	}

	// 6. Test multi-step rollback from full migration
	if err := r.Migrate(ctx); err != nil {
		t.Fatalf("Migrate failed on re-migrate: %v", err)
	}
	downCount, err = r.MigrateDown(ctx, 2)
	if err != nil {
		t.Fatalf("MigrateDown(2) failed: %v", err)
	}
	if downCount != 3 {
		t.Fatalf("expected downCount=3 (2 sql + 1 nosql), got %d", downCount)
	}
}

func TestRoutingDAO_MigrateDown_EdgeCasesAndErrors(t *testing.T) {
	ctx := context.Background()

	// 1. steps <= 0
	r, err := NewRoutingDAO(PersistenceModeDualWrite, &mockErrorDAO{}, &mockErrorDAO{}, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}
	count, err := r.MigrateDown(ctx, 0)
	if err != nil || count != 0 {
		t.Fatalf("expected count=0, nil err for steps=0, got %d, %v", count, err)
	}
	count, err = r.MigrateDown(ctx, -1)
	if err != nil || count != 0 {
		t.Fatalf("expected count=0, nil err for steps=-1, got %d, %v", count, err)
	}

	// 2. Canceled context
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.MigrateDown(canceledCtx, 1); err == nil {
		t.Error("expected error with canceled context on MigrateDown")
	}

	// 3. SQL MigrationVersion error
	sqlErrDAO := &mockErrorDAO{versionErr: errors.New("sql version query failed")}
	nosqlOKDAO := &mockErrorDAO{}
	rSQLErr, _ := NewRoutingDAO(PersistenceModeDualWrite, sqlErrDAO, nosqlOKDAO, nil)
	if _, err := rSQLErr.MigrateDown(ctx, 1); err == nil || !strings.Contains(err.Error(), "failed to get sql migration version") {
		t.Errorf("expected sql migration version error, got: %v", err)
	}

	// 4. NoSQL MigrationVersion error
	nosqlErrDAO := &mockErrorDAO{versionErr: errors.New("nosql version query failed")}
	sqlOKDAO := &mockErrorDAO{}
	rNoSQLErr, _ := NewRoutingDAO(PersistenceModeDualWrite, sqlOKDAO, nosqlErrDAO, nil)
	if _, err := rNoSQLErr.MigrateDown(ctx, 1); err == nil || !strings.Contains(err.Error(), "failed to get nosql migration version") {
		t.Errorf("expected nosql migration version error, got: %v", err)
	}
}

func TestRoutingDAO_SQLFallback_WhenNoSQLPrimaryMissingRecord(t *testing.T) {
	ctx := context.Background()
	sqlDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite dao: %v", err)
	}
	defer func() { _ = sqlDAO.Close() }()
	if err := sqlDAO.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate sqlite: %v", err)
	}

	nosqlDAO, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create nosql dao: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()

	// Pre-create user in SQL only (simulating pre-existing data created prior to cutover)
	hash, _ := security.HashPassword("sqlpass")
	pSQL := &models.UserProfile{Name: "PreExisting User", Phone: "+1-555-0999"}
	cSQL := &models.UserCredential{Username: "preexisting_user", PasswordHash: hash}
	userID, err := sqlDAO.CreateUser(ctx, pSQL, cSQL)
	if err != nil {
		t.Fatalf("failed to create user in SQL: %v", err)
	}

	// Router operating with NoSQL primary and SQL fallback
	r, err := NewRoutingDAO(PersistenceModeDualWriteNoSQLPrimary, sqlDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	// 1. GetProfile falls back to SQL
	prof, err := r.GetProfile(ctx, userID)
	if err != nil || prof.Name != "PreExisting User" {
		t.Errorf("expected SQL fallback for GetProfile, got prof %+v, err %v", prof, err)
	}

	// 2. GetCredential falls back to SQL
	cred, err := r.GetCredential(ctx, "preexisting_user")
	if err != nil || cred.UserID != userID {
		t.Errorf("expected SQL fallback for GetCredential, got cred %+v, err %v", cred, err)
	}

	// 3. VerifyUserCredential falls back to SQL
	authProf, err := r.VerifyUserCredential(ctx, "preexisting_user", "sqlpass")
	if err != nil || authProf.ID != userID {
		t.Errorf("expected SQL fallback for VerifyUserCredential, got %+v, err %v", authProf, err)
	}

	// 4. SearchProfiles falls back to SQL
	results, err := r.SearchProfiles(ctx, models.SearchQuery{Name: "PreExisting"})
	if err != nil || len(results) != 1 || results[0].ID != userID {
		t.Errorf("expected SQL fallback for SearchProfiles, got %+v, err %v", results, err)
	}
}

func TestRoutingDAO_NormalizedReplication_NoSQLPrimary(t *testing.T) {
	ctx := context.Background()
	sqlDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite dao: %v", err)
	}
	defer func() { _ = sqlDAO.Close() }()
	if err := sqlDAO.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate sqlite: %v", err)
	}

	nosqlDAO, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create nosql dao: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()
	_ = nosqlDAO.Migrate(ctx)

	r, err := NewRoutingDAO(PersistenceModeDualWriteNoSQLPrimary, sqlDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	hash, _ := security.HashPassword("normpass")
	p := &models.UserProfile{Name: "Norm User", Phone: "+1-555-0888"}
	c := &models.UserCredential{Username: "norm_user", PasswordHash: hash}

	id, err := r.CreateUser(ctx, p, c)
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Fetch from primary (NoSQL) and secondary (SQL)
	pNoSQL, err := nosqlDAO.GetProfile(ctx, id)
	if err != nil {
		t.Fatalf("failed to get from primary: %v", err)
	}
	pSQL, err := sqlDAO.GetProfile(ctx, id)
	if err != nil {
		t.Fatalf("failed to get from secondary: %v", err)
	}

	// Timestamps must match exactly between primary and secondary
	if !pNoSQL.CreatedAt.Equal(pSQL.CreatedAt) {
		t.Errorf("expected matching CreatedAt timestamps, got NoSQL %v vs SQL %v", pNoSQL.CreatedAt, pSQL.CreatedAt)
	}
}

func TestRoutingDAO_ReadFallback_NoSQLToSQL(t *testing.T) {
	ctx := context.Background()
	sqlDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite: %v", err)
	}
	defer func() { _ = sqlDAO.Close() }()
	if err := sqlDAO.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate sqlite: %v", err)
	}

	nosqlDAO, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create nosql: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()
	_ = nosqlDAO.Migrate(ctx)

	// Directly insert a user into NoSQL only (e.g., written during nosql_only mode)
	hash, _ := security.HashPassword("nosqlpass")
	pNoSQL := &models.UserProfile{Name: "NoSQL Only User", Phone: "+1-555-0999"}
	cNoSQL := &models.UserCredential{Username: "nosql_user", PasswordHash: hash}
	userID, err := nosqlDAO.CreateUser(ctx, pNoSQL, cNoSQL)
	if err != nil {
		t.Fatalf("failed to create nosql user: %v", err)
	}

	// Router is configured with SQL primary (e.g. switched to sql_only or dual_write)
	r, err := NewRoutingDAO(PersistenceModeSQLOnly, sqlDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	// 1. GetProfile falls back to NoSQL
	prof, err := r.GetProfile(ctx, userID)
	if err != nil || prof.Name != "NoSQL Only User" {
		t.Errorf("expected NoSQL fallback for GetProfile, got prof %+v, err %v", prof, err)
	}

	// 2. GetCredential falls back to NoSQL
	cred, err := r.GetCredential(ctx, "nosql_user")
	if err != nil || cred.UserID != userID {
		t.Errorf("expected NoSQL fallback for GetCredential, got cred %+v, err %v", cred, err)
	}

	// 3. VerifyUserCredential falls back to NoSQL
	authProf, err := r.VerifyUserCredential(ctx, "nosql_user", "nosqlpass")
	if err != nil || authProf.ID != userID {
		t.Errorf("expected NoSQL fallback for VerifyUserCredential, got %+v, err %v", authProf, err)
	}

	// Wrong password returns ErrInvalidPassword, not ErrUserNotFound
	if _, err := r.VerifyUserCredential(ctx, "nosql_user", "wrongpass"); !errors.Is(err, security.ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword for wrong password via fallback, got %v", err)
	}

	// 4. SearchProfiles falls back to NoSQL
	results, err := r.SearchProfiles(ctx, models.SearchQuery{Name: "NoSQL Only"})
	if err != nil || len(results) != 1 || results[0].ID != userID {
		t.Errorf("expected NoSQL fallback for SearchProfiles, got %+v, err %v", results, err)
	}
}

func TestRoutingDAO_SearchProfiles_MergeAndDeduplicate(t *testing.T) {
	ctx := context.Background()
	sqlDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite: %v", err)
	}
	defer func() { _ = sqlDAO.Close() }()
	if err := sqlDAO.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate sqlite: %v", err)
	}

	nosqlDAO, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create nosql: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()
	_ = nosqlDAO.Migrate(ctx)

	hash, _ := security.HashPassword("pass")

	// User 1 only in SQL
	p1 := &models.UserProfile{ID: "user-sql-1", Name: "Shared Alice", Phone: "+1-555-0201"}
	c1 := &models.UserCredential{Username: "alice_sql", PasswordHash: hash}
	if _, err := sqlDAO.CreateUser(ctx, p1, c1); err != nil {
		t.Fatalf("failed creating sql user: %v", err)
	}

	// User 2 only in NoSQL
	p2 := &models.UserProfile{ID: "user-nosql-2", Name: "Shared Bob", Phone: "+1-555-0202"}
	c2 := &models.UserCredential{Username: "bob_nosql", PasswordHash: hash}
	if _, err := nosqlDAO.CreateUser(ctx, p2, c2); err != nil {
		t.Fatalf("failed creating nosql user: %v", err)
	}

	// User 3 in both SQL and NoSQL (already backfilled/replicated)
	p3 := &models.UserProfile{ID: "user-both-3", Name: "Shared Charlie", Phone: "+1-555-0203"}
	c3 := &models.UserCredential{Username: "charlie_both", PasswordHash: hash}
	if _, err := sqlDAO.CreateUser(ctx, p3, c3); err != nil {
		t.Fatalf("failed creating user 3 in sql: %v", err)
	}
	if _, err := nosqlDAO.CreateUser(ctx, p3, c3); err != nil {
		t.Fatalf("failed creating user 3 in nosql: %v", err)
	}

	r, err := NewRoutingDAO(PersistenceModeDualWriteNoSQLPrimary, sqlDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}

	// Search matching "Shared" across both datastores
	results, err := r.SearchProfiles(ctx, models.SearchQuery{Name: "Shared", Limit: 10})
	if err != nil {
		t.Fatalf("SearchProfiles failed: %v", err)
	}

	// Must return exactly 3 deduplicated results
	if len(results) != 3 {
		t.Fatalf("expected 3 merged and deduplicated results, got %d", len(results))
	}

	ids := make(map[string]bool)
	for _, p := range results {
		if ids[p.ID] {
			t.Errorf("duplicate ID %q returned in search results", p.ID)
		}
		ids[p.ID] = true
	}
	if !ids["user-sql-1"] || !ids["user-nosql-2"] || !ids["user-both-3"] {
		t.Errorf("missing expected user IDs in results: %v", ids)
	}

	// Test pagination on merged results: offset 1, limit 1
	paged, err := r.SearchProfiles(ctx, models.SearchQuery{Name: "Shared", Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("paginated SearchProfiles failed: %v", err)
	}
	if len(paged) != 1 {
		t.Fatalf("expected 1 paginated result, got %d", len(paged))
	}
	if paged[0].ID != results[1].ID {
		t.Errorf("expected paginated result %q to match offset 1 from full results %q", paged[0].ID, results[1].ID)
	}
}

func TestRoutingDAO_SearchProfiles_ExceedsSinglePageCap(t *testing.T) {
	ctx := context.Background()
	sqlDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite: %v", err)
	}
	defer func() { _ = sqlDAO.Close() }()
	if err := sqlDAO.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate sqlite: %v", err)
	}

	nosqlDAO, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create nosql: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()
	_ = nosqlDAO.Migrate(ctx)

	hash, _ := security.HashPassword("pass")

	// Insert 105 users into SQL and 15 into NoSQL (total 120)
	for i := 1; i <= 105; i++ {
		p := &models.UserProfile{ID: fmt.Sprintf("sql-%03d", i), Name: "PageTest User", Phone: fmt.Sprintf("+1-555-%04d", i)}
		c := &models.UserCredential{Username: fmt.Sprintf("sql_user_%03d", i), PasswordHash: hash}
		if _, err := sqlDAO.CreateUser(ctx, p, c); err != nil {
			t.Fatalf("failed creating sql user %d: %v", i, err)
		}
	}
	for i := 106; i <= 120; i++ {
		p := &models.UserProfile{ID: fmt.Sprintf("nosql-%03d", i), Name: "PageTest User", Phone: fmt.Sprintf("+1-555-%04d", i)}
		c := &models.UserCredential{Username: fmt.Sprintf("nosql_user_%03d", i), PasswordHash: hash}
		if _, err := nosqlDAO.CreateUser(ctx, p, c); err != nil {
			t.Fatalf("failed creating nosql user %d: %v", i, err)
		}
	}

	r, err := NewRoutingDAO(PersistenceModeDualWriteNoSQLPrimary, sqlDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}

	// Request offset=100, limit=20 (targeting items 100..119 out of 120 total)
	pageResults, err := r.SearchProfiles(ctx, models.SearchQuery{Name: "PageTest", Offset: 100, Limit: 20})
	if err != nil {
		t.Fatalf("SearchProfiles beyond page cap failed: %v", err)
	}

	if len(pageResults) != 20 {
		t.Fatalf("expected 20 items for offset 100 and limit 20 beyond 100-page cap, got %d", len(pageResults))
	}
}

func TestRoutingDAO_SearchProfiles_ErrorAndEdgeCases(t *testing.T) {
	ctx := context.Background()

	// 1. Router with no primary configured
	emptyRouter := &RoutingDAO{}
	if _, err := emptyRouter.SearchProfiles(ctx, models.SearchQuery{}); err == nil {
		t.Error("expected error when searching profiles with no primary configured")
	}
	if _, err := emptyRouter.GetProfile(ctx, "any"); err == nil {
		t.Error("expected error when getting profile with no primary configured")
	}
	if _, err := emptyRouter.GetCredential(ctx, "any"); err == nil {
		t.Error("expected error when getting credential with no primary configured")
	}
	if _, err := emptyRouter.VerifyUserCredential(ctx, "any", "pw"); err == nil {
		t.Error("expected error when verifying credential with no primary configured")
	}

	// 2. Primary fails on search
	primaryFail := &mockErrorDAO{searchErr: errors.New("primary boom")}
	fallbackStub := &mockErrorDAO{}
	rFail, _ := NewRoutingDAO(PersistenceModeDualWrite, primaryFail, fallbackStub, nil)
	if _, err := rFail.SearchProfiles(ctx, models.SearchQuery{}); err == nil {
		t.Error("expected error when primary fails during SearchProfiles")
	}

	// 3. Fallback fails on search: primary succeeds, fallback error is handled gracefully
	nosqlDAO, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create nosql: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()
	_ = nosqlDAO.Migrate(ctx)

	hash, _ := security.HashPassword("pass")
	if _, err := nosqlDAO.CreateUser(ctx, &models.UserProfile{ID: "p1", Name: "Alpha", Phone: "111"}, &models.UserCredential{Username: "u1", PasswordHash: hash}); err != nil {
		t.Fatalf("failed creating p1: %v", err)
	}
	if _, err := nosqlDAO.CreateUser(ctx, &models.UserProfile{ID: "p2", Name: "Beta", Phone: "222"}, &models.UserCredential{Username: "u2", PasswordHash: hash}); err != nil {
		t.Fatalf("failed creating p2: %v", err)
	}

	fallbackFail := &mockErrorDAO{searchErr: errors.New("fallback down")}
	rFallbackFail, _ := NewRoutingDAO(PersistenceModeDualWriteNoSQLPrimary, fallbackFail, nosqlDAO, nil)

	// Subcase A: offset >= len(primaryResults) -> empty slice
	resA, err := rFallbackFail.SearchProfiles(ctx, models.SearchQuery{Offset: 5, Limit: 10})
	if err != nil || len(resA) != 0 {
		t.Errorf("expected empty slice when offset exceeds primary results during fallback failure, got %+v, err %v", resA, err)
	}

	// Subcase B: end > len(primaryResults) -> returns all available primary results
	resB, err := rFallbackFail.SearchProfiles(ctx, models.SearchQuery{Offset: 0, Limit: 10})
	if err != nil || len(resB) != 2 {
		t.Errorf("expected 2 primary results when limit exceeds count during fallback failure, got %+v, err %v", resB, err)
	}

	// 4. offset >= len(merged) when both datastores succeed
	rBoth, _ := NewRoutingDAO(PersistenceModeDualWriteNoSQLPrimary, fallbackStub, nosqlDAO, nil)
	resBeyond, err := rBoth.SearchProfiles(ctx, models.SearchQuery{Offset: 10, Limit: 5})
	if err != nil || len(resBeyond) != 0 {
		t.Errorf("expected empty slice when offset exceeds total merged count, got %+v, err %v", resBeyond, err)
	}

	// 5. Standalone fallback accessor behavior
	sqlDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite: %v", err)
	}
	defer func() { _ = sqlDAO.Close() }()

	rSQLSolo, _ := NewRoutingDAO(PersistenceModeSQLOnly, sqlDAO, nil, nil)
	if fb := rSQLSolo.fallback(); fb != nil {
		t.Errorf("expected nil fallback for SQL-only without NoSQL, got %+v", fb)
	}

	rNoSQLSolo, _ := NewRoutingDAO(PersistenceModeNoSQLOnly, nil, nosqlDAO, nil)
	if fb := rNoSQLSolo.fallback(); fb != nil {
		t.Errorf("expected nil fallback for NoSQL-only without SQL, got %+v", fb)
	}
}

func TestRoutingDAO_SearchProfiles_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel context

	nosqlDAO, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create nosql dao: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()

	sqlDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite dao: %v", err)
	}
	defer func() { _ = sqlDAO.Close() }()

	r, err := NewRoutingDAO(PersistenceModeDualWrite, sqlDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	if _, err := r.SearchProfiles(ctx, models.SearchQuery{}); !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled on pre-canceled context, got: %v", err)
	}

	// Test cancellation during fallback query
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	fallbackCanceled := &mockErrorDAO{searchErr: context.Canceled}
	rFallbackCanceled, _ := NewRoutingDAO(PersistenceModeDualWriteNoSQLPrimary, fallbackCanceled, nosqlDAO, nil)
	if _, err := rFallbackCanceled.SearchProfiles(ctx2, models.SearchQuery{}); !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled propagated from fallback, got: %v", err)
	}
}

func TestRoutingDAO_AlternateDatastore_UsernameConflictPreflight(t *testing.T) {
	ctx := context.Background()
	hash, _ := security.HashPassword("secretpass")

	// Case 1: Standalone sql_only mode with configured NoSQL fallback
	// A user "historical_user" exists only in NoSQL. Attempting to create "historical_user" via RoutingDAO in sql_only
	// must be rejected with ErrUsernameTaken to prevent identity collision with fallback data.
	sqliteDAO1, _ := NewSQLiteDAO("file::memory:?cache=shared")
	defer func() { _ = sqliteDAO1.Close() }()
	_ = sqliteDAO1.Migrate(ctx)

	nosqlDAO1, _ := NewNoSQLDAO("memory://")
	defer func() { _ = nosqlDAO1.Close() }()
	_ = nosqlDAO1.Migrate(ctx)

	// Prepopulate NoSQL with "historical_user"
	_, err := nosqlDAO1.CreateUser(ctx, &models.UserProfile{ID: "nosql-u1", Name: "Historical NoSQL", Phone: "111"}, &models.UserCredential{Username: "historical_user", PasswordHash: hash})
	if err != nil {
		t.Fatalf("failed prepopulating NoSQL: %v", err)
	}

	rSQLOnly, err := NewRoutingDAO(PersistenceModeSQLOnly, sqliteDAO1, nosqlDAO1, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	_, err = rSQLOnly.CreateUser(ctx, &models.UserProfile{Name: "New SQL", Phone: "222"}, &models.UserCredential{Username: "historical_user", PasswordHash: hash})
	if !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("expected ErrUsernameTaken when username exists in NoSQL fallback, got: %v", err)
	}
	// Verify primary SQL datastore was not written to
	if _, err := sqliteDAO1.GetCredential(ctx, "historical_user"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected primary SQL datastore to remain empty, got: %v", err)
	}

	// Case 2: Standalone nosql_only mode with configured SQL fallback
	// A user "sql_user" exists only in SQL. Attempting to create "sql_user" via RoutingDAO in nosql_only
	// must be rejected with ErrUsernameTaken.
	sqliteDAO2, _ := NewSQLiteDAO("file::memory:?cache=shared")
	defer func() { _ = sqliteDAO2.Close() }()
	_ = sqliteDAO2.Migrate(ctx)

	nosqlDAO2, _ := NewNoSQLDAO("memory://")
	defer func() { _ = nosqlDAO2.Close() }()
	_ = nosqlDAO2.Migrate(ctx)

	// Prepopulate SQL with "sql_user"
	_, err = sqliteDAO2.CreateUser(ctx, &models.UserProfile{ID: "sql-u2", Name: "Historical SQL", Phone: "333"}, &models.UserCredential{Username: "sql_user", PasswordHash: hash})
	if err != nil {
		t.Fatalf("failed prepopulating SQL: %v", err)
	}

	rNoSQLOnly, err := NewRoutingDAO(PersistenceModeNoSQLOnly, sqliteDAO2, nosqlDAO2, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	_, err = rNoSQLOnly.CreateUser(ctx, &models.UserProfile{Name: "New NoSQL", Phone: "444"}, &models.UserCredential{Username: "sql_user", PasswordHash: hash})
	if !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("expected ErrUsernameTaken when username exists in SQL fallback, got: %v", err)
	}
	// Verify primary NoSQL datastore was not written to
	if _, err := nosqlDAO2.GetCredential(ctx, "sql_user"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected primary NoSQL datastore to remain empty, got: %v", err)
	}

	// Case 3: Dual-write mode preflight
	// Username exists in secondary NoSQL -> preflight rejects immediately without writing to primary SQL.
	sqliteDAO3, _ := NewSQLiteDAO("file::memory:?cache=shared")
	defer func() { _ = sqliteDAO3.Close() }()
	_ = sqliteDAO3.Migrate(ctx)

	nosqlDAO3, _ := NewNoSQLDAO("memory://")
	defer func() { _ = nosqlDAO3.Close() }()
	_ = nosqlDAO3.Migrate(ctx)

	_, err = nosqlDAO3.CreateUser(ctx, &models.UserProfile{ID: "nosql-u3", Name: "Existing Secondary", Phone: "555"}, &models.UserCredential{Username: "dup_user", PasswordHash: hash})
	if err != nil {
		t.Fatalf("failed prepopulating NoSQL: %v", err)
	}

	rDual, err := NewRoutingDAO(PersistenceModeDualWrite, sqliteDAO3, nosqlDAO3, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	_, err = rDual.CreateUser(ctx, &models.UserProfile{Name: "Dual User", Phone: "666"}, &models.UserCredential{Username: "dup_user", PasswordHash: hash})
	if !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("expected ErrUsernameTaken in dual_write preflight, got: %v", err)
	}
	// Case 4: Alternate store outage / error -> fail closed to preserve uniqueness guarantees
	sqliteDAO4, _ := NewSQLiteDAO("file::memory:?cache=shared")
	defer func() { _ = sqliteDAO4.Close() }()
	_ = sqliteDAO4.Migrate(ctx)

	failingAlternate := &mockErrorDAO{getCredErr: errors.New("nosql datastore connection timeout")}
	rFailClosed, err := NewRoutingDAO(PersistenceModeSQLOnly, sqliteDAO4, failingAlternate, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	_, err = rFailClosed.CreateUser(ctx, &models.UserProfile{Name: "Fail Closed", Phone: "777"}, &models.UserCredential{Username: "any_user", PasswordHash: hash})
	if err == nil || !strings.Contains(err.Error(), "failed to verify username uniqueness against alternate datastore") {
		t.Fatalf("expected fail-closed error on alternate store outage, got: %v", err)
	}
	if _, err := sqliteDAO4.GetCredential(ctx, "any_user"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected primary SQL store not to be written to when alternate check fails closed, got: %v", err)
	}
}

func TestRoutingDAO_MigrationStatus_IncludesSecondaryState(t *testing.T) {
	ctx := context.Background()
	sqlDAO, _ := NewSQLiteDAO("file::memory:?cache=shared")
	defer func() { _ = sqlDAO.Close() }()
	_ = sqlDAO.Migrate(ctx)

	nosqlDAO, _ := NewNoSQLDAO("memory://")
	defer func() { _ = nosqlDAO.Close() }()
	// Keep nosqlDAO unmigrated initially
	r, err := NewRoutingDAO(PersistenceModeDualWrite, sqlDAO, nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}

	statuses, err := r.MigrationStatus(ctx)
	if err != nil {
		t.Fatalf("MigrationStatus failed: %v", err)
	}

	// Should contain SQL migrations (2) + NoSQL migration (1) = 3 total statuses
	if len(statuses) != 3 {
		t.Fatalf("expected 3 migration statuses across SQL and NoSQL, got %d: %+v", len(statuses), statuses)
	}

	// SQL migrations should be applied; NoSQL should show applied=false
	var foundNoSQL bool
	for _, s := range statuses {
		if s.Name == "000001_nosql_document_store" {
			foundNoSQL = true
			if s.Applied {
				t.Errorf("expected NoSQL migration to show applied=false before Migrate")
			}
		}
	}
	if !foundNoSQL {
		t.Error("expected NoSQL migration status to be included in multi-store MigrationStatus")
	}

	// Now migrate NoSQL and verify status updates
	_ = nosqlDAO.Migrate(ctx)
	statusesUpdated, err := r.MigrationStatus(ctx)
	if err != nil {
		t.Fatalf("MigrationStatus after migrate failed: %v", err)
	}
	for _, s := range statusesUpdated {
		if s.Name == "000001_nosql_document_store" && !s.Applied {
			t.Errorf("expected NoSQL migration to show applied=true after Migrate")
		}
	}
}

func TestRoutingDAO_SecondaryUsernameConflict_FailsCreate(t *testing.T) {
	ctx := context.Background()
	hash, _ := security.HashPassword("secretpass")

	primary, _ := NewSQLiteDAO("file::memory:?cache=shared")
	defer func() { _ = primary.Close() }()
	_ = primary.Migrate(ctx)

	// mockSecondary returns ErrUserNotFound on GetCredential (so preflight passes),
	// but returns ErrUsernameTaken on CreateUser (e.g. concurrent race condition).
	mockSecondary := &mockErrorDAO{createErr: ErrUsernameTaken}

	r, err := NewRoutingDAO(PersistenceModeDualWrite, primary, mockSecondary, nil)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	var secondaryErrorOp string
	var secondaryErrorCaptured error
	r.SetOnSecondaryError(func(op string, err error) {
		secondaryErrorOp = op
		secondaryErrorCaptured = err
	})

	prof := &models.UserProfile{Name: "Conflict User", Phone: "+1-555-0999"}
	cred := &models.UserCredential{Username: "conflict_user", PasswordHash: hash}

	// Unlike transient errors, ErrUsernameTaken from secondary must reject the create call!
	_, err = r.CreateUser(ctx, prof, cred)
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("expected ErrUsernameTaken when secondary rejects with duplicate username, got: %v", err)
	}
	if secondaryErrorOp != "CreateUser" || !errors.Is(secondaryErrorCaptured, ErrUsernameTaken) {
		t.Errorf("expected secondary error callback with ErrUsernameTaken, got op: %q, err: %v", secondaryErrorOp, secondaryErrorCaptured)
	}
}





