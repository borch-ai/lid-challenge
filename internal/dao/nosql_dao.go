// Package dao defines data access object interfaces and persistence abstractions.
package dao

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/borch-ai/lid-challenge/internal/models"
	"github.com/borch-ai/lid-challenge/internal/security"
)

// userDocument represents the unified NoSQL document structure storing
// both user profile attributes and credentials in a single document pattern.
type userDocument struct {
	ID         string                `json:"id"`
	Profile    models.UserProfile    `json:"profile"`
	Credential models.UserCredential `json:"credential"`
	CreatedAt  time.Time             `json:"created_at"`
	UpdatedAt  time.Time             `json:"updated_at"`
}

// NoSQLDAO implements UserDAO using an in-memory document store pattern
// with secondary index management and atomic single-document write semantics.
type NoSQLDAO struct {
	mu         sync.RWMutex
	docs       map[string]*userDocument // Primary index by user ID
	byUsername map[string]string        // Unique secondary index: username -> user ID
	closed     bool
	indexed    bool
}

// NewNoSQLDAO creates a new NoSQLDAO document store instance.
func NewNoSQLDAO(dsn string) (*NoSQLDAO, error) {
	_ = dsn // DSN can specify storage options or collection names
	return &NoSQLDAO{
		docs:       make(map[string]*userDocument),
		byUsername: make(map[string]string),
		indexed:    true,
	}, nil
}

// CreateUser persists a user profile and credentials atomically within a single document.
func (d *NoSQLDAO) CreateUser(ctx context.Context, profile *models.UserProfile, cred *models.UserCredential) (string, error) {
	if profile == nil || cred == nil {
		return "", ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return "", errors.New("nosql dao is closed")
	}

	username := strings.TrimSpace(cred.Username)
	if username == "" {
		return "", ErrInvalidInput
	}

	// Enforce unique secondary index on username
	if _, exists := d.byUsername[username]; exists {
		return "", ErrUsernameTaken
	}

	id := strings.TrimSpace(profile.ID)
	if id == "" {
		id = uuid.New().String()
	}
	now := time.Now().UTC()

	profCopy := *profile
	profCopy.ID = id
	profCopy.CreatedAt = now
	profCopy.UpdatedAt = now

	credCopy := *cred
	credCopy.UserID = id
	credCopy.Username = username
	credCopy.CreatedAt = now
	credCopy.UpdatedAt = now

	doc := &userDocument{
		ID:         id,
		Profile:    profCopy,
		Credential: credCopy,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	d.docs[id] = doc
	d.byUsername[username] = id

	return id, nil
}

// GetProfile retrieves a user profile by unique user ID.
func (d *NoSQLDAO) GetProfile(ctx context.Context, userID string) (*models.UserProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUserNotFound
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	if d.closed {
		return nil, errors.New("nosql dao is closed")
	}

	doc, exists := d.docs[userID]
	if !exists {
		return nil, ErrUserNotFound
	}

	prof := doc.Profile
	return &prof, nil
}

// SearchProfiles finds user profiles matching search criteria with pagination.
func (d *NoSQLDAO) SearchProfiles(ctx context.Context, query models.SearchQuery) ([]*models.UserProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	if d.closed {
		return nil, errors.New("nosql dao is closed")
	}

	var matched []*models.UserProfile

	qName := strings.ToLower(strings.TrimSpace(query.Name))
	qPhone := strings.ToLower(strings.TrimSpace(query.Phone))
	qLocality := strings.ToLower(strings.TrimSpace(query.Locality))
	qRegion := strings.ToLower(strings.TrimSpace(query.Region))
	qCountry := strings.ToLower(strings.TrimSpace(query.Country))

	for _, doc := range d.docs {
		p := doc.Profile
		if qName != "" && !strings.Contains(strings.ToLower(p.Name), qName) {
			continue
		}
		if qPhone != "" && !strings.Contains(strings.ToLower(p.Phone), qPhone) {
			continue
		}
		if qLocality != "" && !strings.Contains(strings.ToLower(p.Address.Locality), qLocality) {
			continue
		}
		if qRegion != "" && !strings.Contains(strings.ToLower(p.Address.Region), qRegion) {
			continue
		}
		if qCountry != "" && !strings.Contains(strings.ToLower(p.Address.Country), qCountry) {
			continue
		}
		profCopy := p
		matched = append(matched, &profCopy)
	}

	// Sort deterministically by CreatedAt DESC, then ID ASC
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].ID < matched[j].ID
		}
		return matched[i].CreatedAt.After(matched[j].CreatedAt)
	})

	limit := query.Limit
	if limit <= 0 {
		limit = 20
	} else if limit > 100 {
		limit = 100
	}

	offset := query.Offset
	if offset < 0 {
		offset = 0
	}
	if offset >= len(matched) {
		return []*models.UserProfile{}, nil
	}

	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}

	return matched[offset:end], nil
}

// GetCredential retrieves user credentials by username.
func (d *NoSQLDAO) GetCredential(ctx context.Context, username string) (*models.UserCredential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	uname := strings.TrimSpace(username)
	if uname == "" {
		return nil, ErrUserNotFound
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	if d.closed {
		return nil, errors.New("nosql dao is closed")
	}

	userID, exists := d.byUsername[uname]
	if !exists {
		return nil, ErrUserNotFound
	}

	doc, exists := d.docs[userID]
	if !exists {
		return nil, ErrUserNotFound
	}

	cred := doc.Credential
	return &cred, nil
}

// VerifyUserCredential validates the provided username and password, returning the UserProfile on success.
func (d *NoSQLDAO) VerifyUserCredential(ctx context.Context, username, password string) (*models.UserProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cred, err := d.GetCredential(ctx, username)
	if err != nil {
		return nil, err
	}

	if err := security.CheckPassword(cred.PasswordHash, password); err != nil {
		return nil, security.ErrInvalidPassword
	}

	return d.GetProfile(ctx, cred.UserID)
}

// Migrate ensures collections and secondary indexes are initialized.
func (d *NoSQLDAO) Migrate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return errors.New("nosql dao is closed")
	}

	d.indexed = true
	return nil
}

// MigrateUp executes pending migrations or index initialization for the document store.
func (d *NoSQLDAO) MigrateUp(ctx context.Context) (int, error) {
	if err := d.Migrate(ctx); err != nil {
		return 0, err
	}
	return 1, nil
}

// MigrateDown simulates rolling back migrations for the document store.
func (d *NoSQLDAO) MigrateDown(ctx context.Context, steps int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return 0, errors.New("nosql dao is closed")
	}
	return 0, nil
}

// MigrationVersion returns the current schema version of the document store.
func (d *NoSQLDAO) MigrationVersion(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return 0, errors.New("nosql dao is closed")
	}
	return 1, nil
}

// MigrationStatus returns status information for the NoSQL document collection.
func (d *NoSQLDAO) MigrationStatus(ctx context.Context) ([]MigrationStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, errors.New("nosql dao is closed")
	}
	now := time.Now().UTC()
	return []MigrationStatus{
		{
			Version:   1,
			Name:      "000001_nosql_document_store",
			Applied:   d.indexed,
			AppliedAt: &now,
		},
	}, nil
}

// Ping verifies the connectivity and health of the document store.
func (d *NoSQLDAO) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	if d.closed {
		return errors.New("nosql dao is closed")
	}
	return nil
}

// Close releases any resources associated with the document store.
func (d *NoSQLDAO) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.closed = true
	return nil
}

