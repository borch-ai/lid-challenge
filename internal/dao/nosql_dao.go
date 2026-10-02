// Package dao defines data access object interfaces and persistence abstractions.
package dao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/borch-ai/lid-challenge/internal/models"
	"github.com/borch-ai/lid-challenge/internal/security"
	"github.com/google/uuid"
)

// storageCredential persists user credentials including the hashed password on disk.
// This is distinct from models.UserCredential which omits PasswordHash from JSON serialization.
type storageCredential struct {
	UserID       string    `json:"user_id"`
	Username     string    `json:"username"`
	Method       string    `json:"method"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (s storageCredential) toModel() models.UserCredential {
	return models.UserCredential{
		UserID:       s.UserID,
		Username:     s.Username,
		Method:       s.Method,
		PasswordHash: s.PasswordHash,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
}

func fromModelCredential(c models.UserCredential) storageCredential {
	return storageCredential{
		UserID:       c.UserID,
		Username:     c.Username,
		Method:       c.Method,
		PasswordHash: c.PasswordHash,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
}

// userDocument represents the unified NoSQL document structure storing
// both user profile attributes and credentials in a single document pattern.
type userDocument struct {
	ID         string             `json:"id"`
	Profile    models.UserProfile `json:"profile"`
	Credential storageCredential  `json:"credential"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

// nosqlFilePayload represents the on-disk JSON file structure containing
// version metadata and the collection of user documents.
type nosqlFilePayload struct {
	Version   int64                    `json:"version"`
	AppliedAt *time.Time               `json:"applied_at,omitempty"`
	Documents map[string]*userDocument `json:"documents"`
}

func validateDocuments(filePath string, docs map[string]*userDocument) error {
	seenUsernames := make(map[string]string, len(docs))
	for id, doc := range docs {
		if doc == nil {
			return fmt.Errorf("document store file %q contains null document for user ID %q", filePath, id)
		}
		if doc.ID == "" {
			doc.ID = id
		} else if doc.ID != id {
			return fmt.Errorf("document store file %q contains mismatched document ID %q for map key %q", filePath, doc.ID, id)
		}
		if doc.Profile.ID == "" {
			doc.Profile.ID = id
		} else if doc.Profile.ID != id {
			return fmt.Errorf("document store file %q contains mismatched profile ID %q for map key %q", filePath, doc.Profile.ID, id)
		}
		if doc.Credential.UserID == "" {
			doc.Credential.UserID = id
		} else if doc.Credential.UserID != id {
			return fmt.Errorf("document store file %q contains mismatched credential user ID %q for map key %q", filePath, doc.Credential.UserID, id)
		}
		if doc.Credential.Username != "" {
			if existingID, exists := seenUsernames[doc.Credential.Username]; exists && existingID != id {
				return fmt.Errorf("document store file %q contains duplicate username %q for user IDs %q and %q", filePath, doc.Credential.Username, existingID, id)
			}
			seenUsernames[doc.Credential.Username] = id
		}
	}
	return nil
}

func parseDocumentStoreFile(filePath string, data []byte) (map[string]*userDocument, int64, *time.Time, error) {
	if len(data) == 0 {
		return make(map[string]*userDocument), 0, nil, nil
	}

	// 1. Try structured payload containing version metadata and documents map
	var payload nosqlFilePayload
	if err := json.Unmarshal(data, &payload); err == nil && payload.Documents != nil {
		if err := validateDocuments(filePath, payload.Documents); err != nil {
			return nil, 0, nil, err
		}
		return payload.Documents, payload.Version, payload.AppliedAt, nil
	}

	// 2. Fallback to raw document map format
	var rawDocs map[string]*userDocument
	if err := json.Unmarshal(data, &rawDocs); err != nil {
		return nil, 0, nil, fmt.Errorf("failed to unmarshal document store from %q: %w", filePath, err)
	}
	if rawDocs == nil {
		return nil, 0, nil, fmt.Errorf("document store file %q decoded to null, expected document map", filePath)
	}
	if err := validateDocuments(filePath, rawDocs); err != nil {
		return nil, 0, nil, err
	}
	return rawDocs, 0, nil, nil
}

// NoSQLDAO implements UserDAO using a document store pattern supporting both
// in-memory mode (for tests and ephemeral workloads) and durable JSON file backing
// with secondary index management and atomic single-document write semantics.
type NoSQLDAO struct {
	mu         sync.RWMutex
	filePath   string                   // Non-empty when durable file persistence is configured
	docs       map[string]*userDocument // Primary index by user ID
	byUsername map[string]string        // Unique secondary index: username -> user ID
	closed     bool
	migrated   bool
	appliedAt  *time.Time
}

// NewNoSQLDAO creates a new NoSQLDAO document store instance.
// If dsn is empty, "memory", ":memory:", or starts with "memory://", data is held in-memory.
// Otherwise, dsn is treated as a file path (or file:// URI) for durable document persistence.
// Note: File-backed NoSQL mode is designed for single-process embedded deployments, local
// development, and automated testing. For multi-replica production deployments requiring
// distributed concurrent writes, configure a networked datastore (PostgreSQL/CockroachDB).
func NewNoSQLDAO(dsn string) (*NoSQLDAO, error) {
	trimmed := strings.TrimSpace(dsn)
	var filePath string
	if trimmed != "" && trimmed != "memory" && trimmed != ":memory:" && trimmed != "memory://" {
		if strings.HasPrefix(trimmed, "memory://") {
			return nil, fmt.Errorf("invalid memory DSN %q: expected 'memory', ':memory:', or 'memory://'", dsn)
		}
		if strings.HasPrefix(trimmed, "file://") {
			filePath = strings.TrimPrefix(trimmed, "file://")
			filePath = strings.TrimSpace(filePath)
			if filePath == "" || filePath == "/" {
				return nil, fmt.Errorf("invalid file DSN %q: empty file path", dsn)
			}
		} else if strings.Contains(trimmed, "://") {
			return nil, fmt.Errorf("unsupported DSN scheme in %q: only 'file://' and 'memory://' are supported for NoSQL", dsn)
		} else {
			filePath = trimmed
		}
	}

	dao := &NoSQLDAO{
		filePath:   filePath,
		docs:       make(map[string]*userDocument),
		byUsername: make(map[string]string),
		migrated:   false,
	}

	if filePath != "" {
		cleanPath := filepath.Clean(filePath)
		// #nosec G304 -- administrative datastore file path configured via DSN
		data, err := os.ReadFile(cleanPath)
		if err == nil && len(data) > 0 {
			loaded, ver, appliedAt, err := parseDocumentStoreFile(cleanPath, data)
			if err != nil {
				return nil, err
			}
			if ver < 0 || ver > 1 {
				return nil, fmt.Errorf("unsupported document store schema version %d (only versions 0 and 1 are supported)", ver)
			}
			dao.docs = loaded
			if ver == 1 {
				dao.migrated = true
				dao.appliedAt = appliedAt
			}
			for id, doc := range loaded {
				if doc != nil && doc.Credential.Username != "" {
					if existingID, exists := dao.byUsername[doc.Credential.Username]; exists && existingID != id {
						return nil, fmt.Errorf("document store file %q contains duplicate username %q for user IDs %q and %q", filePath, doc.Credential.Username, existingID, id)
					}
					dao.byUsername[doc.Credential.Username] = id
				}
			}
		} else if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to read document store file %q: %w", filePath, err)
		}
	}

	return dao, nil
}

func (d *NoSQLDAO) mergeFromDiskLocked(cleanPath string, targetDocs map[string]*userDocument, targetByUsername map[string]string, isRollback bool, newlyCreatedID string) error {
	if isRollback {
		// Rolling back tears down the schema and drops all documents/indexes,
		// matching the behavior of SQL migrations dropping user_profile and user_credential tables.
		return nil
	}
	// #nosec G304 -- administrative datastore file path configured via DSN
	diskData, err := os.ReadFile(cleanPath)
	if err == nil && len(diskData) > 0 {
		diskDocs, diskVer, diskAppliedAt, err := parseDocumentStoreFile(cleanPath, diskData)
		if err != nil {
			return fmt.Errorf("failed to reload document store file %q: %w", cleanPath, err)
		}
		if diskVer < 0 || diskVer > 1 {
			return fmt.Errorf("unsupported document store schema version %d (only versions 0 and 1 are supported)", diskVer)
		}
		if newlyCreatedID != "" {
			if _, exists := diskDocs[newlyCreatedID]; exists {
				return fmt.Errorf("user with ID %q already exists on disk: %w", newlyCreatedID, ErrInvalidInput)
			}
		}
		for id, diskDoc := range diskDocs {
			existing, exists := targetDocs[id]
			if !exists || diskDoc.UpdatedAt.After(existing.UpdatedAt) {
				if exists && existing.Credential.Username != "" && existing.Credential.Username != diskDoc.Credential.Username {
					delete(targetByUsername, existing.Credential.Username)
				}
				if diskDoc.Credential.Username != "" {
					if existingID, ok := targetByUsername[diskDoc.Credential.Username]; ok && existingID != id {
						return fmt.Errorf("reload document store file %q detected duplicate username %q for user IDs %q and %q", cleanPath, diskDoc.Credential.Username, existingID, id)
					}
					targetByUsername[diskDoc.Credential.Username] = id
				}
				targetDocs[id] = diskDoc
			}
		}
		if diskVer == 1 && !d.migrated {
			d.migrated = true
			d.appliedAt = diskAppliedAt
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read document store file %q: %w", cleanPath, err)
	}
	return nil
}

func (d *NoSQLDAO) persistLocked(isRollback bool, newlyCreatedIDs ...string) error {
	if d.filePath == "" {
		return nil
	}

	cleanPath := filepath.Clean(d.filePath)

	// Build candidate document map and candidate username index first to ensure atomic merge and validation.
	candidateDocs := make(map[string]*userDocument, len(d.docs))
	for k, v := range d.docs {
		candidateDocs[k] = v
	}
	candidateByUsername := make(map[string]string, len(d.byUsername))
	for k, v := range d.byUsername {
		candidateByUsername[k] = v
	}

	var newID string
	if len(newlyCreatedIDs) > 0 {
		newID = newlyCreatedIDs[0]
	}

	// Reload and merge any existing documents on disk to prevent lost writes
	// across processes or separate instances accessing the same file.
	if err := d.mergeFromDiskLocked(cleanPath, candidateDocs, candidateByUsername, isRollback, newID); err != nil {
		return err
	}

	var version int64
	if d.migrated {
		version = 1
	}
	payload := nosqlFilePayload{
		Version:   version,
		AppliedAt: d.appliedAt,
		Documents: candidateDocs,
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal document store: %w", err)
	}

	dir := filepath.Dir(cleanPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("failed to create directory %q: %w", dir, err)
		}
	}

	// Use PID and nanosecond timestamp to eliminate race conditions on temporary files
	tmpFile := fmt.Sprintf("%s.%d.%d.tmp", cleanPath, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return fmt.Errorf("failed to write temporary document store file: %w", err)
	}

	if err := os.Rename(tmpFile, cleanPath); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed to persist document store: %w", err)
	}

	// Commit candidate maps only after successful validation, merge, and disk serialization
	d.docs = candidateDocs
	d.byUsername = candidateByUsername

	return nil
}

// CreateUser persists a user profile and credentials atomically within a single document.
func (d *NoSQLDAO) CreateUser(ctx context.Context, profile *models.UserProfile, cred *models.UserCredential) (string, error) {
	if profile == nil || cred == nil {
		return "", ErrInvalidInput
	}
	if strings.TrimSpace(profile.Name) == "" || strings.TrimSpace(profile.Phone) == "" {
		return "", fmt.Errorf("%w: name and phone are required", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return "", errors.New("nosql dao is closed")
	}

	// For file-backed DAO, reload disk state to incorporate documents and usernames persisted by other instances.
	if d.filePath != "" {
		cleanPath := filepath.Clean(d.filePath)
		candidateDocs := make(map[string]*userDocument, len(d.docs))
		for k, v := range d.docs {
			candidateDocs[k] = v
		}
		candidateByUsername := make(map[string]string, len(d.byUsername))
		for k, v := range d.byUsername {
			candidateByUsername[k] = v
		}
		if err := d.mergeFromDiskLocked(cleanPath, candidateDocs, candidateByUsername, false, ""); err != nil {
			return "", err
		}
		d.docs = candidateDocs
		d.byUsername = candidateByUsername
	}

	if !d.migrated {
		return "", errors.New("nosql datastore schema is not migrated: collection unavailable")
	}

	if strings.TrimSpace(cred.Username) == "" || strings.TrimSpace(cred.PasswordHash) == "" {
		return "", fmt.Errorf("%w: username and password_hash are required", ErrInvalidInput)
	}

	// Enforce unique secondary index on username (preserving original string without trimming)
	if _, exists := d.byUsername[cred.Username]; exists {
		return "", ErrUsernameTaken
	}

	id := profile.ID
	if id != "" {
		// Reject duplicate caller-supplied IDs to prevent corrupting indices
		if _, exists := d.docs[id]; exists {
			return "", fmt.Errorf("user with ID %q already exists: %w", id, ErrInvalidInput)
		}
	} else {
		id = uuid.New().String()
	}
	now := time.Now().UTC()

	profCopy := *profile
	profCopy.ID = id
	if profCopy.CreatedAt.IsZero() {
		profCopy.CreatedAt = now
	}
	profCopy.UpdatedAt = now

	credCopy := *cred
	credCopy.UserID = id
	if credCopy.CreatedAt.IsZero() {
		credCopy.CreatedAt = now
	}
	credCopy.UpdatedAt = now

	if credCopy.Method == "" {
		credCopy.Method = security.DefaultHashMethod
	} else if credCopy.Method != security.DefaultHashMethod {
		return "", fmt.Errorf("%w: unsupported credential hash method %q", ErrInvalidInput, cred.Method)
	}

	doc := &userDocument{
		ID:         id,
		Profile:    profCopy,
		Credential: fromModelCredential(credCopy),
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	d.docs[id] = doc
	d.byUsername[cred.Username] = id

	if err := d.persistLocked(false, id); err != nil {
		delete(d.docs, id)
		delete(d.byUsername, cred.Username)
		return "", err
	}

	*profile = profCopy
	*cred = credCopy

	return id, nil
}

// GetProfile retrieves a user profile by unique user ID.
func (d *NoSQLDAO) GetProfile(ctx context.Context, userID string) (*models.UserProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrInvalidInput
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

// DeleteUser removes a user profile and associated credentials atomically.
func (d *NoSQLDAO) DeleteUser(ctx context.Context, userID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(userID) == "" {
		return ErrInvalidInput
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return errors.New("nosql dao is closed")
	}

	doc, exists := d.docs[userID]
	if !exists {
		return ErrUserNotFound
	}

	username := doc.Credential.Username
	delete(d.docs, userID)
	if username != "" {
		delete(d.byUsername, username)
	}

	if err := d.persistLocked(false); err != nil {
		d.docs[userID] = doc
		if username != "" {
			d.byUsername[username] = userID
		}
		return err
	}
	return nil
}

// SearchProfiles finds user profiles matching search criteria with pagination.
// Semantics align with the relational SQL DAO: substring for Name/Phone, exact case-insensitive for Locality/Region/Country.
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

	qName := strings.ToLower(query.Name)
	qPhone := query.Phone
	qLocality := query.Locality
	qRegion := query.Region
	qCountry := query.Country

	for _, doc := range d.docs {
		p := doc.Profile
		// Substring matching for Name and Phone (case-insensitive for name)
		if qName != "" && !strings.Contains(strings.ToLower(p.Name), qName) {
			continue
		}
		if qPhone != "" && !strings.Contains(p.Phone, qPhone) {
			continue
		}
		// Exact case-insensitive matching preserving whitespace for Locality, Region, and Country (matching SQL semantics)
		if qLocality != "" && !strings.EqualFold(p.Address.Locality, qLocality) {
			continue
		}
		if qRegion != "" && !strings.EqualFold(p.Address.Region, qRegion) {
			continue
		}
		if qCountry != "" && !strings.EqualFold(p.Address.Country, qCountry) {
			continue
		}
		profCopy := p
		matched = append(matched, &profCopy)
	}

	// Sort deterministically by CreatedAt DESC, then ID DESC (matching SQL DAO)
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].ID > matched[j].ID
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
	} else if offset > 10000 {
		offset = 10000
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
	if strings.TrimSpace(username) == "" {
		return nil, ErrInvalidInput
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	if d.closed {
		return nil, errors.New("nosql dao is closed")
	}

	userID, exists := d.byUsername[username]
	if !exists {
		return nil, ErrUserNotFound
	}

	doc, exists := d.docs[userID]
	if !exists {
		return nil, ErrUserNotFound
	}

	cred := doc.Credential.toModel()
	if cred.UserID == "" {
		cred.UserID = doc.ID
	}
	return &cred, nil
}

// VerifyUserCredential validates the provided username and password, returning the UserProfile on success.
func (d *NoSQLDAO) VerifyUserCredential(ctx context.Context, username, password string) (*models.UserProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
		return nil, ErrInvalidInput
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

	if !d.migrated {
		d.migrated = true
		now := time.Now().UTC()
		d.appliedAt = &now
		if err := d.persistLocked(false); err != nil {
			d.migrated = false
			d.appliedAt = nil
			return err
		}
	}
	return nil
}

// MigrateUp executes pending migrations or index initialization for the document store.
// Tracks migration state transitions and returns 0 when already applied.
func (d *NoSQLDAO) MigrateUp(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return 0, errors.New("nosql dao is closed")
	}

	if d.migrated {
		return 0, nil
	}

	d.migrated = true
	now := time.Now().UTC()
	d.appliedAt = &now
	if err := d.persistLocked(false); err != nil {
		d.migrated = false
		d.appliedAt = nil
		return 0, err
	}
	return 1, nil
}

// MigrateDown simulates rolling back migrations for the document store.
func (d *NoSQLDAO) MigrateDown(ctx context.Context, steps int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return 0, errors.New("nosql dao is closed")
	}

	if !d.migrated || steps < 1 {
		return 0, nil
	}

	d.migrated = false
	prevAppliedAt := d.appliedAt
	d.appliedAt = nil
	prevDocs := d.docs
	prevByUsername := d.byUsername
	d.docs = make(map[string]*userDocument)
	d.byUsername = make(map[string]string)
	if err := d.persistLocked(true); err != nil {
		d.migrated = true
		d.appliedAt = prevAppliedAt
		d.docs = prevDocs
		d.byUsername = prevByUsername
		return 0, err
	}
	return 1, nil
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

	if d.migrated {
		return 1, nil
	}
	return 0, nil
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

	var appliedAt *time.Time
	if d.migrated {
		if d.appliedAt != nil {
			tCopy := *d.appliedAt
			appliedAt = &tCopy
		} else {
			now := time.Now().UTC()
			appliedAt = &now
		}
	}

	return []MigrationStatus{
		{
			Version:   1,
			Name:      "000001_nosql_document_store",
			Applied:   d.migrated,
			AppliedAt: appliedAt,
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

	if d.filePath != "" {
		cleanPath := filepath.Clean(d.filePath)
		if info, err := os.Stat(cleanPath); err == nil {
			if info.IsDir() {
				return fmt.Errorf("configured document store path %q is a directory, expected a file", cleanPath)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("document store file %q inaccessible: %w", cleanPath, err)
		} else {
			dir := filepath.Dir(cleanPath)
			if _, err := os.Stat(dir); err != nil {
				return fmt.Errorf("document store directory %q inaccessible: %w", dir, err)
			}
		}
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
