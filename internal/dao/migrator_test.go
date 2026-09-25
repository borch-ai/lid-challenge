package dao

import (
	"context"
	"database/sql"
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
