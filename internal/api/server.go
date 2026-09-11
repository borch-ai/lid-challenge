package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/borch-ai/lid-challenge/internal/dao"
)

// Config configures the RESTful API server.
type Config struct {
	Port           int      `json:"port"`
	AuthSecret     string   `json:"auth_secret"`
	RateLimitRPS   float64  `json:"rate_limit_rps"`
	RateLimitBurst float64  `json:"rate_limit_burst"`
	TrustedProxies []string `json:"trusted_proxies"`
	DevAuthBypass  bool     `json:"dev_auth_bypass"`
}

// Server coordinates HTTP routing, middleware, and dependency injection for the API.
type Server struct {
	dao           dao.UserDAO
	cfg           Config
	mux           *http.ServeMux
	limiter       *RateLimiter
	logger        *slog.Logger
	trustedIPNets []*net.IPNet
}

// NewServer initializes a new Server with injected dependencies.
func NewServer(userDAO dao.UserDAO, cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}

	var limiter *RateLimiter
	if cfg.RateLimitRPS > 0 {
		burst := cfg.RateLimitBurst
		if burst <= 0 {
			burst = cfg.RateLimitRPS * 2
		}
		limiter = NewRateLimiter(cfg.RateLimitRPS, burst)
	}

	var trustedIPNets []*net.IPNet
	for _, raw := range cfg.TrustedProxies {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		cidr := trimmed
		if !strings.Contains(trimmed, "/") {
			ip := net.ParseIP(trimmed)
			if ip == nil {
				return nil, fmt.Errorf("invalid trusted proxy IP %q", raw)
			}
			if ip.To4() != nil {
				cidr = trimmed + "/32"
			} else {
				cidr = trimmed + "/128"
			}
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil || ipNet == nil {
			return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", raw, err)
		}
		trustedIPNets = append(trustedIPNets, ipNet)
	}

	s := &Server{
		dao:           userDAO,
		cfg:           cfg,
		mux:           http.NewServeMux(),
		limiter:       limiter,
		logger:        logger,
		trustedIPNets: trustedIPNets,
	}

	s.routes()
	return s, nil
}

// routes configures the Go 1.22+ ServeMux routing tree.
func (s *Server) routes() {
	// Public routes
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/ready", s.handleReady)
	s.mux.HandleFunc("GET /api/v1/openapi.yaml", s.handleOpenAPI)
	s.mux.HandleFunc("POST /api/v1/users", s.handleCreateUser)
	s.mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)

	// Protected routes (require Bearer token authentication)
	s.mux.Handle("GET /api/v1/profiles/{id}", s.WithAuth(http.HandlerFunc(s.handleGetProfile)))
	s.mux.Handle("GET /api/v1/profiles", s.WithAuth(http.HandlerFunc(s.handleSearchProfiles)))
}

// Handler returns the fully wrapped HTTP handler chain with top-level middleware.
func (s *Server) Handler() http.Handler {
	var handler http.Handler = s.mux
	handler = WithContextTimeout(10 * time.Second)(handler)
	handler = s.WithRateLimit(handler)
	handler = s.WithRecovery(handler)
	handler = s.WithLogging(handler)
	return handler
}

// writeJSON writes a JSON response with status code and Content-Type header.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// ErrorResponse represents standard error payload format.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// writeError writes a standardized JSON error message.
func writeError(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, ErrorResponse{
		Error: message,
		Code:  code,
	})
}
