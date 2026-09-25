package dao

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/borch-ai/lid-challenge/internal/models"
	"github.com/borch-ai/lid-challenge/internal/security"
	"github.com/lib/pq"
)

func setupTestDAO(t *testing.T) *SQLDAO {
	t.Helper()
	// Use unique in-memory database name per test
	dao, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to create sqlite dao: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := dao.Migrate(ctx); err != nil {
		_ = dao.Close()
		t.Fatalf("failed to migrate schema: %v", err)
	}

	t.Cleanup(func() {
		_ = dao.Close()
	})

	return dao
}

func TestSQLDAO_CreateAndGetProfile(t *testing.T) {
	dao := setupTestDAO(t)
	ctx := context.Background()

	hash, _ := security.HashPassword("hunter2")
	profile := &models.UserProfile{
		Name:  "Daniel Borch",
		Phone: "3035559876",
		Address: models.Address{
			StreetAddress: "100 Innovation Way",
			Locality:      "Denver",
			Region:        "CO",
			PostalCode:    "80202",
			Country:       "USA",
		},
	}
	cred := &models.UserCredential{
		Username:     "daniel",
		Method:       security.DefaultHashMethod,
		PasswordHash: hash,
	}

	userID, err := dao.CreateUser(ctx, profile, cred)
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	if userID == "" {
		t.Fatal("expected non-empty user ID")
	}

	// Retrieve profile
	gotProfile, err := dao.GetProfile(ctx, userID)
	if err != nil {
		t.Fatalf("failed to get profile: %v", err)
	}
	if gotProfile.Name != "Daniel Borch" {
		t.Errorf("got name %q, want 'Daniel Borch'", gotProfile.Name)
	}
	if gotProfile.Phone != "3035559876" {
		t.Errorf("got phone %q, want '3035559876'", gotProfile.Phone)
	}
	if gotProfile.Address.Locality != "Denver" {
		t.Errorf("got locality %q, want 'Denver'", gotProfile.Address.Locality)
	}

	// Test non-existent user
	_, err = dao.GetProfile(ctx, "non-existent-id")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound, got: %v", err)
	}

	// Test invalid input
	_, err = dao.GetProfile(ctx, "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got: %v", err)
	}
}

func TestSQLDAO_CreateUser_ValidationAndDuplicate(t *testing.T) {
	dao := setupTestDAO(t)
	ctx := context.Background()

	hash, _ := security.HashPassword("secret")

	// Nil inputs
	if _, err := dao.CreateUser(ctx, nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for nil inputs, got: %v", err)
	}

	// Missing name
	invalidProfile := &models.UserProfile{Phone: "123"}
	validCred := &models.UserCredential{Username: "dan", PasswordHash: hash}
	if _, err := dao.CreateUser(ctx, invalidProfile, validCred); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing name, got: %v", err)
	}

	// Missing phone
	invalidPhoneProfile := &models.UserProfile{Name: "Dan"}
	if _, err := dao.CreateUser(ctx, invalidPhoneProfile, validCred); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing phone, got: %v", err)
	}

	// Missing username
	validProfile := &models.UserProfile{Name: "Dan", Phone: "123"}
	invalidCred := &models.UserCredential{PasswordHash: hash}
	if _, err := dao.CreateUser(ctx, validProfile, invalidCred); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing username, got: %v", err)
	}

	// Missing password hash
	invalidHashCred := &models.UserCredential{Username: "dan"}
	if _, err := dao.CreateUser(ctx, validProfile, invalidHashCred); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing password hash, got: %v", err)
	}

	// Unsupported hash method
	unsupportedMethodCred := &models.UserCredential{Username: "dan", PasswordHash: hash, Method: "argon2id"}
	if _, err := dao.CreateUser(ctx, validProfile, unsupportedMethodCred); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for unsupported hash method, got: %v", err)
	}

	// First creation succeeds with valid inputs
	validCred = &models.UserCredential{Username: "unique_dan", PasswordHash: hash}
	_, err := dao.CreateUser(ctx, validProfile, validCred)
	if err != nil {
		t.Fatalf("unexpected error creating user: %v", err)
	}

	// Duplicate username
	duplicateProfile := &models.UserProfile{Name: "Dan 2", Phone: "456"}
	duplicateCred := &models.UserCredential{Username: "unique_dan", PasswordHash: hash}
	_, err = dao.CreateUser(ctx, duplicateProfile, duplicateCred)
	if !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("expected ErrUsernameTaken for duplicate username, got: %v", err)
	}

	// Verify atomicity: profile insert must have rolled back completely
	_, err = dao.GetProfile(ctx, duplicateProfile.ID)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound for rolled-back profile %s, got: %v", duplicateProfile.ID, err)
	}
}

func TestSQLDAO_SearchProfiles(t *testing.T) {
	dao := setupTestDAO(t)
	ctx := context.Background()

	users := []struct {
		name     string
		phone    string
		locality string
		region   string
		country  string
		username string
	}{
		{"Alice Walker", "3031112222", "Denver", "CO", "USA", "alice"},
		{"Bob Builder", "3039998888", "Boulder", "CO", "USA", "bob"},
		{"Charlie Chaplin", "4155551212", "San Francisco", "CA", "USA", "charlie"},
		{"David Miller", "4420794609", "London", "Greater London", "UK", "david"},
	}

	hash, _ := security.HashPassword("pass123")
	for _, u := range users {
		p := &models.UserProfile{
			Name:  u.name,
			Phone: u.phone,
			Address: models.Address{
				Locality: u.locality,
				Region:   u.region,
				Country:  u.country,
			},
		}
		c := &models.UserCredential{
			Username:     u.username,
			PasswordHash: hash,
		}
		if _, err := dao.CreateUser(ctx, p, c); err != nil {
			t.Fatalf("failed creating test user: %v", err)
		}
	}

	// Search by name substring
	results, err := dao.SearchProfiles(ctx, models.SearchQuery{Name: "walker"})
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 1 || results[0].Name != "Alice Walker" {
		t.Errorf("unexpected name search results: %+v", results)
	}

	// Search by phone
	results, err = dao.SearchProfiles(ctx, models.SearchQuery{Phone: "9998888"})
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 1 || results[0].Name != "Bob Builder" {
		t.Errorf("unexpected phone search results: %+v", results)
	}

	// Search by locality
	results, err = dao.SearchProfiles(ctx, models.SearchQuery{Locality: "Denver"})
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result in Denver, got %d", len(results))
	}

	// Search by country
	results, err = dao.SearchProfiles(ctx, models.SearchQuery{Country: "USA"})
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 results in USA, got %d", len(results))
	}

	// Test pagination limits
	results, err = dao.SearchProfiles(ctx, models.SearchQuery{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 paginated results, got %d", len(results))
	}

	// Test offset
	results, err = dao.SearchProfiles(ctx, models.SearchQuery{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 paginated results with offset, got %d", len(results))
	}

	// Test limit clamp (> 100) and negative offset (< 0)
	results, err = dao.SearchProfiles(ctx, models.SearchQuery{Limit: 150, Offset: -5})
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 4 {
		t.Errorf("expected 4 results with clamped limit, got %d", len(results))
	}

	// Test offset clamp upper bound (> 10000)
	results, err = dao.SearchProfiles(ctx, models.SearchQuery{Limit: 10, Offset: 20000})
	if err != nil {
		t.Fatalf("search error with large offset: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results with clamped large offset, got %d", len(results))
	}

	// Test LIKE metacharacter escaping (% and _)
	// A query with "%" or "_" should be escaped and match literally, not as SQL wildcards
	results, err = dao.SearchProfiles(ctx, models.SearchQuery{Name: "%"})
	if err != nil {
		t.Fatalf("search error with %% wildcard: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for literal '%%' search, got %d", len(results))
	}

	results, err = dao.SearchProfiles(ctx, models.SearchQuery{Phone: "_"})
	if err != nil {
		t.Fatalf("search error with _ wildcard: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for literal '_' search, got %d", len(results))
	}
}

func TestSQLDAO_CredentialAndVerification(t *testing.T) {
	dao := setupTestDAO(t)
	ctx := context.Background()

	hash, _ := security.HashPassword("Sup3rSecret!")
	p := &models.UserProfile{
		Name:  "Test User",
		Phone: "5551234567",
	}
	c := &models.UserCredential{
		Username:     "testuser",
		Method:       security.DefaultHashMethod,
		PasswordHash: hash,
	}

	_, err := dao.CreateUser(ctx, p, c)
	if err != nil {
		t.Fatalf("create user failed: %v", err)
	}

	// Get credential
	cred, err := dao.GetCredential(ctx, "testuser")
	if err != nil {
		t.Fatalf("get credential failed: %v", err)
	}
	if cred.Username != "testuser" {
		t.Errorf("got username %q, want 'testuser'", cred.Username)
	}

	// Get credential empty username
	if _, err := dao.GetCredential(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got: %v", err)
	}

	// Get credential not found
	if _, err := dao.GetCredential(ctx, "nonexistent"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound, got: %v", err)
	}

	// Verify valid password
	verifiedProfile, err := dao.VerifyUserCredential(ctx, "testuser", "Sup3rSecret!")
	if err != nil {
		t.Fatalf("verification failed: %v", err)
	}
	if verifiedProfile.Name != "Test User" {
		t.Errorf("got verified profile name %q, want 'Test User'", verifiedProfile.Name)
	}

	// Verify invalid password
	_, err = dao.VerifyUserCredential(ctx, "testuser", "WrongPassword")
	if !errors.Is(err, security.ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword, got: %v", err)
	}

	// Verify non-existent user
	_, err = dao.VerifyUserCredential(ctx, "ghost", "Sup3rSecret!")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound, got: %v", err)
	}
}

func TestDialect(t *testing.T) {
	pgDialect := PostgresDialect{}
	if pgDialect.Name() != "postgres" {
		t.Errorf("expected 'postgres', got %q", pgDialect.Name())
	}

	rebound := pgDialect.Rebind("SELECT * FROM users WHERE name = ? AND phone = ?")
	expected := "SELECT * FROM users WHERE name = $1 AND phone = $2"
	if rebound != expected {
		t.Errorf("pgDialect.Rebind() = %q, want %q", rebound, expected)
	}

	if len(pgDialect.SchemaDDL()) == 0 {
		t.Error("expected non-empty schema DDL for postgres")
	}

	// Test NewPostgresDAO empty DSN validation
	if _, err := NewPostgresDAO(""); err == nil {
		t.Error("expected error for empty postgres DSN")
	}

	// Test NewPostgresDAO unreachable connection
	if _, err := NewPostgresDAO("postgres://user:pass@127.0.0.1:59999/db?sslmode=disable&connect_timeout=1"); err == nil {
		t.Error("expected connection error for unreachable postgres port")
	}

	// Test NewSQLiteDAO default DSN
	defaultDAO, err := NewSQLiteDAO("")
	if err != nil {
		t.Fatalf("expected NewSQLiteDAO(\"\") to succeed with default in-memory DSN, got: %v", err)
	}
	_ = defaultDAO.Close()

	sqliteDialect := SQLiteDialect{}
	if sqliteDialect.Name() != "sqlite" {
		t.Errorf("expected 'sqlite', got %q", sqliteDialect.Name())
	}
	if sqliteDialect.Rebind("SELECT ?") != "SELECT ?" {
		t.Errorf("expected unchanged query for sqlite rebind")
	}
	if len(sqliteDialect.SchemaDDL()) == 0 {
		t.Error("expected non-empty schema DDL for sqlite")
	}

	// Test isUniqueViolation branches
	if isUniqueViolation(nil) {
		t.Errorf("expected isUniqueViolation(nil) to be false")
	}
	if !isUniqueViolation(&pq.Error{Code: "23505"}) {
		t.Errorf("expected isUniqueViolation to return true for pq.Error with 23505")
	}
	if isUniqueViolation(&pq.Error{Code: "40001"}) {
		t.Errorf("expected isUniqueViolation to return false for pq.Error with 40001")
	}
	if !isUniqueViolation(errors.New("UNIQUE constraint failed")) {
		t.Errorf("expected isUniqueViolation to return true for SQLite unique violation")
	}
	if !isUniqueViolation(errors.New("duplicate key value violates unique constraint")) {
		t.Errorf("expected isUniqueViolation to return true for duplicate error string")
	}
	if isUniqueViolation(errors.New("other error")) {
		t.Errorf("expected isUniqueViolation to return false for unrelated error")
	}

	// Test NewSQLiteDAO with explicit DSN containing question mark
	queryDAO, err := NewSQLiteDAO("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("expected NewSQLiteDAO with query params to succeed, got %v", err)
	}
	_ = queryDAO.Close()

	// DialectFor
	d, err := DialectFor("postgres")
	if err != nil || d.Name() != "postgres" {
		t.Errorf("DialectFor(postgres) failed: %v", err)
	}

	d, err = DialectFor("cockroachdb")
	if err != nil || d.Name() != "postgres" {
		t.Errorf("DialectFor(cockroachdb) failed: %v", err)
	}

	d, err = DialectFor("sqlite3")
	if err != nil || d.Name() != "sqlite" {
		t.Errorf("DialectFor(sqlite3) failed: %v", err)
	}

	_, err = DialectFor("oracle")
	if err == nil {
		t.Error("expected error for unsupported dialect")
	}
}

func TestSQLDAO_Ping(t *testing.T) {
	dao := setupTestDAO(t)
	ctx := context.Background()

	if err := dao.Ping(ctx); err != nil {
		t.Fatalf("expected ping to succeed, got: %v", err)
	}

	_ = dao.Close()
	if err := dao.Ping(ctx); err == nil {
		t.Error("expected ping to fail on closed database")
	}
}

func TestIsRetryableTxError(t *testing.T) {
	if isRetryableTxError(nil) {
		t.Error("expected nil error to not be retryable")
	}

	pqErr := &pq.Error{Code: "40001"}
	if !isRetryableTxError(pqErr) {
		t.Error("expected pq.Error with code 40001 to be retryable")
	}

	pqDeadlockErr := &pq.Error{Code: "40P01"}
	if !isRetryableTxError(pqDeadlockErr) {
		t.Error("expected pq.Error with code 40P01 (deadlock) to be retryable")
	}

	wrappedPqErr := fmt.Errorf("transaction commit failed: %w", pqErr)
	if !isRetryableTxError(wrappedPqErr) {
		t.Error("expected wrapped pq.Error with code 40001 to be retryable")
	}

	pqOtherErr := &pq.Error{Code: "23505"}
	if isRetryableTxError(pqOtherErr) {
		t.Error("expected unique violation not to be retryable")
	}

	crdbErr := errors.New("restart transaction: TransactionRetryWithNewPriorityError: retry transaction")
	if !isRetryableTxError(crdbErr) {
		t.Error("expected crdb restart string to be retryable")
	}

	deadlockStringErr := errors.New("ERROR: deadlock detected (SQLSTATE 40P01)")
	if !isRetryableTxError(deadlockStringErr) {
		t.Error("expected deadlock detected string to be retryable")
	}

	genericErr := errors.New("table not found")
	if isRetryableTxError(genericErr) {
		t.Error("expected generic error not to be retryable")
	}
}

func TestNewDAO_EdgeCases(t *testing.T) {
	// Empty Postgres DSN
	_, err := NewPostgresDAO("")
	if err == nil {
		t.Error("expected error for empty postgres DSN")
	}

	// Unreachable Postgres DSN (fails ping)
	_, err = NewPostgresDAO("postgres://invalid:invalid@127.0.0.1:1/invalid?connect_timeout=1&sslmode=disable")
	if err == nil {
		t.Error("expected ping failure for unreachable postgres DSN")
	}

	// Default SQLite DSN
	d, err := NewSQLiteDAO("")
	if err != nil {
		t.Fatalf("expected NewSQLiteDAO(\"\") to succeed, got: %v", err)
	}
	_ = d.Close()
}

func TestSQLDAO_CreateUser_ContextCanceled(t *testing.T) {
	dao := setupTestDAO(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	profile := &models.UserProfile{
		Name:  "Test",
		Phone: "1234567890",
	}
	cred := &models.UserCredential{
		Username:     "test",
		PasswordHash: "hash",
	}

	_, err := dao.CreateUser(ctx, profile, cred)
	if err == nil {
		t.Error("expected error for canceled context in CreateUser")
	}
}

func TestSQLiteDAO_ForeignKeysEnabled(t *testing.T) {
	dao := setupTestDAO(t)
	ctx := context.Background()

	// 1. Initial pool query has foreign keys enabled
	var fkEnabled int
	err := dao.db.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&fkEnabled)
	if err != nil {
		t.Fatalf("failed to query foreign_keys pragma: %v", err)
	}
	if fkEnabled != 1 {
		t.Fatalf("expected foreign_keys pragma to be 1, got %d", fkEnabled)
	}

	// 2. Acquired dedicated connection also has foreign keys enabled via connection hook
	conn, err := dao.db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to acquire dedicated connection: %v", err)
	}
	defer func() { _ = conn.Close() }()
	var fkConn int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&fkConn); err != nil {
		t.Fatalf("failed to query foreign_keys pragma on dedicated connection: %v", err)
	}
	if fkConn != 1 {
		t.Fatalf("expected foreign_keys pragma on dedicated connection to be 1, got %d", fkConn)
	}

	// 3. Orphaned credential insertion must fail foreign key constraint
	orphanQuery := dao.dialect.Rebind(`
		INSERT INTO user_credential (user_id, username, method, password_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	_, err = conn.ExecContext(ctx, orphanQuery, "nonexistent-user-id", "orphan", "bcrypt", "hash", time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected foreign key constraint violation inserting orphaned credential, got nil")
	}
}

func TestSQLiteDAO_UnicodeCaseFoldingSearch(t *testing.T) {
	dao := setupTestDAO(t)
	ctx := context.Background()

	// Insert user with non-ASCII Unicode characters
	profile := &models.UserProfile{
		Name:  "Élysée Dupont",
		Phone: "555-0199",
		Address: models.Address{
			Locality: "Genève",
			Region:   "Île-de-France",
			Country:  "France",
		},
	}
	cred := &models.UserCredential{
		Username:     "elysee_user",
		PasswordHash: "dummy-hash",
	}

	created, err := dao.CreateUser(ctx, profile, cred)
	if err != nil {
		t.Fatalf("failed to create user with Unicode characters: %v", err)
	}

	// 1. Search with lowercase non-ASCII query
	results, err := dao.SearchProfiles(ctx, models.SearchQuery{Name: "élysée"})
	if err != nil {
		t.Fatalf("failed to search with lowercase Unicode name: %v", err)
	}
	if len(results) == 0 {
		t.Errorf("expected match for 'élysée' against 'Élysée Dupont', got 0 results")
	} else if results[0].ID != created {
		t.Errorf("expected matched profile ID %s, got %s", created, results[0].ID)
	}

	// 2. Search with all-caps non-ASCII query
	results, err = dao.SearchProfiles(ctx, models.SearchQuery{Name: "ÉLYSÉE"})
	if err != nil {
		t.Fatalf("failed to search with uppercase Unicode name: %v", err)
	}
	if len(results) == 0 {
		t.Errorf("expected match for 'ÉLYSÉE' against 'Élysée Dupont', got 0 results")
	}

	// 3. Search locality with lowercase Unicode query
	results, err = dao.SearchProfiles(ctx, models.SearchQuery{Locality: "genève"})
	if err != nil {
		t.Fatalf("failed to search with lowercase Unicode locality: %v", err)
	}
	if len(results) == 0 {
		t.Errorf("expected match for locality 'genève' against 'Genève', got 0 results")
	}
}

func TestSQLiteDAO_IsolatedInMemoryInstances(t *testing.T) {
	ctx := context.Background()

	dao1, err := NewSQLiteDAO("")
	if err != nil {
		t.Fatalf("failed to create dao1: %v", err)
	}
	defer func() { _ = dao1.Close() }()
	if err := dao1.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate dao1: %v", err)
	}

	dao2, err := NewSQLiteDAO("")
	if err != nil {
		t.Fatalf("failed to create dao2: %v", err)
	}
	defer func() { _ = dao2.Close() }()
	if err := dao2.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate dao2: %v", err)
	}

	// Insert in dao1
	p := &models.UserProfile{Name: "Isolated User", Phone: "1234567890"}
	c := &models.UserCredential{Username: "isolated_user", PasswordHash: "hash"}
	_, err = dao1.CreateUser(ctx, p, c)
	if err != nil {
		t.Fatalf("failed to create user in dao1: %v", err)
	}

	// Verify dao2 has zero records (completely isolated database)
	results, err := dao2.SearchProfiles(ctx, models.SearchQuery{})
	if err != nil {
		t.Fatalf("failed to search in dao2: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected dao2 to be completely isolated and empty, got %d records", len(results))
	}
}
