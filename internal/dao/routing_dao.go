// Package dao defines data access object interfaces and persistence abstractions.
package dao

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/borch-ai/lid-challenge/internal/models"
)

// PersistenceMode defines the operational mode for datastore routing and dual-writing.
type PersistenceMode string

const (
	// PersistenceModeSQLOnly routes all operations strictly to the SQL datastore.
	PersistenceModeSQLOnly PersistenceMode = "sql_only"

	// PersistenceModeDualWrite treats SQL as the primary source of truth and
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

// RoutingDAO wraps SQL and NoSQL UserDAO implementations to provide
// flexible routing, migration toggling, and zero-downtime dual-write capabilities.
type RoutingDAO struct {
	mu               sync.RWMutex
	mode             PersistenceMode
	sqlDAO           UserDAO
	nosqlDAO         UserDAO
	logger           *slog.Logger
	onSecondaryError func(op string, err error)
}

// NewRoutingDAO creates and configures a new RoutingDAO instance with explicit SQL and NoSQL roles.
func NewRoutingDAO(mode PersistenceMode, sqlDAO, nosqlDAO UserDAO, logger *slog.Logger) (*RoutingDAO, error) {
	parsedMode, err := ParsePersistenceMode(string(mode))
	if err != nil {
		return nil, err
	}

	switch parsedMode {
	case PersistenceModeSQLOnly:
		if sqlDAO == nil {
			return nil, errors.New("sql DAO cannot be nil for sql_only mode")
		}
	case PersistenceModeNoSQLOnly:
		if nosqlDAO == nil {
			return nil, errors.New("nosql DAO cannot be nil for nosql_only mode")
		}
	case PersistenceModeDualWrite, PersistenceModeDualWriteNoSQLPrimary:
		if sqlDAO == nil {
			return nil, fmt.Errorf("sql DAO is required for dual-write persistence mode %q", parsedMode)
		}
		if nosqlDAO == nil {
			return nil, fmt.Errorf("nosql DAO is required for dual-write persistence mode %q", parsedMode)
		}
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &RoutingDAO{
		mode:      parsedMode,
		sqlDAO:    sqlDAO,
		nosqlDAO:  nosqlDAO,
		logger:    logger,
	}, nil
}

// targets returns the active write primary (and read source) and the replication secondary for the current mode.
func (r *RoutingDAO) targets() (primary, secondary UserDAO) {
	switch r.mode {
	case PersistenceModeSQLOnly:
		return r.sqlDAO, nil
	case PersistenceModeDualWrite:
		return r.sqlDAO, r.nosqlDAO
	case PersistenceModeDualWriteNoSQLPrimary:
		return r.nosqlDAO, r.sqlDAO
	case PersistenceModeNoSQLOnly:
		return r.nosqlDAO, nil
	default:
		return r.sqlDAO, nil
	}
}

// fallback returns the alternate configured datastore if distinct from the primary.
func (r *RoutingDAO) fallback() UserDAO {
	primary, _ := r.targets()
	if primary == r.sqlDAO {
		if r.nosqlDAO != nil && r.nosqlDAO != primary {
			return r.nosqlDAO
		}
		return nil
	}
	if r.sqlDAO != nil && r.sqlDAO != primary {
		return r.sqlDAO
	}
	return nil
}

// Mode returns the currently active persistence mode.
func (r *RoutingDAO) Mode() PersistenceMode {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mode
}

// SetMode updates the active persistence mode dynamically, immediately switching active read/write targets.
func (r *RoutingDAO) SetMode(mode PersistenceMode) error {
	parsedMode, err := ParsePersistenceMode(string(mode))
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	switch parsedMode {
	case PersistenceModeSQLOnly:
		if r.sqlDAO == nil {
			return errors.New("sql DAO is required for sql_only mode")
		}
	case PersistenceModeNoSQLOnly:
		if r.nosqlDAO == nil {
			return errors.New("nosql DAO is required for nosql_only mode")
		}
	case PersistenceModeDualWrite, PersistenceModeDualWriteNoSQLPrimary:
		if r.sqlDAO == nil {
			return fmt.Errorf("sql DAO is required for dual-write persistence mode %q", parsedMode)
		}
		if r.nosqlDAO == nil {
			return fmt.Errorf("nosql DAO is required for dual-write persistence mode %q", parsedMode)
		}
	}

	r.mode = parsedMode
	return nil
}

// Primary returns the active primary DAO instance for the current persistence mode.
func (r *RoutingDAO) Primary() UserDAO {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, _ := r.targets()
	return p
}

// Secondary returns the active secondary replication DAO instance for the current persistence mode.
func (r *RoutingDAO) Secondary() UserDAO {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, s := r.targets()
	return s
}

// SQLDAO returns the underlying SQL datastore instance, if configured.
func (r *RoutingDAO) SQLDAO() UserDAO {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sqlDAO
}

// NoSQLDAO returns the underlying NoSQL datastore instance, if configured.
func (r *RoutingDAO) NoSQLDAO() UserDAO {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.nosqlDAO
}

// SetOnSecondaryError registers an optional callback for replication failures on the secondary store.
func (r *RoutingDAO) SetOnSecondaryError(fn func(op string, err error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onSecondaryError = fn
}

// CreateUser writes the user profile and credentials to the active primary datastore,
// and replicates to the secondary datastore if dual-write mode is active.
func (r *RoutingDAO) CreateUser(ctx context.Context, profile *models.UserProfile, cred *models.UserCredential) (string, error) {
	if profile == nil || cred == nil {
		return "", ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	r.mu.RLock()
	mode := r.mode
	primary, secondary := r.targets()
	fallback := r.fallback()
	logger := r.logger
	errHandler := r.onSecondaryError
	r.mu.RUnlock()

	if primary == nil {
		return "", errors.New("no active primary datastore configured")
	}

	// Preflight check: if an alternate/fallback datastore is configured,
	// verify that the requested username does not already exist in the alternate store.
	// This prevents duplicate username collisions during migration/cutover windows
	// and in standalone modes with an alternate datastore configured.
	// Fails closed if the alternate datastore cannot be queried to preserve uniqueness guarantees.
	if fallback != nil {
		existingCred, altErr := fallback.GetCredential(ctx, cred.Username)
		if altErr == nil && existingCred != nil {
			return "", ErrUsernameTaken
		}
		if altErr != nil && !errors.Is(altErr, ErrUserNotFound) {
			return "", fmt.Errorf("failed to verify username uniqueness against alternate datastore: %w", altErr)
		}
	}

	// Write to active primary datastore
	id, err := primary.CreateUser(ctx, profile, cred)
	if err != nil {
		return "", err
	}

	// Replicate to active secondary datastore if dual-write mode is active
	if secondary != nil && (mode == PersistenceModeDualWrite || mode == PersistenceModeDualWriteNoSQLPrimary) {
		profCopy := *profile
		profCopy.ID = id
		credCopy := *cred
		credCopy.UserID = id

		// Propagate primary's normalized record (e.g. timestamps, hash method, generated ID)
		// so both primary and secondary datastores stay identical across backends.
		if primaryProfile, err := primary.GetProfile(ctx, id); err == nil && primaryProfile != nil {
			profCopy = *primaryProfile
		}
		if primaryCred, err := primary.GetCredential(ctx, cred.Username); err == nil && primaryCred != nil {
			credCopy = *primaryCred
		}

		if _, secErr := secondary.CreateUser(ctx, &profCopy, &credCopy); secErr != nil {
			if errors.Is(secErr, ErrUsernameTaken) {
				logger.Error("secondary datastore replication write failed due to username conflict",
					slog.String("operation", "CreateUser"),
					slog.String("mode", string(mode)),
					slog.String("user_id", id),
					slog.String("username", cred.Username),
					slog.Any("error", secErr),
				)
				if errHandler != nil {
					errHandler("CreateUser", secErr)
				}
				return "", ErrUsernameTaken
			}
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

// GetProfile retrieves a user profile by unique user ID from the active primary datastore,
// falling back to the alternate datastore if missing from the primary prior to historical data backfill.
func (r *RoutingDAO) GetProfile(ctx context.Context, userID string) (*models.UserProfile, error) {
	r.mu.RLock()
	primary, _ := r.targets()
	fallback := r.fallback()
	r.mu.RUnlock()

	if primary == nil {
		return nil, errors.New("no active primary datastore configured")
	}
	prof, err := primary.GetProfile(ctx, userID)
	if errors.Is(err, ErrUserNotFound) && fallback != nil {
		return fallback.GetProfile(ctx, userID)
	}
	return prof, err
}

// fetchUpTo retrieves up to targetCount matching profiles from dao using bounded pages of up to 100.
func fetchUpTo(ctx context.Context, d UserDAO, baseQuery models.SearchQuery, targetCount int) ([]*models.UserProfile, error) {
	if targetCount <= 0 {
		return nil, nil
	}

	const maxBackendPage = 100
	var all []*models.UserProfile
	currentOffset := 0

	for len(all) < targetCount {
		if err := ctx.Err(); err != nil {
			return all, err
		}
		needed := targetCount - len(all)
		pageSize := needed
		if pageSize > maxBackendPage {
			pageSize = maxBackendPage
		}

		pageQuery := baseQuery
		pageQuery.Offset = currentOffset
		pageQuery.Limit = pageSize

		page, err := d.SearchProfiles(ctx, pageQuery)
		if err != nil {
			return all, err
		}
		if len(page) == 0 {
			break
		}

		all = append(all, page...)
		currentOffset += len(page)

		// If fewer records returned than requested, backend has exhausted matching records
		if len(page) < pageSize {
			break
		}
	}

	return all, nil
}

// SearchProfiles finds user profiles matching search criteria using the active primary datastore,
// merging and deduplicating results from the alternate datastore when both datastores are configured.
func (r *RoutingDAO) SearchProfiles(ctx context.Context, query models.SearchQuery) ([]*models.UserProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	primary, _ := r.targets()
	fallback := r.fallback()
	logger := r.logger
	r.mu.RUnlock()

	if primary == nil {
		return nil, errors.New("no active primary datastore configured")
	}

	if fallback == nil {
		return primary.SearchProfiles(ctx, query)
	}

	limit := query.Limit
	if limit <= 0 {
		limit = 20
	} else if limit > 100 {
		limit = 100
	}

	offset := query.Offset
	if offset < 0 {
		offset = 0
	} else if offset > 10000 {
		offset = 10000
	}

	targetCount := offset + limit

	primaryResults, err := fetchUpTo(ctx, primary, query, targetCount)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fallbackResults, fallbackErr := fetchUpTo(ctx, fallback, query, targetCount)
	if fallbackErr != nil {
		if ctx.Err() != nil || errors.Is(fallbackErr, context.Canceled) || errors.Is(fallbackErr, context.DeadlineExceeded) {
			return nil, fallbackErr
		}
		logger.Warn("alternate datastore search query failed during search merge",
			slog.String("operation", "SearchProfiles"),
			slog.Any("error", fallbackErr),
		)
		if offset >= len(primaryResults) {
			return []*models.UserProfile{}, nil
		}
		end := offset + limit
		if end > len(primaryResults) {
			end = len(primaryResults)
		}
		return primaryResults[offset:end], nil
	}

	seen := make(map[string]struct{}, len(primaryResults)+len(fallbackResults))
	merged := make([]*models.UserProfile, 0, len(primaryResults)+len(fallbackResults))

	for _, p := range primaryResults {
		if p == nil {
			continue
		}
		if _, ok := seen[p.ID]; !ok {
			seen[p.ID] = struct{}{}
			merged = append(merged, p)
		}
	}

	for _, p := range fallbackResults {
		if p == nil {
			continue
		}
		if _, ok := seen[p.ID]; !ok {
			seen[p.ID] = struct{}{}
			merged = append(merged, p)
		}
	}

	sort.Slice(merged, func(i, j int) bool {
		if merged[i].CreatedAt.Equal(merged[j].CreatedAt) {
			return merged[i].ID > merged[j].ID
		}
		return merged[i].CreatedAt.After(merged[j].CreatedAt)
	})

	if offset >= len(merged) {
		return []*models.UserProfile{}, nil
	}

	end := offset + limit
	if end > len(merged) {
		end = len(merged)
	}

	return merged[offset:end], nil
}

// GetCredential retrieves user credential details by username from the active primary datastore,
// falling back to the alternate datastore if missing from the primary prior to historical data backfill.
func (r *RoutingDAO) GetCredential(ctx context.Context, username string) (*models.UserCredential, error) {
	r.mu.RLock()
	primary, _ := r.targets()
	fallback := r.fallback()
	r.mu.RUnlock()

	if primary == nil {
		return nil, errors.New("no active primary datastore configured")
	}
	cred, err := primary.GetCredential(ctx, username)
	if errors.Is(err, ErrUserNotFound) && fallback != nil {
		return fallback.GetCredential(ctx, username)
	}
	return cred, err
}

// VerifyUserCredential validates credentials against the active primary datastore,
// falling back to the alternate datastore if missing from the primary prior to historical data backfill.
func (r *RoutingDAO) VerifyUserCredential(ctx context.Context, username, password string) (*models.UserProfile, error) {
	r.mu.RLock()
	primary, _ := r.targets()
	fallback := r.fallback()
	r.mu.RUnlock()

	if primary == nil {
		return nil, errors.New("no active primary datastore configured")
	}
	prof, err := primary.VerifyUserCredential(ctx, username, password)
	if errors.Is(err, ErrUserNotFound) && fallback != nil {
		fallbackProf, fallbackErr := fallback.VerifyUserCredential(ctx, username, password)
		if fallbackErr == nil {
			return fallbackProf, nil
		}
		if !errors.Is(fallbackErr, ErrUserNotFound) {
			return nil, fallbackErr
		}
	}
	return prof, err
}

// Migrate executes migrations on both SQL and NoSQL datastores if configured.
func (r *RoutingDAO) Migrate(ctx context.Context) error {
	r.mu.RLock()
	sqlDAO := r.sqlDAO
	nosqlDAO := r.nosqlDAO
	r.mu.RUnlock()

	if sqlDAO != nil {
		if err := sqlDAO.Migrate(ctx); err != nil {
			return fmt.Errorf("sql datastore migration failed: %w", err)
		}
	}

	if nosqlDAO != nil && nosqlDAO != sqlDAO {
		if err := nosqlDAO.Migrate(ctx); err != nil {
			return fmt.Errorf("nosql datastore migration failed: %w", err)
		}
	}

	return nil
}

// Ping verifies connectivity to both configured datastores.
func (r *RoutingDAO) Ping(ctx context.Context) error {
	r.mu.RLock()
	sqlDAO := r.sqlDAO
	nosqlDAO := r.nosqlDAO
	r.mu.RUnlock()

	if sqlDAO != nil {
		if err := sqlDAO.Ping(ctx); err != nil {
			return fmt.Errorf("sql datastore ping failed: %w", err)
		}
	}

	if nosqlDAO != nil && nosqlDAO != sqlDAO {
		if err := nosqlDAO.Ping(ctx); err != nil {
			return fmt.Errorf("nosql datastore ping failed: %w", err)
		}
	}

	return nil
}

// Close closes both SQL and NoSQL datastores.
func (r *RoutingDAO) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var errs []error
	if r.sqlDAO != nil {
		if err := r.sqlDAO.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close sql datastore: %w", err))
		}
	}

	if r.nosqlDAO != nil && r.nosqlDAO != r.sqlDAO {
		if err := r.nosqlDAO.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close nosql datastore: %w", err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (r *RoutingDAO) migratables() []MigratableDAO {
	var migratables []MigratableDAO
	seen := make(map[UserDAO]bool)

	r.mu.RLock()
	primary, secondary := r.targets()
	candidates := []UserDAO{primary, secondary, r.sqlDAO, r.nosqlDAO}
	r.mu.RUnlock()

	for _, cand := range candidates {
		if cand != nil && !seen[cand] {
			seen[cand] = true
			if m, ok := cand.(MigratableDAO); ok {
				migratables = append(migratables, m)
			}
		}
	}
	return migratables
}

// MigrateUp executes pending migrations on all configured datastores.
func (r *RoutingDAO) MigrateUp(ctx context.Context) (int, error) {
	var totalCount int
	for _, m := range r.migratables() {
		count, err := m.MigrateUp(ctx)
		if err != nil {
			return totalCount, err
		}
		totalCount += count
	}
	return totalCount, nil
}

// MigrateDown rolls back migrations on configured datastores.
// When both SQL and NoSQL datastores are configured in a routed deployment,
// their migration histories are coordinated: SQL has migration 2 (secondary indexes)
// and migration 1 (base user tables), whereas NoSQL has a single migration 1
// (document collection initialization). Rolling down by 1 step from SQL v2
// rolls back SQL indexes to v1 while preserving NoSQL collection and data at v1.
// Rolling down to v0 rolls down both SQL base tables and the NoSQL collection.
func (r *RoutingDAO) MigrateDown(ctx context.Context, steps int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if steps <= 0 {
		return 0, nil
	}

	r.mu.RLock()
	sqlDAO := r.sqlDAO
	nosqlDAO := r.nosqlDAO
	r.mu.RUnlock()

	sqlMig, hasSQL := sqlDAO.(MigratableDAO)
	nosqlMig, hasNoSQL := nosqlDAO.(MigratableDAO)

	// If both SQL and NoSQL are distinct migratable datastores, coordinate rollback
	// to prevent tearing down NoSQL document collections prematurely when SQL is only
	// rolling back secondary index migrations (v2 -> v1).
	if hasSQL && hasNoSQL && sqlDAO != nosqlDAO {
		sqlVer, err := sqlMig.MigrationVersion(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to get sql migration version: %w", err)
		}
		nosqlVer, err := nosqlMig.MigrationVersion(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to get nosql migration version: %w", err)
		}

		targetSQLVer := sqlVer - int64(steps)
		if targetSQLVer < 0 {
			targetSQLVer = 0
		}
		sqlSteps := int(sqlVer - targetSQLVer)

		// Coordinated NoSQL rollback:
		// If SQL still retains base tables (targetSQLVer >= 1), the NoSQL collection must remain at v1.
		// Only if SQL rolls down past base tables (targetSQLVer == 0) does NoSQL roll down to v0.
		var nosqlSteps int
		if targetSQLVer == 0 && nosqlVer > 0 {
			nosqlSteps = int(nosqlVer)
		}

		var totalCount int
		if sqlSteps > 0 {
			count, err := sqlMig.MigrateDown(ctx, sqlSteps)
			if err != nil {
				return totalCount, err
			}
			totalCount += count
		}
		if nosqlSteps > 0 {
			count, err := nosqlMig.MigrateDown(ctx, nosqlSteps)
			if err != nil {
				return totalCount, err
			}
			totalCount += count
		}
		return totalCount, nil
	}

	// Fallback for single datastore configurations or standalone migratables.
	var totalCount int
	for _, m := range r.migratables() {
		count, err := m.MigrateDown(ctx, steps)
		if err != nil {
			return totalCount, err
		}
		totalCount += count
	}
	return totalCount, nil
}

// MigrationVersion returns the schema version of the active primary datastore.
func (r *RoutingDAO) MigrationVersion(ctx context.Context) (int64, error) {
	r.mu.RLock()
	primary, _ := r.targets()
	r.mu.RUnlock()

	if m, ok := primary.(MigratableDAO); ok {
		return m.MigrationVersion(ctx)
	}
	return 1, nil
}

// MigrationStatus returns status information for all registered migrations across configured datastores.
func (r *RoutingDAO) MigrationStatus(ctx context.Context) ([]MigrationStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var allStatuses []MigrationStatus
	for _, m := range r.migratables() {
		statuses, err := m.MigrationStatus(ctx)
		if err != nil {
			return nil, err
		}
		allStatuses = append(allStatuses, statuses...)
	}
	return allStatuses, nil
}
