package dao

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/borch-ai/lid-challenge/internal/models"
)

type unsupportedDialect struct{}

func (unsupportedDialect) Name() string           { return "oracle" }
func (unsupportedDialect) Rebind(q string) string { return q }
func (unsupportedDialect) SchemaDDL() string      { return "" }

func TestMigrator_Up_Down_Lifecycle_SQLite(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	migrator, err := NewMigrator(db, SQLiteDialect{})
	if err != nil {
		t.Fatalf("failed to create sqlite migrator: %v", err)
	}

	// 1. Initial State: Version 0, all migrations pending
	ver, err := migrator.Version(ctx)
	if err != nil {
		t.Fatalf("failed to get initial version: %v", err)
	}
	if ver != 0 {
		t.Fatalf("expected initial version 0, got %d", ver)
	}

	statuses, err := migrator.Status(ctx)
	if err != nil {
		t.Fatalf("failed to get initial status: %v", err)
	}
	if len(statuses) < 2 {
		t.Fatalf("expected at least 2 migrations, got %d", len(statuses))
	}
	for _, s := range statuses {
		if s.Applied {
			t.Errorf("expected migration %d to be pending initially", s.Version)
		}
	}

	// 2. Up: Apply all migrations
	applied, err := migrator.Up(ctx)
	if err != nil {
		t.Fatalf("failed to apply migrations up: %v", err)
	}
	if applied != len(statuses) {
		t.Fatalf("expected %d migrations applied, got %d", len(statuses), applied)
	}

	ver, err = migrator.Version(ctx)
	if err != nil {
		t.Fatalf("failed to get version after up: %v", err)
	}
	if ver != statuses[len(statuses)-1].Version {
		t.Fatalf("expected version %d, got %d", statuses[len(statuses)-1].Version, ver)
	}

	// 3. Up again: Idempotent (0 applied)
	appliedAgain, err := migrator.Up(ctx)
	if err != nil {
		t.Fatalf("failed to run up again: %v", err)
	}
	if appliedAgain != 0 {
		t.Errorf("expected 0 migrations applied on re-run, got %d", appliedAgain)
	}

	// 4. Down (1 step): Roll back top migration
	rolledBack, err := migrator.Down(ctx, 1)
	if err != nil {
		t.Fatalf("failed to roll back 1 migration: %v", err)
	}
	if rolledBack != 1 {
		t.Fatalf("expected 1 migration rolled back, got %d", rolledBack)
	}

	ver, err = migrator.Version(ctx)
	if err != nil {
		t.Fatalf("failed to get version after rollback: %v", err)
	}
	if ver != statuses[len(statuses)-2].Version {
		t.Fatalf("expected version %d after rollback, got %d", statuses[len(statuses)-2].Version, ver)
	}

	// Check status reflects rollback
	statuses, err = migrator.Status(ctx)
	if err != nil {
		t.Fatalf("failed to get status after rollback: %v", err)
	}
	if !statuses[0].Applied {
		t.Errorf("expected first migration to still be applied")
	}
	if statuses[len(statuses)-1].Applied {
		t.Errorf("expected last migration to be rolled back/pending")
	}

	// 5. Down remaining: Roll back to 0
	rolledBackAll, err := migrator.Down(ctx, 100)
	if err != nil {
		t.Fatalf("failed to roll back all migrations: %v", err)
	}
	if rolledBackAll != len(statuses)-1 {
		t.Fatalf("expected %d rolled back, got %d", len(statuses)-1, rolledBackAll)
	}

	ver, err = migrator.Version(ctx)
	if err != nil {
		t.Fatalf("failed to get version after full rollback: %v", err)
	}
	if ver != 0 {
		t.Fatalf("expected version 0 after full rollback, got %d", ver)
	}

	// Down when already at 0
	rolledBackNone, err := migrator.Down(ctx, 1)
	if err != nil {
		t.Fatalf("unexpected error rolling back at version 0: %v", err)
	}
	if rolledBackNone != 0 {
		t.Errorf("expected 0 rolled back at version 0, got %d", rolledBackNone)
	}
}

func TestMigrator_Postgres_EmbeddedLoading(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}
	defer func() { _ = db.Close() }()

	migrator, err := NewMigrator(db, PostgresDialect{})
	if err != nil {
		t.Fatalf("failed to load postgres embedded migrations: %v", err)
	}

	migrations := migrator.Migrations()
	if len(migrations) < 2 {
		t.Fatalf("expected at least 2 postgres migrations, got %d", len(migrations))
	}
	if migrations[0].Version != 1 || migrations[0].UpSQL == "" || migrations[0].DownSQL == "" {
		t.Errorf("malformed migration 1: %+v", migrations[0])
	}
	if migrations[1].Version != 2 || migrations[1].UpSQL == "" || migrations[1].DownSQL == "" {
		t.Errorf("malformed migration 2: %+v", migrations[1])
	}
}

func TestMigrator_ErrorCases_Validation(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}
	defer func() { _ = db.Close() }()

	// 1. Nil DB
	if _, err := NewMigratorWithFS(nil, SQLiteDialect{}, sqliteMigrationsFS, "migrations/sqlite"); err == nil {
		t.Errorf("expected error with nil db, got nil")
	}

	// 2. Nil Dialect
	if _, err := NewMigrator(db, nil); err == nil {
		t.Errorf("expected error with nil dialect in NewMigrator, got nil")
	}
	if _, err := NewMigratorWithFS(db, nil, sqliteMigrationsFS, "migrations/sqlite"); err == nil {
		t.Errorf("expected error with nil dialect in NewMigratorWithFS, got nil")
	}

	// 3. Unsupported Dialect
	if _, err := NewMigrator(db, unsupportedDialect{}); err == nil {
		t.Errorf("expected error with unsupported dialect, got nil")
	}

	// 4. Missing directory
	mockFS := fstest.MapFS{}
	if _, err := NewMigratorWithFS(db, SQLiteDialect{}, mockFS, "non_existent"); err == nil {
		t.Errorf("expected error for non-existent directory, got nil")
	}

	// 5. Non-.sql file is safely ignored
	ignoredFS := fstest.MapFS{
		"migrations/README.txt":           &fstest.MapFile{Data: []byte("Documentation")},
		"migrations/000001_init.up.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE foo(id INT);")},
		"migrations/000001_init.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE foo;")},
	}
	migs, err := LoadMigrations(ignoredFS, "migrations")
	if err != nil || len(migs) != 1 {
		t.Errorf("expected non-.sql file to be ignored and 1 migration loaded, got %d (err: %v)", len(migs), err)
	}

	// 6. Non-conforming .sql filename (not ending in .up.sql or .down.sql)
	invalidFormatFS := fstest.MapFS{
		"migrations/something.sql": &fstest.MapFile{Data: []byte("CREATE TABLE foo(id INT);")},
	}
	if _, err := LoadMigrations(invalidFormatFS, "migrations"); err == nil {
		t.Errorf("expected error for invalid filename structure, got nil")
	}

	// 7. Non-numeric version
	nonNumericFS := fstest.MapFS{
		"migrations/abc_test.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE foo(id INT);")},
	}
	if _, err := LoadMigrations(nonNumericFS, "migrations"); err == nil {
		t.Errorf("expected error for non-numeric version, got nil")
	}

	// 8. Missing Up SQL
	missingUpFS := fstest.MapFS{
		"migrations/000001_test.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE foo;")},
	}
	if _, err := LoadMigrations(missingUpFS, "migrations"); err == nil {
		t.Errorf("expected error for missing .up.sql, got nil")
	}

	// 9. Conflicting names for same version
	conflictNameFS := fstest.MapFS{
		"migrations/000001_alpha.up.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE a(id INT);")},
		"migrations/000001_beta.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE a;")},
	}
	if _, err := LoadMigrations(conflictNameFS, "migrations"); err == nil {
		t.Errorf("expected error for conflicting migration names, got nil")
	}

	// 10. Zero/negative migration version
	zeroVersionFS := fstest.MapFS{
		"migrations/000000_zero.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE zero(id INT);")},
	}
	if _, err := LoadMigrations(zeroVersionFS, "migrations"); err == nil {
		t.Errorf("expected error for zero migration version, got nil")
	} else if !strings.Contains(err.Error(), "version must be positive") {
		t.Errorf("expected error to contain 'version must be positive', got %v", err)
	}

	// 11. Duplicate up migration file for same version
	duplicateUpFS := fstest.MapFS{
		"migrations/000001_first.up.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE a(id INT);")},
		"migrations/000001_first.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE a;")},
		"migrations/1_first.up.sql":        &fstest.MapFile{Data: []byte("CREATE TABLE b(id INT);")},
	}
	if _, err := LoadMigrations(duplicateUpFS, "migrations"); err == nil {
		t.Errorf("expected error for duplicate up migration, got nil")
	} else if !strings.Contains(err.Error(), "duplicate up migration") {
		t.Errorf("expected error to contain 'duplicate up migration', got %v", err)
	}

	// 12. Duplicate down migration file for same version
	duplicateDownFS := fstest.MapFS{
		"migrations/000001_first.up.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE a(id INT);")},
		"migrations/000001_first.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE a;")},
		"migrations/1_first.down.sql":      &fstest.MapFile{Data: []byte("DROP TABLE b;")},
	}
	if _, err := LoadMigrations(duplicateDownFS, "migrations"); err == nil {
		t.Errorf("expected error for duplicate down migration, got nil")
	} else if !strings.Contains(err.Error(), "duplicate down migration") {
		t.Errorf("expected error to contain 'duplicate down migration', got %v", err)
	}

	// 13. Subdirectory inside migrations directory is skipped
	subdirFS := fstest.MapFS{
		"migrations/subdir":               &fstest.MapFile{Mode: os.ModeDir},
		"migrations/000001_init.up.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE foo(id INT);")},
		"migrations/000001_init.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE foo;")},
	}
	migsWithSubdir, err := LoadMigrations(subdirFS, "migrations")
	if err != nil || len(migsWithSubdir) != 1 {
		t.Errorf("expected subdir to be skipped and 1 migration loaded, got %d (err: %v)", len(migsWithSubdir), err)
	}

	// 14. Malformed filenames with missing version or name parts
	missingPrefixFS := fstest.MapFS{
		"migrations/_init.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	}
	if _, err := LoadMigrations(missingPrefixFS, "migrations"); err == nil {
		t.Errorf("expected error for missing version prefix, got nil")
	}

	missingSuffixFS := fstest.MapFS{
		"migrations/000001_.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	}
	if _, err := LoadMigrations(missingSuffixFS, "migrations"); err == nil {
		t.Errorf("expected error for missing name suffix, got nil")
	}

	// Test parseMigrationTime error cases
	if _, err := parseMigrationTime("not-a-timestamp"); err == nil {
		t.Errorf("expected error parsing invalid time string")
	}
	if _, err := parseMigrationTime([]byte("not-a-timestamp")); err == nil {
		t.Errorf("expected error parsing invalid time byte slice")
	}
	if _, err := parseMigrationTime(12345); err == nil {
		t.Errorf("expected error parsing unsupported type")
	}
}

func TestMigrator_Execution_Failures(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// 1. Faulty Up SQL: Transaction should roll back cleanly
	faultyUpFS := fstest.MapFS{
		"migrations/000001_broken.up.sql":   &fstest.MapFile{Data: []byte("SYNTAX ERROR IN SQL")},
		"migrations/000001_broken.down.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	}
	m, err := NewMigratorWithFS(db, SQLiteDialect{}, faultyUpFS, "migrations")
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}
	if _, err := m.Up(ctx); err == nil {
		t.Errorf("expected error executing faulty up SQL, got nil")
	}

	// 2. Migration without Down SQL attempting rollback
	noDownFS := fstest.MapFS{
		"migrations/000001_nodown.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE t1(id INT);")},
	}
	m2, err := NewMigratorWithFS(db, SQLiteDialect{}, noDownFS, "migrations")
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}
	if _, err := m2.Up(ctx); err != nil {
		t.Fatalf("unexpected error running up: %v", err)
	}
	if _, err := m2.Down(ctx, 1); err == nil {
		t.Errorf("expected error rolling back migration with empty down SQL, got nil")
	}

	// 3. Faulty Down SQL
	_, _ = db.ExecContext(ctx, "DELETE FROM schema_migrations; DROP TABLE IF EXISTS t1;")
	faultyDownFS := fstest.MapFS{
		"migrations/000001_faultydown.up.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE t2(id INT);")},
		"migrations/000001_faultydown.down.sql": &fstest.MapFile{Data: []byte("FAULTY SQL STATEMENT")},
	}
	m3, err := NewMigratorWithFS(db, SQLiteDialect{}, faultyDownFS, "migrations")
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}
	if _, err := m3.Up(ctx); err != nil {
		t.Fatalf("unexpected error running up: %v", err)
	}
	if _, err := m3.Down(ctx, 1); err == nil {
		t.Errorf("expected error rolling back faulty down SQL, got nil")
	}
}

func TestSQLDAO_MigratableDAO_Methods(t *testing.T) {
	dao, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite dao: %v", err)
	}
	defer func() { _ = dao.Close() }()

	ctx := context.Background()

	// Verify Migrator() getter
	if dao.Migrator() == nil {
		t.Fatalf("expected non-nil Migrator from SQLDAO")
	}

	// Test MigrateUp
	count, err := dao.MigrateUp(ctx)
	if err != nil {
		t.Fatalf("failed to MigrateUp: %v", err)
	}
	if count < 2 {
		t.Errorf("expected at least 2 migrations applied, got %d", count)
	}

	// Test MigrationVersion
	ver, err := dao.MigrationVersion(ctx)
	if err != nil {
		t.Fatalf("failed to get MigrationVersion: %v", err)
	}
	if ver < 2 {
		t.Errorf("expected version >= 2, got %d", ver)
	}

	// Test MigrationStatus
	statuses, err := dao.MigrationStatus(ctx)
	if err != nil {
		t.Fatalf("failed to get MigrationStatus: %v", err)
	}
	if len(statuses) < 2 {
		t.Errorf("expected at least 2 statuses, got %d", len(statuses))
	}

	// Verify user creation works with migrated schema
	profile := &models.UserProfile{Name: "Migrated User", Phone: "555-1234"}
	cred := &models.UserCredential{Username: "migrated_user", PasswordHash: "hash"}
	if _, err := dao.CreateUser(ctx, profile, cred); err != nil {
		t.Fatalf("failed to create user after migration: %v", err)
	}

	// Test MigrateDown
	rolledBack, err := dao.MigrateDown(ctx, 1)
	if err != nil {
		t.Fatalf("failed to MigrateDown: %v", err)
	}
	if rolledBack != 1 {
		t.Errorf("expected 1 migration rolled back, got %d", rolledBack)
	}

	// Test nil migrator fallback branches
	nilMigratorDAO := &SQLDAO{
		db:      dao.db,
		dialect: dao.dialect,
	}
	if _, err := nilMigratorDAO.MigrateUp(ctx); err == nil {
		t.Errorf("expected error for MigrateUp with nil migrator")
	}
	if _, err := nilMigratorDAO.MigrateDown(ctx, 1); err == nil {
		t.Errorf("expected error for MigrateDown with nil migrator")
	}
	if _, err := nilMigratorDAO.MigrationVersion(ctx); err == nil {
		t.Errorf("expected error for MigrationVersion with nil migrator")
	}
	if _, err := nilMigratorDAO.MigrationStatus(ctx); err == nil {
		t.Errorf("expected error for MigrationStatus with nil migrator")
	}
	// Migrate fallback with nil migrator executes SchemaDDL directly
	if err := nilMigratorDAO.Migrate(ctx); err != nil {
		t.Fatalf("expected fallback Migrate to succeed, got %v", err)
	}

	// Migrate fallback with closed DB triggers error
	closedDB, _ := sql.Open("sqlite", "file::memory:?cache=shared")
	_ = closedDB.Close()
	closedMigratorDAO := &SQLDAO{
		db:      closedDB,
		dialect: dao.dialect,
	}
	if err := closedMigratorDAO.Migrate(ctx); err == nil {
		t.Errorf("expected error from fallback Migrate on closed DB, got nil")
	}
}

func TestMigrator_Additional_Coverage(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// 1. EnsureTable with PostgresDialect on SQLite (valid syntax in SQLite)
	pgMigrator := &Migrator{
		db:      db,
		dialect: PostgresDialect{},
	}
	if err := pgMigrator.EnsureTable(ctx); err != nil {
		t.Fatalf("failed EnsureTable with PostgresDialect: %v", err)
	}

	// 2. Down with steps <= 0 defaults to 1
	m, err := NewMigrator(db, SQLiteDialect{})
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}
	if _, err := m.Up(ctx); err != nil {
		t.Fatalf("failed to run Up: %v", err)
	}
	rolledBack, err := m.Down(ctx, 0)
	if err != nil {
		t.Fatalf("failed Down with steps=0: %v", err)
	}
	if rolledBack != 1 {
		t.Errorf("expected 1 migration rolled back with steps=0, got %d", rolledBack)
	}

	// 3. Error branches when DB is closed
	closedDB, _ := sql.Open("sqlite", "file::memory:?cache=shared")
	_ = closedDB.Close()

	closedMigrator, _ := NewMigrator(closedDB, SQLiteDialect{})
	if _, err := closedMigrator.Up(ctx); err == nil {
		t.Errorf("expected Up error on closed DB, got nil")
	}
	if _, err := closedMigrator.Down(ctx, 1); err == nil {
		t.Errorf("expected Down error on closed DB, got nil")
	}
	if _, err := closedMigrator.Version(ctx); err == nil {
		t.Errorf("expected Version error on closed DB, got nil")
	}
	if _, err := closedMigrator.Status(ctx); err == nil {
		t.Errorf("expected Status error on closed DB, got nil")
	}
}

func TestParseMigrationTime(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	// 1. time.Time
	t1, err := parseMigrationTime(now)
	if err != nil || !t1.Equal(now) {
		t.Errorf("expected time.Time identity, got %v (err: %v)", t1, err)
	}

	// 2. RFC3339 string
	str := now.Format(time.RFC3339)
	t2, err := parseMigrationTime(str)
	if err != nil || !t2.Equal(now) {
		t.Errorf("expected parsed string to equal now, got %v (err: %v)", t2, err)
	}

	// 3. Byte slice
	b := []byte(str)
	t3, err := parseMigrationTime(b)
	if err != nil || !t3.Equal(now) {
		t.Errorf("expected parsed bytes to equal now, got %v (err: %v)", t3, err)
	}

	// 4. Invalid time string
	if _, err := parseMigrationTime("not-a-timestamp"); err == nil {
		t.Errorf("expected error parsing invalid time string, got nil")
	}

	// 5. Unsupported type
	if _, err := parseMigrationTime(12345); err == nil {
		t.Errorf("expected error for unsupported integer type, got nil")
	}
}

func TestMigrator_ConcurrentUp_Coordination(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent_test.db")
	baseDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open shared base db: %v", err)
	}
	defer func() { _ = baseDB.Close() }()

	const numReplicas = 4
	errChan := make(chan error, numReplicas)

	for i := 0; i < numReplicas; i++ {
		go func() {
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				errChan <- err
				return
			}
			defer func() { _ = db.Close() }()

			migrator, err := NewMigrator(db, SQLiteDialect{})
			if err != nil {
				errChan <- err
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			_, err = migrator.Up(ctx)
			errChan <- err
		}()
	}

	for i := 0; i < numReplicas; i++ {
		if err := <-errChan; err != nil {
			t.Fatalf("concurrent migration replica failed: %v", err)
		}
	}

	m, err := NewMigrator(baseDB, SQLiteDialect{})
	if err != nil {
		t.Fatalf("failed to create verifier migrator: %v", err)
	}
	ver, err := m.Version(context.Background())
	if err != nil {
		t.Fatalf("failed to verify final version: %v", err)
	}
	if ver < 2 {
		t.Errorf("expected final version >= 2, got %d", ver)
	}
}

func TestMigrator_Lock_And_BreakStaleLock(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	m, err := NewMigrator(db, SQLiteDialect{})
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}

	if err := m.EnsureTable(ctx); err != nil {
		t.Fatalf("failed to ensure tables: %v", err)
	}

	// 1. Manually set stale lock (locked 10 minutes ago)
	staleTime := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
	_, err = db.ExecContext(ctx, "UPDATE schema_migrations_lock SET is_locked = 1, locked_at = ?, locked_by = 'crashed_pod' WHERE id = 1", staleTime)
	if err != nil {
		t.Fatalf("failed to insert stale lock: %v", err)
	}

	// breakStaleLock should clear it
	m.breakStaleLock(ctx, time.Now().UTC())

	var isLocked bool
	err = db.QueryRowContext(ctx, "SELECT is_locked FROM schema_migrations_lock WHERE id = 1").Scan(&isLocked)
	if err != nil {
		t.Fatalf("failed to query lock state: %v", err)
	}
	if isLocked {
		t.Errorf("expected stale lock to be broken (is_locked = false), got true")
	}

	// Also test breakStaleLock with PostgresDialect
	pgM := &Migrator{db: db, dialect: PostgresDialect{}}
	pgM.breakStaleLock(ctx, time.Now().UTC())

	// 2. acquireLock with already-canceled context
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// Lock the table so acquireLock cannot immediately grab it
	_, _ = db.ExecContext(ctx, "UPDATE schema_migrations_lock SET is_locked = 1, locked_at = ?, locked_by = 'holder' WHERE id = 1", time.Now().UTC().Format(time.RFC3339Nano))

	if _, _, err := m.acquireLock(canceledCtx); err == nil {
		t.Errorf("expected acquireLock to fail with canceled context, got nil")
	}

	// 3. Test ownership fencing on releaseLock
	// Wrong owner cannot release lock
	if err := m.releaseLock(ctx, "wrong-owner"); err != nil {
		t.Fatalf("releaseLock unexpected error: %v", err)
	}
	var isStillLocked bool
	var lockedBy string
	_ = db.QueryRowContext(ctx, "SELECT is_locked, locked_by FROM schema_migrations_lock WHERE id = 1").Scan(&isStillLocked, &lockedBy)
	if !isStillLocked || lockedBy != "holder" {
		t.Errorf("expected lock to remain held by 'holder', got is_locked=%v, locked_by=%s", isStillLocked, lockedBy)
	}

	// Right owner releases lock successfully
	if err := m.releaseLock(ctx, "holder"); err != nil {
		t.Fatalf("releaseLock error: %v", err)
	}
	_ = db.QueryRowContext(ctx, "SELECT is_locked FROM schema_migrations_lock WHERE id = 1").Scan(&isStillLocked)
	if isStillLocked {
		t.Errorf("expected lock to be released (is_locked=false), got true")
	}

	// 4. Test acquireLock and unlock function lifecycle
	_, unlock, err := m.acquireLock(ctx)
	if err != nil {
		t.Fatalf("failed to acquire lock: %v", err)
	}
	_ = db.QueryRowContext(ctx, "SELECT is_locked FROM schema_migrations_lock WHERE id = 1").Scan(&isStillLocked)
	if !isStillLocked {
		t.Errorf("expected lock to be acquired, got false")
	}

	// renewLock test
	if err := m.renewLock(ctx, "nonexistent"); !errors.Is(err, errLockLost) {
		t.Errorf("expected renewLock with nonexistent owner to return errLockLost, got: %v", err)
	}
	if err := pgM.renewLock(ctx, "nonexistent"); !errors.Is(err, errLockLost) {
		t.Errorf("expected pgM.renewLock with nonexistent owner to return errLockLost, got: %v", err)
	}

	// Unlock successfully
	unlock()
	_ = db.QueryRowContext(ctx, "SELECT is_locked FROM schema_migrations_lock WHERE id = 1").Scan(&isStillLocked)
	if isStillLocked {
		t.Errorf("expected unlock() to release lock, got true")
	}
}

func TestMigrator_UnknownAndConflictingAppliedMigrations(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	m, err := NewMigrator(db, SQLiteDialect{})
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}
	if err := m.EnsureTable(ctx); err != nil {
		t.Fatalf("failed to ensure table: %v", err)
	}

	// 1. Insert an unknown future migration version (e.g. version 99)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (99, 'future_migration', ?)", now)
	if err != nil {
		t.Fatalf("failed to insert unknown migration: %v", err)
	}

	// Up must reject the unknown applied version
	if _, err := m.Up(ctx); err == nil {
		t.Errorf("expected Up to fail due to unknown applied version, got nil")
	} else if !strings.Contains(err.Error(), "unknown migration version 99") {
		t.Errorf("expected error to mention unknown migration version 99, got: %v", err)
	}

	// Down must also reject the unknown applied version
	if _, err := m.Down(ctx, 1); err == nil {
		t.Errorf("expected Down to fail due to unknown applied version, got nil")
	} else if !strings.Contains(err.Error(), "unknown migration version 99") {
		t.Errorf("expected error to mention unknown migration version 99, got: %v", err)
	}

	// Status should include the unknown migration marked as (unknown)
	statuses, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	foundUnknown := false
	for _, st := range statuses {
		if st.Version == 99 && strings.Contains(st.Name, "(unknown)") && st.Applied {
			foundUnknown = true
			break
		}
	}
	if !foundUnknown {
		t.Errorf("expected Status to include unknown migration version 99, got: %+v", statuses)
	}

	// 2. Clear unknown migration and test conflicting name for known version (version 1)
	_, _ = db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = 99")
	_, err = db.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (1, 'wrong_name_for_v1', ?)", now)
	if err != nil {
		t.Fatalf("failed to insert conflicting migration name: %v", err)
	}

	if _, err := m.Up(ctx); err == nil {
		t.Errorf("expected Up to fail due to conflicting migration name, got nil")
	} else if !strings.Contains(err.Error(), "conflicting name") {
		t.Errorf("expected error to mention conflicting name, got: %v", err)
	}

	if _, err := m.Down(ctx, 1); err == nil {
		t.Errorf("expected Down to fail due to conflicting migration name, got nil")
	} else if !strings.Contains(err.Error(), "conflicting name") {
		t.Errorf("expected error to mention conflicting name, got: %v", err)
	}
}

func TestMigrator_SQLite_ProcessLiveness_Lock(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	m, err := NewMigrator(db, SQLiteDialect{})
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}
	if err := m.EnsureTable(ctx); err != nil {
		t.Fatalf("failed to ensure table: %v", err)
	}

	// Test isProcessAlive helper
	if !isProcessAlive(os.Getpid()) {
		t.Errorf("expected current process to be alive")
	}
	if isProcessAlive(0) {
		t.Errorf("expected PID 0 not to be considered alive")
	}
	if isProcessAlive(-1) {
		t.Errorf("expected negative PID not to be alive")
	}
	if isProcessAlive(9999999) {
		t.Errorf("expected non-existent PID 9999999 not to be alive")
	}

	// 1. Lock held by current PID on current host (active transaction simulation)
	hostname, _ := os.Hostname()
	aliveOwner := fmt.Sprintf("%s:%d:token1", hostname, os.Getpid())
	staleTime := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
	_, _ = db.ExecContext(ctx, "UPDATE schema_migrations_lock SET is_locked = 1, locked_at = ?, locked_by = ? WHERE id = 1", staleTime, aliveOwner)

	// breakStaleLock must NOT reclaim the lock because the PID is alive on this host
	m.breakStaleLock(ctx, time.Now().UTC())

	var isLocked bool
	_ = db.QueryRowContext(ctx, "SELECT is_locked FROM schema_migrations_lock WHERE id = 1").Scan(&isLocked)
	if !isLocked {
		t.Errorf("expected lock to remain held for alive local process, but it was reclaimed")
	}

	// 2. Lock held by dead PID on current host (crashed process simulation)
	deadOwner := fmt.Sprintf("%s:9999999:token2", hostname)
	_, _ = db.ExecContext(ctx, "UPDATE schema_migrations_lock SET is_locked = 1, locked_at = ?, locked_by = ? WHERE id = 1", staleTime, deadOwner)

	// breakStaleLock MUST reclaim the lock because the PID is dead
	m.breakStaleLock(ctx, time.Now().UTC())

	_ = db.QueryRowContext(ctx, "SELECT is_locked FROM schema_migrations_lock WHERE id = 1").Scan(&isLocked)
	if isLocked {
		t.Errorf("expected lock to be broken for dead local process, but it remained locked")
	}
}

func TestMigrator_Validation_Branches(t *testing.T) {
	if _, err := NewMigrator(nil, SQLiteDialect{}); err == nil {
		t.Errorf("expected error for nil db")
	}
	db, _ := sql.Open("sqlite", "file::memory:?cache=shared")
	defer func() { _ = db.Close() }()
	if _, err := NewMigrator(db, nil); err == nil {
		t.Errorf("expected error for nil dialect")
	}
	if _, err := NewMigrator(db, unsupportedDialect{}); err == nil {
		t.Errorf("expected error for unsupported dialect")
	}

	// Test EnsureTable, Version, Status, getAppliedMigrations with closed db
	closedDB, _ := sql.Open("sqlite", "file::memory:?cache=shared")
	_ = closedDB.Close()
	mClosed := &Migrator{db: closedDB, dialect: SQLiteDialect{}}
	if err := mClosed.EnsureTable(context.Background()); err == nil {
		t.Errorf("expected EnsureTable to fail on closed db")
	}
	if _, err := mClosed.Version(context.Background()); err == nil {
		t.Errorf("expected Version to fail on closed db")
	}
	if _, err := mClosed.Status(context.Background()); err == nil {
		t.Errorf("expected Status to fail on closed db")
	}
	if _, err := mClosed.getAppliedMigrations(context.Background()); err == nil {
		t.Errorf("expected getAppliedMigrations to fail on closed db")
	}
}

func TestMigrator_LockLost_FencesMigration(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open sqlite database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	m, err := NewMigrator(db, SQLiteDialect{})
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}
	if err := m.EnsureTable(ctx); err != nil {
		t.Fatalf("failed to ensure table: %v", err)
	}

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	// applyMigration and rollbackMigration with canceled context
	if err := m.applyMigration(canceledCtx, m.migrations[0]); err == nil {
		t.Errorf("expected applyMigration to fail with canceled context")
	}
	if err := m.rollbackMigration(canceledCtx, m.migrations[0]); err == nil {
		t.Errorf("expected rollbackMigration to fail with canceled context")
	}

	// acquireLock with PostgresDialect renewal
	pgM := &Migrator{db: db, dialect: PostgresDialect{}, migrations: m.migrations}
	migrationCtx, unlock, err := pgM.acquireLock(ctx)
	if err != nil {
		t.Fatalf("failed to acquire pg lock: %v", err)
	}
	defer unlock()

	// Steal lock by overwriting locked_by in DB
	_, _ = db.ExecContext(ctx, "UPDATE schema_migrations_lock SET locked_by = 'thief' WHERE id = 1")
	// renewLock must fail with errLockLost
	if err := pgM.renewLock(ctx, "someone"); !errors.Is(err, errLockLost) {
		t.Errorf("expected errLockLost, got: %v", err)
	}
	_ = migrationCtx
}
