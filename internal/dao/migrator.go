package dao

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed migrations/sqlite/*.sql
var sqliteMigrationsFS embed.FS

//go:embed migrations/postgres/*.sql
var postgresMigrationsFS embed.FS

// Migration represents an individual versioned database schema migration.
type Migration struct {
	Version int64
	Name    string
	UpSQL   string
	DownSQL string
}

// MigrationStatus describes the current database state for a specific migration.
type MigrationStatus struct {
	Version   int64      `json:"version"`
	Name      string     `json:"name"`
	Applied   bool       `json:"applied"`
	AppliedAt *time.Time `json:"applied_at,omitempty"`
}

// Migrator manages applying and rolling back database migrations.
type Migrator struct {
	db         *sql.DB
	dialect    Dialect
	migrations []Migration
}

// LoadMigrations scans an fs.FS directory for SQL migration files of the form:
// <version>_<name>.up.sql and <version>_<name>.down.sql.
func LoadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read migrations directory %q: %w", dir, err)
	}

	migrationsMap := make(map[int64]*Migration)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}

		var direction string
		var base string
		if strings.HasSuffix(name, ".up.sql") {
			direction = "up"
			base = strings.TrimSuffix(name, ".up.sql")
		} else if strings.HasSuffix(name, ".down.sql") {
			direction = "down"
			base = strings.TrimSuffix(name, ".down.sql")
		} else {
			return nil, fmt.Errorf("invalid migration filename %q: must end in .up.sql or .down.sql", name)
		}

		parts := strings.SplitN(base, "_", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid migration filename %q: must match <version>_<name>.<up|down>.sql", name)
		}

		version, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid migration version in filename %q: %w", name, err)
		}
		if version <= 0 {
			return nil, fmt.Errorf("invalid migration version in filename %q: version must be positive", name)
		}
		migName := parts[1]

		contentBytes, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("failed to read migration file %q: %w", name, err)
		}
		content := string(contentBytes)

		mig, exists := migrationsMap[version]
		if !exists {
			mig = &Migration{
				Version: version,
				Name:    migName,
			}
			migrationsMap[version] = mig
		} else if mig.Name != migName {
			return nil, fmt.Errorf("migration version %d has conflicting names: %q vs %q", version, mig.Name, migName)
		}

		if direction == "up" {
			mig.UpSQL = content
		} else {
			mig.DownSQL = content
		}
	}

	migrations := make([]Migration, 0, len(migrationsMap))
	for _, m := range migrationsMap {
		if strings.TrimSpace(m.UpSQL) == "" {
			return nil, fmt.Errorf("migration %d (%s) is missing .up.sql content", m.Version, m.Name)
		}
		migrations = append(migrations, *m)
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	return migrations, nil
}

// NewMigrator creates a Migrator using embedded migrations for the given dialect.
func NewMigrator(db *sql.DB, dialect Dialect) (*Migrator, error) {
	if dialect == nil {
		return nil, errors.New("dialect is nil")
	}
	var fsys fs.FS
	var dir string
	switch dialect.Name() {
	case "sqlite":
		fsys = sqliteMigrationsFS
		dir = "migrations/sqlite"
	case "postgres":
		fsys = postgresMigrationsFS
		dir = "migrations/postgres"
	default:
		return nil, fmt.Errorf("unsupported migration dialect: %s", dialect.Name())
	}
	return NewMigratorWithFS(db, dialect, fsys, dir)
}

// NewMigratorWithFS creates a Migrator from a specified fs.FS and directory.
func NewMigratorWithFS(db *sql.DB, dialect Dialect, fsys fs.FS, dir string) (*Migrator, error) {
	if db == nil {
		return nil, errors.New("database connection is nil")
	}
	if dialect == nil {
		return nil, errors.New("dialect is nil")
	}
	migrations, err := LoadMigrations(fsys, dir)
	if err != nil {
		return nil, err
	}
	return &Migrator{
		db:         db,
		dialect:    dialect,
		migrations: migrations,
	}, nil
}

// EnsureTable creates the schema_migrations and schema_migrations_lock tables if they do not already exist.
func (m *Migrator) EnsureTable(ctx context.Context) error {
	var ddl string
	if m.dialect.Name() == "sqlite" {
		ddl = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at DATETIME NOT NULL
		);
		CREATE TABLE IF NOT EXISTS schema_migrations_lock (
			id INT PRIMARY KEY,
			is_locked BOOLEAN NOT NULL DEFAULT 0,
			locked_at DATETIME,
			locked_by TEXT
		);
		INSERT OR IGNORE INTO schema_migrations_lock (id, is_locked) VALUES (1, 0);`
	} else {
		ddl = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS schema_migrations_lock (
			id INT PRIMARY KEY,
			is_locked BOOLEAN NOT NULL DEFAULT FALSE,
			locked_at TIMESTAMPTZ,
			locked_by VARCHAR(255)
		);
		INSERT INTO schema_migrations_lock (id, is_locked) VALUES (1, FALSE) ON CONFLICT (id) DO NOTHING;`
	}
	var lastErr error
	for attempt := 0; attempt < 50; attempt++ {
		_, err := m.db.ExecContext(ctx, ddl)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil || errors.Is(err, sql.ErrConnDone) || strings.Contains(strings.ToLower(err.Error()), "closed") {
			return fmt.Errorf("failed to create schema_migrations tables: %w", lastErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("failed to create schema_migrations tables: %w", lastErr)
}

const (
	lockHeartbeatInterval = 5 * time.Second
	staleLockTimeout      = 1 * time.Minute
)

// acquireLock acquires an exclusive distributed lock on schema_migrations_lock, retrying until ctx expires.
// On success, it launches a background heartbeat goroutine to continuously renew the lock lease,
// and returns an unlock function that stops the heartbeat and releases the lock with ownership fencing.
func (m *Migrator) acquireLock(ctx context.Context) (func(), error) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	hostname, _ := os.Hostname()
	lockOwner := fmt.Sprintf("%s:%d:%s", hostname, os.Getpid(), hex.EncodeToString(b))
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	query := m.dialect.Rebind(`
		UPDATE schema_migrations_lock
		SET is_locked = TRUE, locked_at = ?, locked_by = ?
		WHERE id = 1 AND is_locked = FALSE
	`)

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout or canceled while waiting for migration lock: %w", ctx.Err())
		default:
		}

		now := time.Now().UTC()
		var nowParam any = now
		if m.dialect.Name() == "sqlite" {
			nowParam = now.Format(time.RFC3339Nano)
		}

		res, err := m.db.ExecContext(ctx, query, nowParam, lockOwner)
		if err == nil {
			rows, _ := res.RowsAffected()
			if rows == 1 {
				heartbeatCtx, cancelHeartbeat := context.WithCancel(context.Background())
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					hbTicker := time.NewTicker(lockHeartbeatInterval)
					defer hbTicker.Stop()
					for {
						select {
						case <-heartbeatCtx.Done():
							return
						case <-hbTicker.C:
							_ = m.renewLock(heartbeatCtx, lockOwner)
						}
					}
				}()

				var once sync.Once
				unlock := func() {
					once.Do(func() {
						cancelHeartbeat()
						wg.Wait()
						relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						_ = m.releaseLock(relCtx, lockOwner)
					})
				}
				return unlock, nil
			}
		}

		// Break stale locks older than staleLockTimeout (e.g. from crashed containers)
		m.breakStaleLock(ctx, now)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout or canceled while waiting for migration lock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// renewLock extends the lock lease timestamp for the active lock owner.
func (m *Migrator) renewLock(ctx context.Context, lockOwner string) error {
	now := time.Now().UTC()
	var nowParam any = now
	if m.dialect.Name() == "sqlite" {
		nowParam = now.Format(time.RFC3339Nano)
	}
	query := m.dialect.Rebind(`
		UPDATE schema_migrations_lock
		SET locked_at = ?
		WHERE id = 1 AND is_locked = TRUE AND locked_by = ?
	`)
	_, err := m.db.ExecContext(ctx, query, nowParam, lockOwner)
	return err
}

// releaseLock releases the exclusive distributed lock on schema_migrations_lock,
// verifying current lock ownership so it never clears another replica's lock.
func (m *Migrator) releaseLock(ctx context.Context, lockOwner string) error {
	query := m.dialect.Rebind(`
		UPDATE schema_migrations_lock
		SET is_locked = FALSE, locked_at = NULL, locked_by = NULL
		WHERE id = 1 AND locked_by = ?
	`)
	_, err := m.db.ExecContext(ctx, query, lockOwner)
	return err
}

// breakStaleLock clears any lock that has exceeded staleLockTimeout without heartbeat renewal.
func (m *Migrator) breakStaleLock(ctx context.Context, now time.Time) {
	staleThreshold := now.Add(-staleLockTimeout)
	var staleParam any = staleThreshold
	if m.dialect.Name() == "sqlite" {
		staleParam = staleThreshold.Format(time.RFC3339Nano)
	}
	query := m.dialect.Rebind(`
		UPDATE schema_migrations_lock
		SET is_locked = FALSE, locked_at = NULL, locked_by = NULL
		WHERE id = 1 AND is_locked = TRUE AND locked_at < ?
	`)
	_, _ = m.db.ExecContext(ctx, query, staleParam)
}

func (m *Migrator) applyMigration(ctx context.Context, migration Migration) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin migration transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, migration.UpSQL); err != nil {
		return fmt.Errorf("failed to execute migration %06d (%s) up: %w", migration.Version, migration.Name, err)
	}

	recordSQL := m.dialect.Rebind("INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)")
	now := time.Now().UTC()
	var appliedAtParam any = now
	if m.dialect.Name() == "sqlite" {
		appliedAtParam = now.Format(time.RFC3339Nano)
	}
	if _, err := tx.ExecContext(ctx, recordSQL, migration.Version, migration.Name, appliedAtParam); err != nil {
		return fmt.Errorf("failed to record migration %06d in schema_migrations: %w", migration.Version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit migration transaction: %w", err)
	}
	return nil
}

func (m *Migrator) rollbackMigration(ctx context.Context, migration Migration) error {
	if strings.TrimSpace(migration.DownSQL) == "" {
		return fmt.Errorf("migration %06d (%s) has no down migration defined", migration.Version, migration.Name)
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin rollback transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, migration.DownSQL); err != nil {
		return fmt.Errorf("failed to execute migration %06d (%s) down: %w", migration.Version, migration.Name, err)
	}

	deleteSQL := m.dialect.Rebind("DELETE FROM schema_migrations WHERE version = ?")
	if _, err := tx.ExecContext(ctx, deleteSQL, migration.Version); err != nil {
		return fmt.Errorf("failed to remove migration %06d from schema_migrations: %w", migration.Version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit rollback transaction: %w", err)
	}
	return nil
}

// Up executes all pending migrations in ascending order, coordinated via distributed lock.
func (m *Migrator) Up(ctx context.Context) (int, error) {
	if err := m.EnsureTable(ctx); err != nil {
		return 0, err
	}

	unlock, err := m.acquireLock(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to acquire migration lock: %w", err)
	}
	defer unlock()

	applied, err := m.getAppliedVersions(ctx)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, mig := range m.migrations {
		if _, exists := applied[mig.Version]; exists {
			continue
		}

		if err := m.applyMigration(ctx, mig); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// Down rolls back the specified number of applied migrations in descending order, coordinated via distributed lock.
// If steps <= 0, exactly 1 migration is rolled back.
func (m *Migrator) Down(ctx context.Context, steps int) (int, error) {
	if err := m.EnsureTable(ctx); err != nil {
		return 0, err
	}

	unlock, err := m.acquireLock(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to acquire migration lock: %w", err)
	}
	defer unlock()

	applied, err := m.getAppliedVersions(ctx)
	if err != nil {
		return 0, err
	}

	if len(applied) == 0 {
		return 0, nil
	}

	if steps <= 0 {
		steps = 1
	}

	var toRollback []Migration
	for i := len(m.migrations) - 1; i >= 0; i-- {
		mig := m.migrations[i]
		if _, exists := applied[mig.Version]; exists {
			toRollback = append(toRollback, mig)
		}
	}

	count := 0
	for _, mig := range toRollback {
		if count >= steps {
			break
		}
		if err := m.rollbackMigration(ctx, mig); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// Version returns the highest applied migration version, or 0 if no migrations have been applied.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	if err := m.EnsureTable(ctx); err != nil {
		return 0, err
	}

	var version sql.NullInt64
	err := m.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve max migration version: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return version.Int64, nil
}

// Status returns the status for all known migrations.
func (m *Migrator) Status(ctx context.Context) ([]MigrationStatus, error) {
	if err := m.EnsureTable(ctx); err != nil {
		return nil, err
	}

	applied, err := m.getAppliedVersions(ctx)
	if err != nil {
		return nil, err
	}

	statuses := make([]MigrationStatus, 0, len(m.migrations))
	for _, mig := range m.migrations {
		st := MigrationStatus{
			Version: mig.Version,
			Name:    mig.Name,
		}
		if appliedAt, ok := applied[mig.Version]; ok {
			st.Applied = true
			st.AppliedAt = &appliedAt
		}
		statuses = append(statuses, st)
	}
	return statuses, nil
}

// Migrations returns a copy of the registered migrations slice.
func (m *Migrator) Migrations() []Migration {
	copied := make([]Migration, len(m.migrations))
	copy(copied, m.migrations)
	return copied
}

func (m *Migrator) getAppliedVersions(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := m.db.QueryContext(ctx, "SELECT version, applied_at FROM schema_migrations ORDER BY version ASC")
	if err != nil {
		return nil, fmt.Errorf("failed to query schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[int64]time.Time)
	for rows.Next() {
		var v int64
		var rawAppliedAt any
		if err := rows.Scan(&v, &rawAppliedAt); err != nil {
			return nil, fmt.Errorf("failed to scan schema_migrations row: %w", err)
		}
		appliedAt, err := parseMigrationTime(rawAppliedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to parse applied_at for migration %d: %w", v, err)
		}
		applied[v] = appliedAt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error reading schema_migrations rows: %w", err)
	}
	return applied, nil
}

func parseMigrationTime(val any) (time.Time, error) {
	switch v := val.(type) {
	case time.Time:
		return v, nil
	case string:
		formats := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02 15:04:05.999999999-07:00",
			"2006-01-02 15:04:05-07:00",
			"2006-01-02 15:04:05.999999999",
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05Z",
		}
		for _, f := range formats {
			if t, err := time.Parse(f, v); err == nil {
				return t, nil
			}
		}
		return time.Time{}, fmt.Errorf("failed to parse time string: %s", v)
	case []byte:
		return parseMigrationTime(string(v))
	default:
		return time.Time{}, fmt.Errorf("unexpected time type %T: %v", val, val)
	}
}
