// Package dao defines data access object interfaces and persistence abstractions.
package dao

import (
	"context"
	"errors"

	"github.com/borch-ai/lid-challenge/internal/models"
)

var (
	// ErrUserNotFound indicates that the requested user does not exist.
	ErrUserNotFound = errors.New("user not found")
	// ErrUsernameTaken indicates that a user with the provided username already exists.
	ErrUsernameTaken = errors.New("username already taken")
	// ErrInvalidInput indicates that mandatory fields are missing or malformed.
	ErrInvalidInput = errors.New("invalid input")
)

// UserDAO defines the data access contract for storing and retrieving
// user profiles and user credentials across relational database engines.
type UserDAO interface {
	// CreateUser persists a user profile and credentials atomically.
	CreateUser(ctx context.Context, profile *models.UserProfile, cred *models.UserCredential) (string, error)

	// GetProfile retrieves a user profile by unique user ID.
	GetProfile(ctx context.Context, userID string) (*models.UserProfile, error)

	// SearchProfiles finds user profiles matching the search criteria.
	SearchProfiles(ctx context.Context, query models.SearchQuery) ([]*models.UserProfile, error)

	// GetCredential retrieves user credential details by username.
	GetCredential(ctx context.Context, username string) (*models.UserCredential, error)

	// VerifyUserCredential validates the provided username and password,
	// returning the associated UserProfile upon successful authentication.
	VerifyUserCredential(ctx context.Context, username, password string) (*models.UserProfile, error)

	// Migrate executes dialect-specific DDL to initialize database tables and indexes.
	Migrate(ctx context.Context) error

	// Ping verifies connectivity to the underlying database engine.
	Ping(ctx context.Context) error

	// Close releases any database connections and resources.
	Close() error
}
