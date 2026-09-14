package dao

import (
	"fmt"
	"strconv"
	"strings"
)

// Dialect abstracts database-specific SQL behavior, such as placeholder syntax and schema DDL.
type Dialect interface {
	Name() string
	Rebind(query string) string
	SchemaDDL() string
}

// SQLiteDialect implements Dialect for SQLite engines.
type SQLiteDialect struct{}

// Name returns the dialect identifier "sqlite".
func (d SQLiteDialect) Name() string {
	return "sqlite"
}

// Rebind for SQLite leaves the standard '?' placeholders intact.
func (d SQLiteDialect) Rebind(query string) string {
	return query
}

// SchemaDDL returns the table creation DDL statements for SQLite.
func (d SQLiteDialect) SchemaDDL() string {
	return `
	CREATE TABLE IF NOT EXISTS user_profile (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		phone TEXT NOT NULL,
		street_address TEXT NOT NULL,
		locality TEXT NOT NULL,
		region TEXT NOT NULL,
		postal_code TEXT NOT NULL,
		country TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_user_profile_phone ON user_profile(phone);
	CREATE INDEX IF NOT EXISTS idx_user_profile_name ON user_profile(name);
	CREATE INDEX IF NOT EXISTS idx_user_profile_name_lower ON user_profile(LOWER(name));
	CREATE INDEX IF NOT EXISTS idx_user_profile_locality_lower ON user_profile(LOWER(locality));
	CREATE INDEX IF NOT EXISTS idx_user_profile_region_lower ON user_profile(LOWER(region));
	CREATE INDEX IF NOT EXISTS idx_user_profile_country_lower ON user_profile(LOWER(country));

	CREATE TABLE IF NOT EXISTS user_credential (
		user_id TEXT PRIMARY KEY,
		username TEXT UNIQUE NOT NULL,
		method TEXT NOT NULL,
		password_hash TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		FOREIGN KEY (user_id) REFERENCES user_profile(id) ON DELETE CASCADE
	);
	`
}

// PostgresDialect implements Dialect for PostgreSQL and CockroachDB engines.
type PostgresDialect struct{}

// Name returns the dialect identifier "postgres".
func (d PostgresDialect) Name() string {
	return "postgres"
}

// Rebind converts '?' placeholders into PostgreSQL '$1', '$2', ... placeholders.
func (d PostgresDialect) Rebind(query string) string {
	var builder strings.Builder
	builder.Grow(len(query) + 16)
	paramIdx := 1

	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			builder.WriteByte('$')
			builder.WriteString(strconv.Itoa(paramIdx))
			paramIdx++
		} else {
			builder.WriteByte(query[i])
		}
	}
	return builder.String()
}

// SchemaDDL returns the table creation DDL statements for PostgreSQL and CockroachDB.
func (d PostgresDialect) SchemaDDL() string {
	return `
	CREATE TABLE IF NOT EXISTS user_profile (
		id VARCHAR(64) PRIMARY KEY,
		name VARCHAR(255) NOT NULL,
		phone VARCHAR(64) NOT NULL,
		street_address TEXT NOT NULL,
		locality VARCHAR(128) NOT NULL,
		region VARCHAR(128) NOT NULL,
		postal_code VARCHAR(32) NOT NULL,
		country VARCHAR(64) NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_user_profile_phone ON user_profile(phone);
	CREATE INDEX IF NOT EXISTS idx_user_profile_name ON user_profile(name);
	CREATE INDEX IF NOT EXISTS idx_user_profile_name_lower ON user_profile((LOWER(name)));
	CREATE INDEX IF NOT EXISTS idx_user_profile_locality_lower ON user_profile((LOWER(locality)));
	CREATE INDEX IF NOT EXISTS idx_user_profile_region_lower ON user_profile((LOWER(region)));
	CREATE INDEX IF NOT EXISTS idx_user_profile_country_lower ON user_profile((LOWER(country)));

	CREATE TABLE IF NOT EXISTS user_credential (
		user_id VARCHAR(64) PRIMARY KEY REFERENCES user_profile(id) ON DELETE CASCADE,
		username VARCHAR(128) UNIQUE NOT NULL,
		method VARCHAR(32) NOT NULL,
		password_hash TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	);
	`
}

// DialectFor returns the appropriate Dialect implementation based on driver name.
func DialectFor(driverName string) (Dialect, error) {
	switch strings.ToLower(driverName) {
	case "sqlite", "sqlite3":
		return SQLiteDialect{}, nil
	case "postgres", "postgresql", "cockroach", "cockroachdb":
		return PostgresDialect{}, nil
	default:
		return nil, fmt.Errorf("unsupported database dialect: %s", driverName)
	}
}
