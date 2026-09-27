package dao

import (
	"context"
	"errors"
	"log/slog"
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
	primary, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create primary: %v", err)
	}
	secondary, err := NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create secondary: %v", err)
	}

	// Invalid mode
	if _, err := NewRoutingDAO("invalid", primary, secondary, nil); err == nil {
		t.Error("expected error for invalid mode")
	}

	// Nil primary
	if _, err := NewRoutingDAO(PersistenceModeSQLOnly, nil, secondary, nil); err == nil {
		t.Error("expected error for nil primary")
	}

	// DualWrite with nil secondary
	if _, err := NewRoutingDAO(PersistenceModeDualWrite, primary, nil, nil); err == nil {
		t.Error("expected error for dual write with nil secondary")
	}
	if _, err := NewRoutingDAO(PersistenceModeDualWriteNoSQLPrimary, primary, nil, nil); err == nil {
		t.Error("expected error for dual write nosql primary with nil secondary")
	}

	// Valid with nil logger
	r, err := NewRoutingDAO(PersistenceModeSQLOnly, primary, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error creating routing DAO: %v", err)
	}
	if r.Mode() != PersistenceModeSQLOnly {
		t.Errorf("expected mode %q, got %q", PersistenceModeSQLOnly, r.Mode())
	}
	if r.Primary() != primary {
		t.Error("expected primary instance to match")
	}
	if r.Secondary() != nil {
		t.Error("expected nil secondary")
	}

	// Mode switching
	if err := r.SetMode("invalid"); err == nil {
		t.Error("expected error setting invalid mode")
	}
	if err := r.SetMode(PersistenceModeDualWrite); err == nil {
		t.Error("expected error setting dual write without secondary")
	}
	if err := r.SetMode(PersistenceModeNoSQLOnly); err != nil {
		t.Errorf("unexpected error setting valid mode: %v", err)
	}
	if r.Mode() != PersistenceModeNoSQLOnly {
		t.Errorf("expected mode %q, got %q", PersistenceModeNoSQLOnly, r.Mode())
	}
}

// mockErrorDAO is a UserDAO stub that produces errors for testing resilience and error paths.
type mockErrorDAO struct {
	UserDAO
	createErr  error
	migrateErr error
	pingErr    error
	closeErr   error
}

func (m *mockErrorDAO) CreateUser(ctx context.Context, profile *models.UserProfile, cred *models.UserCredential) (string, error) {
	if m.createErr != nil {
		return "", m.createErr
	}
	return "mock-id", nil
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
	return 0, m.migrateErr
}

func (m *mockErrorDAO) MigrateDown(ctx context.Context, steps int) (int, error) {
	return 0, nil
}

func (m *mockErrorDAO) MigrationVersion(ctx context.Context) (int64, error) {
	return 1, nil
}

func (m *mockErrorDAO) MigrationStatus(ctx context.Context) ([]MigrationStatus, error) {
	return nil, nil
}

func TestRoutingDAO_DualWrite(t *testing.T) {
	ctx := context.Background()
	primary, _ := NewNoSQLDAO("")
	secondary, _ := NewNoSQLDAO("")

	logger := slog.Default()
	r, err := NewRoutingDAO(PersistenceModeDualWrite, primary, secondary, logger)
	if err != nil {
		t.Fatalf("failed to create routing DAO: %v", err)
	}

	hash, _ := security.HashPassword("secretpass")
	prof := &models.UserProfile{Name: "David Copperfield"}
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

	// Successful dual-write: both primary and secondary should contain the record
	id, err := r.CreateUser(ctx, prof, cred)
	if err != nil {
		t.Fatalf("failed to create user in dual-write mode: %v", err)
	}

	// Verify primary has the record
	primaryProf, err := primary.GetProfile(ctx, id)
	if err != nil || primaryProf.Name != "David Copperfield" {
		t.Fatalf("expected profile in primary: %v, err: %v", primaryProf, err)
	}

	// Verify secondary received replicated record
	secProf, err := secondary.GetProfile(ctx, id)
	if err != nil || secProf.Name != "David Copperfield" {
		t.Fatalf("expected replicated profile in secondary: %v, err: %v", secProf, err)
	}

	// Reads should delegate cleanly to primary
	readProf, err := r.GetProfile(ctx, id)
	if err != nil || readProf.Name != "David Copperfield" {
		t.Fatalf("expected GetProfile to read from primary: %v, err: %v", readProf, err)
	}

	searchRes, err := r.SearchProfiles(ctx, models.SearchQuery{Name: "Copperfield"})
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

func TestRoutingDAO_SecondaryWriteFailureResilience(t *testing.T) {
	ctx := context.Background()
	primary, _ := NewNoSQLDAO("")
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
	prof := &models.UserProfile{Name: "Resilient User"}
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

	prof := &models.UserProfile{Name: "Fail User"}
	cred := &models.UserCredential{Username: "fail_user"}

	_, err = r.CreateUser(ctx, prof, cred)
	if err == nil || !errors.Is(err, failingPrimary.createErr) {
		t.Fatalf("expected error from failing primary, got: %v", err)
	}
}

func TestRoutingDAO_DualWriteNoSQLPrimary(t *testing.T) {
	ctx := context.Background()
	nosqlPrimary, _ := NewNoSQLDAO("")
	sqlSecondary, _ := NewNoSQLDAO("")

	r, err := NewRoutingDAO(PersistenceModeDualWriteNoSQLPrimary, nosqlPrimary, sqlSecondary, nil)
	if err != nil {
		t.Fatalf("failed to create routing DAO: %v", err)
	}

	hash, _ := security.HashPassword("pw")
	prof := &models.UserProfile{Name: "NoSQL Primary User"}
	cred := &models.UserCredential{Username: "nosql_prim", PasswordHash: hash}

	id, err := r.CreateUser(ctx, prof, cred)
	if err != nil {
		t.Fatalf("unexpected error creating user: %v", err)
	}

	// Verify record in primary
	if _, err := nosqlPrimary.GetProfile(ctx, id); err != nil {
		t.Errorf("expected user in nosqlPrimary: %v", err)
	}
	// Verify record in secondary
	if _, err := sqlSecondary.GetProfile(ctx, id); err != nil {
		t.Errorf("expected user in sqlSecondary: %v", err)
	}
}

func TestRoutingDAO_SQLOnlyAndNoSQLOnly(t *testing.T) {
	ctx := context.Background()
	primary, _ := NewNoSQLDAO("")
	secondary, _ := NewNoSQLDAO("")

	// SQLOnly mode with secondary present: secondary should not be written to
	rSQL, _ := NewRoutingDAO(PersistenceModeSQLOnly, primary, secondary, nil)
	prof1 := &models.UserProfile{Name: "SQL Only"}
	cred1 := &models.UserCredential{Username: "sql_only_user"}

	id1, err := rSQL.CreateUser(ctx, prof1, cred1)
	if err != nil {
		t.Fatalf("failed creating user: %v", err)
	}
	if _, err := secondary.GetProfile(ctx, id1); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected secondary to not have record in sql_only mode")
	}

	// NoSQLOnly mode with secondary present: secondary should not be written to
	rNoSQL, _ := NewRoutingDAO(PersistenceModeNoSQLOnly, primary, secondary, nil)
	prof2 := &models.UserProfile{Name: "NoSQL Only"}
	cred2 := &models.UserCredential{Username: "nosql_only_user"}

	id2, err := rNoSQL.CreateUser(ctx, prof2, cred2)
	if err != nil {
		t.Fatalf("failed creating user: %v", err)
	}
	if _, err := secondary.GetProfile(ctx, id2); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected secondary to not have record in nosql_only mode")
	}
}

func TestRoutingDAO_MigratePingClose(t *testing.T) {
	ctx := context.Background()
	primary, _ := NewNoSQLDAO("")
	secondary, _ := NewNoSQLDAO("")

	r, err := NewRoutingDAO(PersistenceModeDualWrite, primary, secondary, nil)
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
	if err != nil || count != 1 {
		t.Fatalf("expected MigrateUp count 1, got %d, err: %v", count, err)
	}

	downCount, err := r.MigrateDown(ctx, 1)
	if err != nil || downCount != 0 {
		t.Fatalf("expected MigrateDown 0, got %d, err: %v", downCount, err)
	}

	ver, err := r.MigrationVersion(ctx)
	if err != nil || ver != 1 {
		t.Fatalf("expected MigrationVersion 1, got %d, err: %v", ver, err)
	}

	statuses, err := r.MigrationStatus(ctx)
	if err != nil || len(statuses) != 1 {
		t.Fatalf("expected 1 migration status, got %v, err: %v", statuses, err)
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
