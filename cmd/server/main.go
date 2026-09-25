// Package main is the entrypoint for the LID Challenge service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/borch-ai/lid-challenge/internal/api"
	"github.com/borch-ai/lid-challenge/internal/config"
	"github.com/borch-ai/lid-challenge/internal/dao"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	logLevel := slog.LevelInfo
	if cfg.Debug {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	logger.Info("starting LID Challenge service",
		slog.String("db_driver", cfg.DBDriver),
		slog.Int("port", cfg.Server.Port),
	)

	// Initialize database DAO based on driver
	var userDAO dao.UserDAO
	switch strings.ToLower(strings.TrimSpace(cfg.DBDriver)) {
	case "sqlite", "sqlite3":
		userDAO, err = dao.NewSQLiteDAO(cfg.DBDSN)
	case "postgres", "postgresql", "cockroach", "cockroachdb":
		userDAO, err = dao.NewPostgresDAO(cfg.DBDSN)
	default:
		logger.Error("unsupported database driver", slog.String("driver", cfg.DBDriver))
		os.Exit(1)
	}
	if err != nil {
		logger.Error("failed to connect to database", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() {
		_ = userDAO.Close()
	}()

	// Handle migration CLI command if requested
	if len(os.Args) > 1 && (os.Args[1] == "migrate" || os.Args[1] == "--migrate") {
		runMigrationCLI(userDAO, os.Args[2:], logger)
		return
	}

	// Execute migrations on startup if enabled
	if cfg.MigrateOnStartup {
		migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer migrateCancel()
		if err := userDAO.Migrate(migrateCtx); err != nil {
			logger.Error("failed to migrate database schema", slog.Any("error", err))
			os.Exit(1)
		}
		logger.Info("database schema migrated successfully")
	}

	// Construct API server
	apiServer, err := api.NewServer(userDAO, cfg.Server, logger)
	if err != nil {
		logger.Error("failed to create api server", "error", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Server shutdown channel
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("server listening", slog.String("addr", httpServer.Addr))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	sig := <-shutdownChan
	logger.Info("received shutdown signal, shutting down gracefully", slog.String("signal", sig.String()))

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", slog.Any("error", err))
		os.Exit(1)
	}

	logger.Info("server shut down successfully")
}

func runMigrationCLI(userDAO dao.UserDAO, args []string, logger *slog.Logger) {
	migratable, ok := userDAO.(dao.MigratableDAO)
	if !ok {
		logger.Error("configured DAO does not support migration operations")
		os.Exit(1)
	}

	subcmd := "up"
	if len(args) > 0 {
		subcmd = strings.ToLower(strings.TrimSpace(args[0]))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch subcmd {
	case "up":
		count, err := migratable.MigrateUp(ctx)
		if err != nil {
			logger.Error("migration up failed", slog.Any("error", err))
			os.Exit(1)
		}
		ver, _ := migratable.MigrationVersion(ctx)
		fmt.Printf("Successfully applied %d migration(s). Current version: %d\n", count, ver)

	case "down":
		steps := 1
		if len(args) > 1 {
			var err error
			steps, err = strconv.Atoi(args[1])
			if err != nil || steps < 1 {
				fmt.Fprintf(os.Stderr, "invalid steps argument: %q (must be a positive integer)\n", args[1])
				os.Exit(1)
			}
		}
		count, err := migratable.MigrateDown(ctx, steps)
		if err != nil {
			logger.Error("migration down failed", slog.Any("error", err))
			os.Exit(1)
		}
		ver, _ := migratable.MigrationVersion(ctx)
		fmt.Printf("Successfully rolled back %d migration(s). Current version: %d\n", count, ver)

	case "status":
		statuses, err := migratable.MigrationStatus(ctx)
		if err != nil {
			logger.Error("failed to retrieve migration status", slog.Any("error", err))
			os.Exit(1)
		}
		fmt.Printf("%-8s %-10s %-30s %s\n", "VERSION", "STATUS", "APPLIED AT", "NAME")
		for _, s := range statuses {
			statusStr := "PENDING"
			appliedAtStr := "-"
			if s.Applied {
				statusStr = "APPLIED"
				if s.AppliedAt != nil {
					appliedAtStr = s.AppliedAt.Format("2006-01-02 15:04:05 UTC")
				}
			}
			fmt.Printf("%06d   %-10s %-30s %s\n", s.Version, statusStr, appliedAtStr, s.Name)
		}

	case "version":
		ver, err := migratable.MigrationVersion(ctx)
		if err != nil {
			logger.Error("failed to retrieve migration version", slog.Any("error", err))
			os.Exit(1)
		}
		fmt.Printf("Current schema version: %d\n", ver)

	case "help", "--help", "-h":
		fmt.Println("Usage: lid-server migrate [up|down [steps]|status|version]")
		fmt.Println()
		fmt.Println("Commands:")
		fmt.Println("  up              Apply all pending migrations (default)")
		fmt.Println("  down [steps]    Roll back [steps] migrations (default: 1)")
		fmt.Println("  status          Show status of all registered migrations")
		fmt.Println("  version         Show current database schema version")

	default:
		fmt.Fprintf(os.Stderr, "unknown migration subcommand: %q. Run 'lid-server migrate help' for usage.\n", subcmd)
		os.Exit(1)
	}
}
