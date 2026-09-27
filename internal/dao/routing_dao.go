// Package dao defines data access object interfaces and persistence abstractions.
package dao

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/borch-ai/lid-challenge/internal/models"
)

// PersistenceMode defines the operational mode for datastore routing and dual-writing.
type PersistenceMode string

const (
	// PersistenceModeSQLOnly routes all operations strictly to the SQL datastore.
	PersistenceModeSQLOnly PersistenceMode = "sql_only"

	// PersistenceModeDualWrite treats SQL as the primary source of truth and asynchronously/synchronously
	// replicates writes to the secondary NoSQL datastore while serving reads from SQL.
	PersistenceModeDualWrite PersistenceMode = "dual_write"

	// PersistenceModeDualWriteNoSQLPrimary treats NoSQL as the primary source of truth and replicates
	// writes to the secondary SQL datastore while serving reads from NoSQL.
	PersistenceModeDualWriteNoSQLPrimary PersistenceMode = "dual_write_nosql_primary"

	// PersistenceModeNoSQLOnly routes all operations strictly to the NoSQL datastore.
	PersistenceModeNoSQLOnly PersistenceMode = "nosql_only"
)

// ParsePersistenceMode validates and parses a string into a known PersistenceMode.
func ParsePersistenceMode(s string) (PersistenceMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "sql_only", "sql", "":
		return PersistenceModeSQLOnly, nil
	case "dual_write", "dualwrite", "dual-write":
		return PersistenceModeDualWrite, nil
	case "dual_write_nosql_primary", "dualwrite_nosql", "dual-write-nosql":
		return PersistenceModeDualWriteNoSQLPrimary, nil
	case "nosql_only", "nosql", "document":
		return PersistenceModeNoSQLOnly, nil
	default:
		return "", fmt.Errorf("invalid persistence mode: %q (expected sql_only, dual_write, dual_write_nosql_primary, or nosql_only)", s)
	}
}

// RoutingDAO wraps primary and secondary UserDAO implementations to provide
// flexible routing, migration toggling, and zero-downtime dual-write capabilities.
type RoutingDAO struct {
	mu               sync.RWMutex
	mode             PersistenceMode
	primary          UserDAO
	secondary        UserDAO
	logger           *slog.Logger
	onSecondaryError func(op string, err error)
}

// NewRoutingDAO creates and configures a new RoutingDAO instance.
func NewRoutingDAO(mode PersistenceMode, primary, secondary UserDAO, logger *slog.Logger) (*RoutingDAO, error) {
	parsedMode, err := ParsePersistenceMode(string(mode))
	if err != nil {
		return nil, err
	}
	if primary == nil {
		return nil, errors.New("primary DAO cannot be nil")
	}
	if (parsedMode == PersistenceModeDualWrite || parsedMode == PersistenceModeDualWriteNoSQLPrimary) && secondary == nil {
		return nil, fmt.Errorf("secondary DAO is required for dual-write persistence mode %q", parsedMode)
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &RoutingDAO{
		mode:      parsedMode,
		primary:   primary,
		secondary: secondary,
		logger:    logger,
	}, nil
}

// Mode returns the currently active persistence mode.
func (r *RoutingDAO) Mode() PersistenceMode {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mode
}

// SetMode updates the active persistence mode dynamically.
func (r *RoutingDAO) SetMode(mode PersistenceMode) error {
	parsedMode, err := ParsePersistenceMode(string(mode))
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if (parsedMode == PersistenceModeDualWrite || parsedMode == PersistenceModeDualWriteNoSQLPrimary) && r.secondary == nil {
		return fmt.Errorf("secondary DAO is required for dual-write persistence mode %q", parsedMode)
	}
	r.mode = parsedMode
	return nil
}

// Primary returns the underlying primary DAO instance.
func (r *RoutingDAO) Primary() UserDAO {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.primary
}

// Secondary returns the underlying secondary DAO instance, if configured.
func (r *RoutingDAO) Secondary() UserDAO {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.secondary
}

// SetOnSecondaryError registers an optional callback for replication failures on the secondary store.
func (r *RoutingDAO) SetOnSecondaryError(fn func(op string, err error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onSecondaryError = fn
}

// CreateUser writes the user profile and credentials to the primary datastore,
// and replicates to the secondary datastore if dual-write is enabled.
func (r *RoutingDAO) CreateUser(ctx context.Context, profile *models.UserProfile, cred *models.UserCredential) (string, error) {
	if profile == nil || cred == nil {
		return "", ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	r.mu.RLock()
	mode := r.mode
	primary := r.primary
	secondary := r.secondary
	logger := r.logger
	errHandler := r.onSecondaryError
	r.mu.RUnlock()

	// Write to primary datastore
	id, err := primary.CreateUser(ctx, profile, cred)
	if err != nil {
		return "", err
	}

	// Replicate to secondary datastore if dual-write mode is active
	if secondary != nil && (mode == PersistenceModeDualWrite || mode == PersistenceModeDualWriteNoSQLPrimary) {
		profCopy := *profile
		profCopy.ID = id
		credCopy := *cred
		credCopy.UserID = id

		if _, secErr := secondary.CreateUser(ctx, &profCopy, &credCopy); secErr != nil {
			logger.Warn("secondary datastore replication write failed",
				slog.String("operation", "CreateUser"),
				slog.String("mode", string(mode)),
				slog.String("user_id", id),
				slog.Any("error", secErr),
			)
			if errHandler != nil {
				errHandler("CreateUser", secErr)
			}
		}
	}

	return id, nil
}

// GetProfile retrieves a user profile by unique user ID from the primary datastore.
func (r *RoutingDAO) GetProfile(ctx context.Context, userID string) (*models.UserProfile, error) {
	r.mu.RLock()
	primary := r.primary
	r.mu.RUnlock()
	return primary.GetProfile(ctx, userID)
}

// SearchProfiles finds user profiles matching search criteria using the primary datastore.
func (r *RoutingDAO) SearchProfiles(ctx context.Context, query models.SearchQuery) ([]*models.UserProfile, error) {
	r.mu.RLock()
	primary := r.primary
	r.mu.RUnlock()
	return primary.SearchProfiles(ctx, query)
}

// GetCredential retrieves user credential details by username from the primary datastore.
func (r *RoutingDAO) GetCredential(ctx context.Context, username string) (*models.UserCredential, error) {
	r.mu.RLock()
	primary := r.primary
	r.mu.RUnlock()
	return primary.GetCredential(ctx, username)
}

// VerifyUserCredential validates credentials against the primary datastore.
func (r *RoutingDAO) VerifyUserCredential(ctx context.Context, username, password string) (*models.UserProfile, error) {
	r.mu.RLock()
	primary := r.primary
	r.mu.RUnlock()
	return primary.VerifyUserCredential(ctx, username, password)
}

// Migrate executes migrations on primary and secondary datastores.
func (r *RoutingDAO) Migrate(ctx context.Context) error {
	r.mu.RLock()
	primary := r.primary
	secondary := r.secondary
	r.mu.RUnlock()

	if err := primary.Migrate(ctx); err != nil {
		return fmt.Errorf("primary datastore migration failed: %w", err)
	}

	if secondary != nil {
		if err := secondary.Migrate(ctx); err != nil {
			return fmt.Errorf("secondary datastore migration failed: %w", err)
		}
	}

	return nil
}

// Ping verifies connectivity to the primary and secondary datastores.
func (r *RoutingDAO) Ping(ctx context.Context) error {
	r.mu.RLock()
	primary := r.primary
	secondary := r.secondary
	r.mu.RUnlock()

	if err := primary.Ping(ctx); err != nil {
		return fmt.Errorf("primary datastore ping failed: %w", err)
	}

	if secondary != nil {
		if err := secondary.Ping(ctx); err != nil {
			return fmt.Errorf("secondary datastore ping failed: %w", err)
		}
	}

	return nil
}

// Close closes both primary and secondary datastores.
func (r *RoutingDAO) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var errs []error
	if r.primary != nil {
		if err := r.primary.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close primary datastore: %w", err))
		}
	}

	if r.secondary != nil {
		if err := r.secondary.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close secondary datastore: %w", err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// MigrateUp executes pending migrations on primary and secondary datastores.
func (r *RoutingDAO) MigrateUp(ctx context.Context) (int, error) {
	r.mu.RLock()
	primary := r.primary
	secondary := r.secondary
	r.mu.RUnlock()

	var count int
	if m, ok := primary.(MigratableDAO); ok {
		var err error
		count, err = m.MigrateUp(ctx)
		if err != nil {
			return count, fmt.Errorf("primary datastore migrate up failed: %w", err)
		}
	}

	if secondary != nil {
		if m, ok := secondary.(MigratableDAO); ok {
			if _, err := m.MigrateUp(ctx); err != nil {
				return count, fmt.Errorf("secondary datastore migrate up failed: %w", err)
			}
		}
	}

	return count, nil
}

// MigrateDown rolls back migrations on the primary datastore.
func (r *RoutingDAO) MigrateDown(ctx context.Context, steps int) (int, error) {
	r.mu.RLock()
	primary := r.primary
	r.mu.RUnlock()

	if m, ok := primary.(MigratableDAO); ok {
		return m.MigrateDown(ctx, steps)
	}
	return 0, nil
}

// MigrationVersion returns the schema version of the primary datastore.
func (r *RoutingDAO) MigrationVersion(ctx context.Context) (int64, error) {
	r.mu.RLock()
	primary := r.primary
	r.mu.RUnlock()

	if m, ok := primary.(MigratableDAO); ok {
		return m.MigrationVersion(ctx)
	}
	return 1, nil
}

// MigrationStatus returns migration statuses from the primary datastore.
func (r *RoutingDAO) MigrationStatus(ctx context.Context) ([]MigrationStatus, error) {
	r.mu.RLock()
	primary := r.primary
	r.mu.RUnlock()

	if m, ok := primary.(MigratableDAO); ok {
		return m.MigrationStatus(ctx)
	}
	return nil, nil
}
