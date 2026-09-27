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

	// driver1 NoSQL, driver2 SQL
	r2, err := buildRoutingDAO(dao.PersistenceModeDualWrite, "nosql", nosqlDAO, "sqlite", sqliteDAO, nil)
	if err != nil {
		t.Fatalf("failed buildRoutingDAO with inverted drivers: %v", err)
	}
	if r2.SQLDAO() != sqliteDAO || r2.NoSQLDAO() != nosqlDAO {
		t.Errorf("expected inverted drivers to map sqlDAO and nosqlDAO correctly")
	}

	// Reject same-kind SQL driver pairs
	if _, err := buildRoutingDAO(dao.PersistenceModeDualWrite, "sqlite", sqliteDAO, "postgres", sqliteDAO, nil); err == nil {
		t.Errorf("expected error when both drivers are SQL, got nil")
	}

	// Reject same-kind NoSQL driver pairs
	if _, err := buildRoutingDAO(dao.PersistenceModeDualWrite, "nosql", nosqlDAO, "memory", nosqlDAO, nil); err == nil {
		t.Errorf("expected error when both drivers are NoSQL, got nil")
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

