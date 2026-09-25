package dao

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/borch-ai/lid-challenge/internal/models"
	"github.com/borch-ai/lid-challenge/internal/security"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"modernc.org/sqlite" // Pure-Go SQLite driver
)

func init() {
	// Register Unicode-aware LOWER function for SQLite so case-folding handles non-ASCII UTF-8 characters (e.g. Élysée)
	// identically to Go's strings.ToLower and PostgreSQL's Unicode LOWER.
	sqlite.MustRegisterDeterministicScalarFunction("lower", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if len(args) == 0 || args[0] == nil {
			return nil, nil
		}
		switch v := args[0].(type) {
		case string:
			return strings.ToLower(v), nil
		case []byte:
			return strings.ToLower(string(v)), nil
		default:
			return strings.ToLower(fmt.Sprint(v)), nil
		}
	})

	// Ensure PRAGMA foreign_keys = ON is executed on every acquired SQLite connection
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		_, err := conn.ExecContext(context.Background(), "PRAGMA foreign_keys = ON;", nil)
		return err
	})
}

const maxTxRetries = 3

// isRetryableTxError determines whether a database error warrants a transaction retry,
// specifically CockroachDB / PostgreSQL serialization failures (SQLSTATE 40001), deadlock errors (SQLSTATE 40P01), or restart signals.
func isRetryableTxError(err error) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		// 40001 is serialization_failure, 40P01 is deadlock_detected (standard SQLSTATEs for PostgreSQL / CockroachDB transaction retry)
		return pqErr.Code == "40001" || pqErr.Code == "40P01"
	}
	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "40001") ||
		strings.Contains(errMsg, "40p01") ||
		strings.Contains(errMsg, "deadlock detected") ||
		strings.Contains(errMsg, "restart transaction") ||
		strings.Contains(errMsg, "retry transaction")
}

// isUniqueViolation detects unique constraint violations across supported databases,
// inspecting PostgreSQL/CockroachDB SQLSTATE 23505 directly before falling back to SQLite error text.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "unique") ||
		strings.Contains(errStr, "duplicate") ||
		strings.Contains(errStr, "23505")
}

// SQLDAO implements UserDAO and MigratableDAO using Go standard database/sql with pluggable dialect support.
type SQLDAO struct {
	db       *sql.DB
	dialect  Dialect
	migrator *Migrator
}

// NewSQLDAO creates a new SQLDAO wrapping an existing *sql.DB and dialect.
func NewSQLDAO(db *sql.DB, dialect Dialect) *SQLDAO {
	migrator, _ := NewMigrator(db, dialect)
	return &SQLDAO{
		db:       db,
		dialect:  dialect,
		migrator: migrator,
	}
}

// NewSQLiteDAO connects to a SQLite database and initializes the DAO.
func NewSQLiteDAO(dsn string) (*SQLDAO, error) {
	if dsn == "" {
		dsn = fmt.Sprintf("file:mem_%s?mode=memory&cache=shared&_pragma=foreign_keys(1)", uuid.New().String())
	} else if !strings.Contains(dsn, "foreign_keys") {
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		dsn = fmt.Sprintf("%s%s_pragma=foreign_keys(1)", dsn, separator)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// SQLite connection tuning
	db.SetMaxOpenConns(1) // Avoid table lock contention in file-based SQLite
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping sqlite database: %w", err)
	}

	// SQLite does not enforce foreign keys by default; enable pragma explicitly
	if _, err := db.ExecContext(pingCtx, "PRAGMA foreign_keys = ON;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to enable foreign keys on sqlite database: %w", err)
	}

	return NewSQLDAO(db, SQLiteDialect{}), nil
}

// NewPostgresDAO connects to a PostgreSQL or CockroachDB database and initializes the DAO.
func NewPostgresDAO(dsn string) (*SQLDAO, error) {
	if dsn == "" {
		return nil, fmt.Errorf("postgres DSN cannot be empty")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open postgres database: %w", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping postgres database: %w", err)
	}

	return NewSQLDAO(db, PostgresDialect{}), nil
}

// Migrate executes the schema migrations for the configured dialect to bring the database to the latest version.
func (s *SQLDAO) Migrate(ctx context.Context) error {
	if s.migrator != nil {
		_, err := s.migrator.Up(ctx)
		return err
	}
	ddl := s.dialect.SchemaDDL()
	_, err := s.db.ExecContext(ctx, ddl)
	if err != nil {
		return fmt.Errorf("failed to execute schema migration: %w", err)
	}
	return nil
}

// MigrateUp executes all pending database migrations in ascending order.
func (s *SQLDAO) MigrateUp(ctx context.Context) (int, error) {
	if s.migrator == nil {
		return 0, errors.New("migrator is not initialized")
	}
	return s.migrator.Up(ctx)
}

// MigrateDown rolls back the specified number of applied migrations in descending order.
func (s *SQLDAO) MigrateDown(ctx context.Context, steps int) (int, error) {
	if s.migrator == nil {
		return 0, errors.New("migrator is not initialized")
	}
	return s.migrator.Down(ctx, steps)
}

// MigrationVersion returns the highest applied migration version.
func (s *SQLDAO) MigrationVersion(ctx context.Context) (int64, error) {
	if s.migrator == nil {
		return 0, errors.New("migrator is not initialized")
	}
	return s.migrator.Version(ctx)
}

// MigrationStatus returns status information for all registered migrations.
func (s *SQLDAO) MigrationStatus(ctx context.Context) ([]MigrationStatus, error) {
	if s.migrator == nil {
		return nil, errors.New("migrator is not initialized")
	}
	return s.migrator.Status(ctx)
}

// Migrator returns the underlying Migrator instance.
func (s *SQLDAO) Migrator() *Migrator {
	return s.migrator
}

// Ping verifies connectivity to the underlying database pool.
func (s *SQLDAO) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close closes the underlying database pool.
func (s *SQLDAO) Close() error {
	return s.db.Close()
}

// CreateUser persists both user profile and credentials within an atomic transaction.
func (s *SQLDAO) CreateUser(ctx context.Context, profile *models.UserProfile, cred *models.UserCredential) (string, error) {
	if profile == nil || cred == nil {
		return "", ErrInvalidInput
	}
	if strings.TrimSpace(profile.Name) == "" || strings.TrimSpace(profile.Phone) == "" {
		return "", fmt.Errorf("%w: name and phone are required", ErrInvalidInput)
	}
	if strings.TrimSpace(cred.Username) == "" || strings.TrimSpace(cred.PasswordHash) == "" {
		return "", fmt.Errorf("%w: username and password_hash are required", ErrInvalidInput)
	}

	if profile.ID == "" {
		profile.ID = uuid.New().String()
	}
	cred.UserID = profile.ID

	now := time.Now().UTC()
	if profile.CreatedAt.IsZero() {
		profile.CreatedAt = now
	}
	profile.UpdatedAt = now

	if cred.CreatedAt.IsZero() {
		cred.CreatedAt = now
	}
	cred.UpdatedAt = now

	if cred.Method == "" {
		cred.Method = security.DefaultHashMethod
	} else if cred.Method != security.DefaultHashMethod {
		return "", fmt.Errorf("%w: unsupported credential hash method %q", ErrInvalidInput, cred.Method)
	}

	for attempt := 0; attempt < maxTxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(20*(1<<attempt)) * time.Millisecond):
			}
		}

		err := s.createUserTx(ctx, profile, cred)
		if err == nil {
			return profile.ID, nil
		}
		if isRetryableTxError(err) && attempt < maxTxRetries-1 {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("transaction exceeded maximum retry attempts")
}

func (s *SQLDAO) createUserTx(ctx context.Context, profile *models.UserProfile, cred *models.UserCredential) (retErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = tx.Rollback()
		}
	}()

	profileQuery := s.dialect.Rebind(`
		INSERT INTO user_profile (
			id, name, phone, street_address, locality, region, postal_code, country, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)

	_, err = tx.ExecContext(ctx, profileQuery,
		profile.ID,
		profile.Name,
		profile.Phone,
		profile.Address.StreetAddress,
		profile.Address.Locality,
		profile.Address.Region,
		profile.Address.PostalCode,
		profile.Address.Country,
		profile.CreatedAt,
		profile.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert user profile: %w", err)
	}

	credQuery := s.dialect.Rebind(`
		INSERT INTO user_credential (
			user_id, username, method, password_hash, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?)
	`)

	_, err = tx.ExecContext(ctx, credQuery,
		cred.UserID,
		cred.Username,
		cred.Method,
		cred.PasswordHash,
		cred.CreatedAt,
		cred.UpdatedAt,
	)
	if err != nil {
		// Handle unique constraint violations
		if isUniqueViolation(err) {
			return ErrUsernameTaken
		}
		return fmt.Errorf("failed to insert user credential: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// GetProfile retrieves a single user profile by its user ID.
func (s *SQLDAO) GetProfile(ctx context.Context, userID string) (*models.UserProfile, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrInvalidInput
	}

	query := s.dialect.Rebind(`
		SELECT id, name, phone, street_address, locality, region, postal_code, country, created_at, updated_at
		FROM user_profile
		WHERE id = ?
	`)

	row := s.db.QueryRowContext(ctx, query, userID)

	var p models.UserProfile
	err := row.Scan(
		&p.ID,
		&p.Name,
		&p.Phone,
		&p.Address.StreetAddress,
		&p.Address.Locality,
		&p.Address.Region,
		&p.Address.PostalCode,
		&p.Address.Country,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("failed to query user profile: %w", err)
	}

	return &p, nil
}

// escapeLike escapes SQL LIKE wildcard characters ('%', '_', and the escape character '\').
func escapeLike(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '%', '_', '\\':
			b.WriteRune('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SearchProfiles filters user profiles by search attributes with pagination.
//
// Indexing & Query Architecture:
//   - Exact match filters (locality, region, country) leverage standard B-Tree indexes.
//   - Substring queries (name, phone) use parameterized LIKE with escaped wildcards.
//   - Both SQLite and PostgreSQL DDL provision expression indexes (e.g. LOWER(name)).
//   - Note on Substring vs B-Tree Indexing: Standard B-Tree indexes cannot index leading wildcards (%term%).
//     In high-scale production PostgreSQL clusters, trigram GIN indexes (pg_trgm) or Full-Text Search (FTS)
//     should be provisioned to avoid sequential scans. Clamped pagination (LIMIT <= 100, OFFSET <= 10000) bounds result sizes.
func (s *SQLDAO) SearchProfiles(ctx context.Context, query models.SearchQuery) ([]*models.UserProfile, error) {
	var whereClauses []string
	var args []any

	if query.Name != "" {
		whereClauses = append(whereClauses, "LOWER(name) LIKE LOWER(?) ESCAPE '\\'")
		args = append(args, "%"+escapeLike(query.Name)+"%")
	}
	if query.Phone != "" {
		whereClauses = append(whereClauses, "phone LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(query.Phone)+"%")
	}
	if query.Locality != "" {
		whereClauses = append(whereClauses, "LOWER(locality) = LOWER(?)")
		args = append(args, query.Locality)
	}
	if query.Region != "" {
		whereClauses = append(whereClauses, "LOWER(region) = LOWER(?)")
		args = append(args, query.Region)
	}
	if query.Country != "" {
		whereClauses = append(whereClauses, "LOWER(country) = LOWER(?)")
		args = append(args, query.Country)
	}

	baseQuery := "SELECT id, name, phone, street_address, locality, region, postal_code, country, created_at, updated_at FROM user_profile"
	if len(whereClauses) > 0 {
		baseQuery += " WHERE " + strings.Join(whereClauses, " AND ")
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

	baseQuery += " ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	reboundQuery := s.dialect.Rebind(baseQuery)

	rows, err := s.db.QueryContext(ctx, reboundQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to execute search query: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var results []*models.UserProfile
	for rows.Next() {
		var p models.UserProfile
		if err := rows.Scan(
			&p.ID,
			&p.Name,
			&p.Phone,
			&p.Address.StreetAddress,
			&p.Address.Locality,
			&p.Address.Region,
			&p.Address.PostalCode,
			&p.Address.Country,
			&p.CreatedAt,
			&p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan user profile row: %w", err)
		}
		results = append(results, &p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating search results: %w", err)
	}

	return results, nil
}

// GetCredential retrieves a user credential record by username.
func (s *SQLDAO) GetCredential(ctx context.Context, username string) (*models.UserCredential, error) {
	if strings.TrimSpace(username) == "" {
		return nil, ErrInvalidInput
	}

	query := s.dialect.Rebind(`
		SELECT user_id, username, method, password_hash, created_at, updated_at
		FROM user_credential
		WHERE username = ?
	`)

	row := s.db.QueryRowContext(ctx, query, username)

	var c models.UserCredential
	err := row.Scan(
		&c.UserID,
		&c.Username,
		&c.Method,
		&c.PasswordHash,
		&c.CreatedAt,
		&c.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("failed to query user credential: %w", err)
	}

	return &c, nil
}

// VerifyUserCredential validates password authentication against stored hash
// and returns the corresponding UserProfile.
func (s *SQLDAO) VerifyUserCredential(ctx context.Context, username, password string) (*models.UserProfile, error) {
	cred, err := s.GetCredential(ctx, username)
	if err != nil {
		return nil, err
	}

	if err := security.CheckPassword(cred.PasswordHash, password); err != nil {
		return nil, err
	}

	return s.GetProfile(ctx, cred.UserID)
}
