package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/borch-ai/lid-challenge/internal/security"
)

// WithRecovery recovers from panics and returns a 500 Internal Server Error JSON response.
func (s *Server) WithRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic recovered in HTTP handler",
					slog.Any("panic", rec),
					slog.String("path", r.URL.Path),
					slog.String("method", r.Method),
				)
				writeError(w, http.StatusInternalServerError, "internal server error", "INTERNAL_ERROR")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// responseWriterInterceptor captures the HTTP status code for structured logging.
type responseWriterInterceptor struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (rw *responseWriterInterceptor) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.statusCode = code
		rw.wroteHeader = true
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriterInterceptor) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.statusCode = http.StatusOK
		rw.wroteHeader = true
	}
	return rw.ResponseWriter.Write(b)
}

// WithLogging produces structured logs for all incoming HTTP requests using log/slog.
func (s *Server) WithLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriterInterceptor{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(rw, r)

		duration := time.Since(start)
		s.logger.Info("http request completed",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rw.statusCode),
			slog.Duration("duration", duration),
			slog.String("remote_addr", r.RemoteAddr),
		)
	})
}

type contextKey string

const (
	// UserClaimsKey is the context key under which authenticated user claims are stored.
	UserClaimsKey contextKey = "user_claims"
)

// WithAuth enforces Bearer token authentication against the configured AuthSecret.
// It accepts either a signed per-user Bearer token (with subject and expiration)
// or the master service/admin AuthSecret.
// If DevAuthBypass is explicitly enabled, dev mode bypass is permitted. Otherwise,
// the middleware fails closed and rejects unauthenticated requests.
func (s *Server) WithAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.DevAuthBypass {
			// Explicit dev mode bypass: accept all requests
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		token := security.ExtractBearerToken(authHeader)
		if token == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "missing or malformed authorization header", "UNAUTHORIZED")
			return
		}

		// 1. Check if token is a valid signed user token
		claims, err := security.VerifyUserToken(token, s.cfg.AuthSecret)
		if err == nil && claims != nil {
			ctx := context.WithValue(r.Context(), UserClaimsKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// 2. Fallback: check if token matches master service AuthSecret in constant time
		if security.VerifyToken(token, s.cfg.AuthSecret) {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeError(w, http.StatusUnauthorized, "invalid authorization token", "UNAUTHORIZED")
	})
}

// clientLimiter tracks rate limit state per remote IP with embedded LRU links.
type clientLimiter struct {
	ip         string
	tokens     float64
	lastUpdate time.Time
	prev       *clientLimiter
	next       *clientLimiter
}

// RateLimiter implements an in-memory token bucket rate limiter per client IP with O(1) LRU bounded eviction.
type RateLimiter struct {
	mu          sync.Mutex
	rate        float64 // tokens per second
	burst       float64 // max bucket capacity
	clients     map[string]*clientLimiter
	head        *clientLimiter // most recently active
	tail        *clientLimiter // least recently active
	lastCleanup time.Time
}

const maxClients = 10000

// NewRateLimiter creates a token bucket rate limiter with automatic idle eviction.
func NewRateLimiter(rate float64, burst float64) *RateLimiter {
	if burst < 1 {
		burst = 1
	}
	rl := &RateLimiter{
		rate:        rate,
		burst:       burst,
		clients:     make(map[string]*clientLimiter),
		lastCleanup: time.Now(),
	}
	return rl
}

func (rl *RateLimiter) removeNode(node *clientLimiter) {
	if node == nil {
		return
	}
	if node.prev != nil {
		node.prev.next = node.next
	} else if rl.head == node {
		rl.head = node.next
	}
	if node.next != nil {
		node.next.prev = node.prev
	} else if rl.tail == node {
		rl.tail = node.prev
	}
	node.prev = nil
	node.next = nil
}

func (rl *RateLimiter) moveToFront(node *clientLimiter) {
	if node == nil || rl.head == node {
		return
	}
	rl.removeNode(node)
	node.next = rl.head
	node.prev = nil
	if rl.head != nil {
		rl.head.prev = node
	}
	rl.head = node
	if rl.tail == nil {
		rl.tail = node
	}
}

func (rl *RateLimiter) pushFront(node *clientLimiter) {
	node.next = rl.head
	node.prev = nil
	if rl.head != nil {
		rl.head.prev = node
	}
	rl.head = node
	if rl.tail == nil {
		rl.tail = node
	}
}

// Allow checks if the given IP is allowed to execute a request.
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	// Evict idle clients older than 5 minutes to prevent unbounded memory growth
	if now.Sub(rl.lastCleanup) > time.Minute {
		rl.lastCleanup = now
		for clientIP, l := range rl.clients {
			if now.Sub(l.lastUpdate) > 5*time.Minute {
				rl.removeNode(l)
				delete(rl.clients, clientIP)
			}
		}
	}

	limiter, exists := rl.clients[ip]
	if !exists {
		// Bounded capacity: if map reaches maxClients, evict the least-recently-active entry in O(1).
		if len(rl.clients) >= maxClients {
			var oldestIP string
			if rl.tail != nil {
				oldestIP = rl.tail.ip
				rl.removeNode(rl.tail)
			} else {
				// Fallback if seeded directly into map in unit tests
				var oldestTime time.Time
				first := true
				for k, v := range rl.clients {
					if first || v.lastUpdate.Before(oldestTime) {
						oldestIP = k
						oldestTime = v.lastUpdate
						first = false
					}
				}
			}
			if oldestIP != "" {
				delete(rl.clients, oldestIP)
			}
		}
		limiter = &clientLimiter{
			ip:         ip,
			tokens:     rl.burst - 1, // First request consumes 1 token
			lastUpdate: now,
		}
		rl.pushFront(limiter)
		rl.clients[ip] = limiter
		return true
	}

	rl.moveToFront(limiter)

	// Refill tokens based on elapsed time
	elapsed := now.Sub(limiter.lastUpdate).Seconds()
	limiter.lastUpdate = now
	limiter.tokens += elapsed * rl.rate
	if limiter.tokens > rl.burst {
		limiter.tokens = rl.burst
	}

	if limiter.tokens >= 1 {
		limiter.tokens--
		return true
	}

	return false
}

// isTrustedIP reports whether the given IP falls within any configured trusted proxy network.
func (s *Server) isTrustedIP(ip net.IP) bool {
	for _, netBlock := range s.trustedIPNets {
		if netBlock.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP extracts the client IP address. When trusted proxies are configured,
// it validates that the immediate peer (RemoteAddr) belongs to a trusted proxy network
// before honoring X-Forwarded-For or X-Real-IP headers.
// The forwarded chain is traversed right-to-left to select the first validated untrusted IP,
// preventing client spoofing via prepended header values.
func (s *Server) clientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	} else if colonIdx := strings.LastIndex(ip, ":"); colonIdx != -1 {
		ip = ip[:colonIdx]
	}

	if len(s.trustedIPNets) > 0 {
		peerIP := net.ParseIP(ip)
		if peerIP != nil && s.isTrustedIP(peerIP) {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				parts := strings.Split(xff, ",")
				for i := len(parts) - 1; i >= 0; i-- {
					part := strings.TrimSpace(parts[i])
					if part == "" {
						continue
					}
					parsed := net.ParseIP(part)
					if parsed != nil && !s.isTrustedIP(parsed) {
						return parsed.String()
					}
				}
			}
			if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
				if parsed := net.ParseIP(xri); parsed != nil {
					return parsed.String()
				}
			}
		}
	}
	return ip
}

// WithRateLimit wraps a handler to limit requests per IP address.
// Lightweight liveness probe (/api/v1/health) is exempt from throttling so orchestrator probes remain uninterrupted.
// Note: /api/v1/ready performs a database ping and remains subject to rate limiting to prevent database connection exhaustion.
func (s *Server) WithRateLimit(next http.Handler) http.Handler {
	if s.limiter == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exempt lightweight liveness health probe from throttling
		if r.URL.Path == "/api/v1/health" {
			next.ServeHTTP(w, r)
			return
		}

		ip := s.clientIP(r)

		if !s.limiter.Allow(ip) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded", "RATE_LIMIT_EXCEEDED")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// WithContextTimeout ensures downstream handlers do not exceed the timeout limit.
func WithContextTimeout(timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
