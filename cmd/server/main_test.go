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

	// 2. Unsupported
	if _, err := initUserDAO("unsupported_driver", ""); err == nil {
		t.Errorf("expected error for unsupported driver, got nil")
	}
}
