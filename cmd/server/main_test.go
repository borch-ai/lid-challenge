package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/borch-ai/lid-challenge/internal/dao"
)

func captureStdout(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	f()

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestRunMigrationCLI(t *testing.T) {
	testDAO, err := dao.NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite dao: %v", err)
	}
	defer func() { _ = testDAO.Close() }()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 1. Status initially
	out := captureStdout(func() {
		runMigrationCLI(testDAO, []string{"status"}, logger)
	})
	if !strings.Contains(out, "VERSION") || !strings.Contains(out, "PENDING") {
		t.Errorf("expected status output with pending migrations, got: %s", out)
	}

	// 2. Up
	out = captureStdout(func() {
		runMigrationCLI(testDAO, []string{"up"}, logger)
	})
	if !strings.Contains(out, "Successfully applied") {
		t.Errorf("expected migration up success, got: %s", out)
	}

	// 3. Version
	out = captureStdout(func() {
		runMigrationCLI(testDAO, []string{"version"}, logger)
	})
	if !strings.Contains(out, "Current schema version:") {
		t.Errorf("expected version output, got: %s", out)
	}

	// 4. Status after Up
	out = captureStdout(func() {
		runMigrationCLI(testDAO, []string{"status"}, logger)
	})
	if !strings.Contains(out, "APPLIED") {
		t.Errorf("expected applied status, got: %s", out)
	}

	// 5. Down 1 step
	out = captureStdout(func() {
		runMigrationCLI(testDAO, []string{"down", "1"}, logger)
	})
	if !strings.Contains(out, "Successfully rolled back 1 migration") {
		t.Errorf("expected rollback output, got: %s", out)
	}

	// 6. Help
	out = captureStdout(func() {
		runMigrationCLI(testDAO, []string{"help"}, logger)
	})
	if !strings.Contains(out, "Usage: lid-server migrate") {
		t.Errorf("expected usage output, got: %s", out)
	}
}

func TestInitUserDAO(t *testing.T) {
	// 1. SQLite
	d1, err := initUserDAO("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("expected sqlite init to succeed, got %v", err)
	}
	_ = d1.Close()

	// 2. NoSQL drivers
	for _, drv := range []string{"nosql", "document", "memory"} {
		d, err := initUserDAO(drv, "")
		if err != nil {
			t.Fatalf("expected %s init to succeed, got %v", drv, err)
		}
		_ = d.Close()
	}

	// 3. Postgres driver routes to NewPostgresDAO
	if _, err := initUserDAO("postgres", ""); err == nil {
		t.Errorf("expected error for postgres with empty DSN, got nil")
	}

	// 4. Unsupported drivers
	for _, drv := range []string{"unsupported_driver", "mongodb"} {
		if _, err := initUserDAO(drv, ""); err == nil {
			t.Errorf("expected error for unsupported driver %q, got nil", drv)
		}
	}
}

func TestBuildRoutingDAO(t *testing.T) {
	sqliteDAO, _ := dao.NewSQLiteDAO("file::memory:?cache=shared")
	nosqlDAO, _ := dao.NewNoSQLDAO("")

	// driver1 SQL, driver2 NoSQL
	r1, err := buildRoutingDAO(dao.PersistenceModeDualWrite, "sqlite", sqliteDAO, "nosql", nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed buildRoutingDAO: %v", err)
	}
	if r1.SQLDAO() != sqliteDAO || r1.NoSQLDAO() != nosqlDAO {
		t.Errorf("expected sqlDAO and nosqlDAO mapped correctly")
	}

	// driver1 NoSQL, driver2 SQL for dual_write_nosql_primary
	r2, err := buildRoutingDAO(dao.PersistenceModeDualWriteNoSQLPrimary, "nosql", nosqlDAO, "sqlite", sqliteDAO, nil)
	if err != nil {
		t.Fatalf("failed buildRoutingDAO with dual_write_nosql_primary: %v", err)
	}
	if r2.SQLDAO() != sqliteDAO || r2.NoSQLDAO() != nosqlDAO {
		t.Errorf("expected dual_write_nosql_primary to map sqlDAO and nosqlDAO correctly")
	}

	// Reject inverted driver orientation: dual_write with NoSQL primary
	if _, err := buildRoutingDAO(dao.PersistenceModeDualWrite, "nosql", nosqlDAO, "sqlite", sqliteDAO, nil); err == nil {
		t.Errorf("expected error when dual_write has NoSQL primary driver, got nil")
	}

	// Reject inverted driver orientation: dual_write_nosql_primary with SQL primary
	if _, err := buildRoutingDAO(dao.PersistenceModeDualWriteNoSQLPrimary, "sqlite", sqliteDAO, "nosql", nosqlDAO, nil); err == nil {
		t.Errorf("expected error when dual_write_nosql_primary has SQL primary driver, got nil")
	}

	// Reject same-kind SQL driver pairs
	if _, err := buildRoutingDAO(dao.PersistenceModeDualWrite, "sqlite", sqliteDAO, "postgres", sqliteDAO, nil); err == nil {
		t.Errorf("expected error when both drivers are SQL, got nil")
	}

	// Reject same-kind NoSQL driver pairs
	if _, err := buildRoutingDAO(dao.PersistenceModeDualWrite, "nosql", nosqlDAO, "memory", nosqlDAO, nil); err == nil {
		t.Errorf("expected error when both drivers are NoSQL, got nil")
	}

	// Standalone mode: nosql_only with secondary nosql driver (primary must be dao1)
	memDAO, _ := dao.NewNoSQLDAO("memory")
	rNoSQLStandalone, err := buildRoutingDAO(dao.PersistenceModeNoSQLOnly, "nosql", nosqlDAO, "memory", memDAO, nil)
	if err != nil {
		t.Fatalf("failed buildRoutingDAO for nosql_only with secondary: %v", err)
	}
	if rNoSQLStandalone.NoSQLDAO() != nosqlDAO {
		t.Errorf("expected primary nosqlDAO to be retained in nosql_only mode, got secondary")
	}

	// Standalone mode: sql_only with secondary nosql driver
	rSQLStandalone, err := buildRoutingDAO(dao.PersistenceModeSQLOnly, "sqlite", sqliteDAO, "nosql", nosqlDAO, nil)
	if err != nil {
		t.Fatalf("failed buildRoutingDAO for sql_only with secondary: %v", err)
	}
	if rSQLStandalone.SQLDAO() != sqliteDAO {
		t.Errorf("expected primary sqlDAO to be retained in sql_only mode")
	}
}

func TestValidatePersistenceConfig(t *testing.T) {
	// Valid configs
	if err := validatePersistenceConfig("sql_only", "sqlite", ""); err != nil {
		t.Errorf("expected valid sql_only config, got: %v", err)
	}
	if err := validatePersistenceConfig("nosql_only", "nosql", ""); err != nil {
		t.Errorf("expected valid nosql_only config, got: %v", err)
	}
	if err := validatePersistenceConfig("dual_write", "sqlite", "nosql"); err != nil {
		t.Errorf("expected valid dual_write config, got: %v", err)
	}
	if err := validatePersistenceConfig("dual_write_nosql_primary", "nosql", "postgres"); err != nil {
		t.Errorf("expected valid dual_write_nosql_primary config, got: %v", err)
	}

	// Invalid configs
	// 1. sql_only with NoSQL driver
	if err := validatePersistenceConfig("sql_only", "nosql", ""); err == nil {
		t.Error("expected error for sql_only with nosql driver, got nil")
	}
	// 2. nosql_only with SQL driver
	if err := validatePersistenceConfig("nosql_only", "sqlite", ""); err == nil {
		t.Error("expected error for nosql_only with sqlite driver, got nil")
	}
	// 3. dual_write without secondary driver
	if err := validatePersistenceConfig("dual_write", "sqlite", ""); err == nil {
		t.Error("expected error for dual_write without secondary driver, got nil")
	}
	// 4. dual_write with both SQL drivers
	if err := validatePersistenceConfig("dual_write", "sqlite", "postgres"); err == nil {
		t.Error("expected error for dual_write with two SQL drivers, got nil")
	}
	// 5. dual_write with both NoSQL drivers
	if err := validatePersistenceConfig("dual_write", "nosql", "memory"); err == nil {
		t.Error("expected error for dual_write with two NoSQL drivers, got nil")
	}
	// 6. Unknown persistence mode
	if err := validatePersistenceConfig("unknown_mode", "sqlite", ""); err == nil {
		t.Error("expected error for unknown persistence mode, got nil")
	}
	// 7. dual_write with inverted drivers (NoSQL primary)
	if err := validatePersistenceConfig("dual_write", "nosql", "sqlite"); err == nil {
		t.Error("expected error for dual_write with NoSQL primary driver, got nil")
	}
	// 8. dual_write_nosql_primary with inverted drivers (SQL primary)
	if err := validatePersistenceConfig("dual_write_nosql_primary", "sqlite", "nosql"); err == nil {
		t.Error("expected error for dual_write_nosql_primary with SQL primary driver, got nil")
	}
	// 9. dual_write_nosql_primary without secondary driver
	if err := validatePersistenceConfig("dual_write_nosql_primary", "nosql", ""); err == nil {
		t.Error("expected error for dual_write_nosql_primary without secondary driver, got nil")
	}
}

func TestRunMigrationCLI_NoSQLAndRoutingDAO(t *testing.T) {
	nosqlDAO, err := dao.NewNoSQLDAO("")
	if err != nil {
		t.Fatalf("failed to create nosql dao: %v", err)
	}
	defer func() { _ = nosqlDAO.Close() }()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Status on NoSQL DAO
	out := captureStdout(func() {
		runMigrationCLI(nosqlDAO, []string{"status"}, logger)
	})
	if !strings.Contains(out, "VERSION") {
		t.Errorf("expected status output with VERSION, got: %s", out)
	}

	// Up on NoSQL DAO
	out = captureStdout(func() {
		runMigrationCLI(nosqlDAO, []string{"up"}, logger)
	})
	if !strings.Contains(out, "Successfully applied") {
		t.Errorf("expected migration up success, got: %s", out)
	}

	// RoutingDAO migration CLI
	sqliteDAO, err := dao.NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite dao: %v", err)
	}
	defer func() { _ = sqliteDAO.Close() }()

	rDAO, err := dao.NewRoutingDAO(dao.PersistenceModeDualWrite, sqliteDAO, nosqlDAO, logger)
	if err != nil {
		t.Fatalf("failed to create routing dao: %v", err)
	}

	out = captureStdout(func() {
		runMigrationCLI(rDAO, []string{"up"}, logger)
	})
	if !strings.Contains(out, "Successfully applied") {
		t.Errorf("expected migration up success on routing dao, got: %s", out)
	}
}

