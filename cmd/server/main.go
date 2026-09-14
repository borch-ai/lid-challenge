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

	// Execute migrations
	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer migrateCancel()
	if err := userDAO.Migrate(migrateCtx); err != nil {
		logger.Error("failed to migrate database schema", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("database schema migrated successfully")

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
