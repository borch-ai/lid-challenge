package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/borch-ai/lid-challenge/internal/dao"
	"github.com/borch-ai/lid-challenge/internal/models"
	"github.com/borch-ai/lid-challenge/internal/security"
)

// HealthResponse represents liveness probe output.
type HealthResponse struct {
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{
		Status:    "healthy",
		Timestamp: time.Now().UTC(),
	})
}

// ReadyResponse represents readiness probe output including downstream connectivity.
type ReadyResponse struct {
	Status    string    `json:"status"`
	Database  string    `json:"database"`
	Timestamp time.Time `json:"timestamp"`
	Error     string    `json:"error,omitempty"`
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.dao == nil {
		writeJSON(w, http.StatusServiceUnavailable, ReadyResponse{
			Status:    "unready",
			Database:  "unreachable",
			Timestamp: time.Now().UTC(),
			Error:     "database not configured",
		})
		return
	}

	if err := s.dao.Ping(r.Context()); err != nil {
		s.logger.Error("readiness probe failed: database unreachable", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, ReadyResponse{
			Status:    "unready",
			Database:  "unreachable",
			Timestamp: time.Now().UTC(),
			Error:     "database unreachable",
		})
		return
	}

	writeJSON(w, http.StatusOK, ReadyResponse{
		Status:    "ready",
		Database:  "healthy",
		Timestamp: time.Now().UTC(),
	})
}

// CreateUserRequest represents user registration payload.
type CreateUserRequest struct {
	Name     string         `json:"name"`
	Phone    string         `json:"phone"`
	Address  models.Address `json:"address"`
	Username string         `json:"username"`
	Password string         `json:"password"`
}

// CreateUserResponse represents successful user creation output.
type CreateUserResponse struct {
	UserID  string              `json:"user_id"`
	Profile *models.UserProfile `json:"profile"`
}

// maxRequestBodyBytes limits incoming JSON bodies to 1 MB to prevent memory exhaustion attacks.
const maxRequestBodyBytes = 1 << 20

// dummyBcryptHash prevents user enumeration timing side channels when a user does not exist.
var dummyBcryptHash, _ = security.HashPassword("dummy-password-for-timing-mitigation")

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req CreateUserRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload or payload too large", "BAD_REQUEST")
		return
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "unexpected trailing data in request body", "BAD_REQUEST")
		return
	}

	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Phone) == "" {
		writeError(w, http.StatusBadRequest, "name and phone are required fields", "INVALID_FIELDS")
		return
	}
	if strings.TrimSpace(req.Address.StreetAddress) == "" ||
		strings.TrimSpace(req.Address.Locality) == "" ||
		strings.TrimSpace(req.Address.Region) == "" ||
		strings.TrimSpace(req.Address.PostalCode) == "" ||
		strings.TrimSpace(req.Address.Country) == "" {
		writeError(w, http.StatusBadRequest, "address fields (street_address, locality, region, postal_code, country) are required", "INVALID_FIELDS")
		return
	}
	if utf8.RuneCountInString(req.Address.Locality) > 128 ||
		utf8.RuneCountInString(req.Address.Region) > 128 ||
		utf8.RuneCountInString(req.Address.PostalCode) > 32 ||
		utf8.RuneCountInString(req.Address.Country) > 64 ||
		utf8.RuneCountInString(req.Address.StreetAddress) > 500 {
		writeError(w, http.StatusBadRequest, "address field exceeds maximum length (street_address: 500, locality: 128, region: 128, postal_code: 32, country: 64)", "INVALID_FIELDS")
		return
	}
	if strings.TrimSpace(req.Username) == "" || strings.TrimSpace(req.Password) == "" {
		writeError(w, http.StatusBadRequest, "username and password are required fields", "INVALID_FIELDS")
		return
	}
	if utf8.RuneCountInString(req.Username) > 128 {
		writeError(w, http.StatusBadRequest, "username exceeds maximum allowed length of 128 characters", "INVALID_FIELDS")
		return
	}
	if utf8.RuneCountInString(req.Name) > 255 {
		writeError(w, http.StatusBadRequest, "name exceeds maximum allowed length of 255 characters", "INVALID_FIELDS")
		return
	}
	if utf8.RuneCountInString(req.Phone) > 64 {
		writeError(w, http.StatusBadRequest, "phone exceeds maximum allowed length of 64 characters", "INVALID_FIELDS")
		return
	}
	if len(req.Password) > 72 {
		writeError(w, http.StatusBadRequest, "password exceeds maximum allowed length of 72 bytes", "PASSWORD_TOO_LONG")
		return
	}

	hashedPassword, err := security.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to process credentials", "INTERNAL_ERROR")
		return
	}

	profile := &models.UserProfile{
		Name:    req.Name,
		Phone:   req.Phone,
		Address: req.Address,
	}

	cred := &models.UserCredential{
		Username:     req.Username,
		Method:       security.DefaultHashMethod,
		PasswordHash: hashedPassword,
	}

	userID, err := s.dao.CreateUser(r.Context(), profile, cred)
	if err != nil {
		if errors.Is(err, dao.ErrUsernameTaken) {
			writeError(w, http.StatusConflict, "username already taken", "CONFLICT")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to create user", "INTERNAL_ERROR")
		return
	}

	profile.ID = userID
	writeJSON(w, http.StatusCreated, CreateUserResponse{
		UserID:  userID,
		Profile: profile,
	})
}

// LoginRequest represents credentials passed to authenticate.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResponse returns the authentication token and authenticated profile.
type LoginResponse struct {
	AccessToken string              `json:"access_token"`
	Profile     *models.UserProfile `json:"profile"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req LoginRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload or payload too large", "BAD_REQUEST")
		return
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "unexpected trailing data in request body", "BAD_REQUEST")
		return
	}

	if strings.TrimSpace(req.Username) == "" || strings.TrimSpace(req.Password) == "" {
		writeError(w, http.StatusBadRequest, "username and password are required", "INVALID_FIELDS")
		return
	}
	if len(req.Password) > 72 {
		writeError(w, http.StatusBadRequest, "password exceeds maximum allowed length of 72 bytes", "PASSWORD_TOO_LONG")
		return
	}

	profile, err := s.dao.VerifyUserCredential(r.Context(), req.Username, req.Password)
	if err != nil {
		if errors.Is(err, dao.ErrUserNotFound) {
			// Perform dummy bcrypt check to equalize response latency and prevent user enumeration
			_ = security.CheckPassword(dummyBcryptHash, req.Password)
			writeError(w, http.StatusUnauthorized, "invalid username or password", "INVALID_CREDENTIALS")
			return
		}
		if errors.Is(err, security.ErrInvalidPassword) {
			writeError(w, http.StatusUnauthorized, "invalid username or password", "INVALID_CREDENTIALS")
			return
		}
		writeError(w, http.StatusInternalServerError, "authentication error", "INTERNAL_ERROR")
		return
	}

	// Issue a signed per-user token with subject and 24-hour expiration
	secret := s.cfg.AuthSecret
	if secret == "" && s.cfg.DevAuthBypass {
		secret = "dev-secret-token"
	}
	token, err := security.GenerateUserToken(profile.ID, req.Username, secret, 24*time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to issue access token", "INTERNAL_ERROR")
		return
	}

	writeJSON(w, http.StatusOK, LoginResponse{
		AccessToken: token,
		Profile:     profile,
	})
}

func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		writeError(w, http.StatusBadRequest, "profile id is required", "INVALID_ID")
		return
	}

	// Enforce user token subject boundary: a user token may only access the owner's profile.
	// Cross-user profile access requires master service/admin authorization.
	if claims, ok := r.Context().Value(UserClaimsKey).(*security.TokenClaims); ok && claims != nil {
		if claims.UserID != id {
			writeError(w, http.StatusForbidden, "access forbidden: cannot access other users' profile", "FORBIDDEN")
			return
		}
	}

	profile, err := s.dao.GetProfile(r.Context(), id)
	if err != nil {
		if errors.Is(err, dao.ErrUserNotFound) {
			writeError(w, http.StatusNotFound, "user profile not found", "NOT_FOUND")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to retrieve profile", "INTERNAL_ERROR")
		return
	}

	writeJSON(w, http.StatusOK, profile)
}

// SearchResponse wraps paginated profile search results.
type SearchResponse struct {
	Data   []*models.UserProfile `json:"data"`
	Count  int                   `json:"count"`
	Limit  int                   `json:"limit"`
	Offset int                   `json:"offset"`
}

func (s *Server) handleSearchProfiles(w http.ResponseWriter, r *http.Request) {
	// Restrict multi-user directory search to master service/admin authorization.
	// Signed user tokens only authorize self-profile retrieval to prevent cross-user PII enumeration.
	if claims, ok := r.Context().Value(UserClaimsKey).(*security.TokenClaims); ok && claims != nil {
		writeError(w, http.StatusForbidden, "access forbidden: profile directory search requires administrative credentials", "FORBIDDEN")
		return
	}
	q := r.URL.Query()

	limit := 20
	if l := q.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil {
			if parsed > 100 {
				limit = 100
			} else if parsed < 1 {
				limit = 1
			} else {
				limit = parsed
			}
		}
	}

	offset := 0
	if o := q.Get("offset"); o != "" {
		if parsed, err := strconv.Atoi(o); err == nil && parsed >= 0 {
			if parsed > 10000 {
				offset = 10000
			} else {
				offset = parsed
			}
		}
	}

	query := models.SearchQuery{
		Name:     q.Get("name"),
		Phone:    q.Get("phone"),
		Locality: q.Get("locality"),
		Region:   q.Get("region"),
		Country:  q.Get("country"),
		Limit:    limit,
		Offset:   offset,
	}

	profiles, err := s.dao.SearchProfiles(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to search profiles", "INTERNAL_ERROR")
		return
	}

	if profiles == nil {
		profiles = []*models.UserProfile{}
	}

	writeJSON(w, http.StatusOK, SearchResponse{
		Data:   profiles,
		Count:  len(profiles),
		Limit:  limit,
		Offset: offset,
	})
}
