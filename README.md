# LID Challenge: Identity & User Profile Service

A production-grade, idiomatic Go implementation addressing all three questions of the LID interview project challenge:
1. **Multi-Database User Profile & Credential DAO** (PostgreSQL, CockroachDB, SQLite).
2. **RESTful API Web Service** with parameterized profile search and Bearer token security authentication.
3. **3rd-Party Identity Provider Connector** (ABC / XYZ) with `/auth` and `/identity` integration, token caching, concurrency de-duplication, and PII log redaction.

> 📘 **Looking for how we engineered this solution?** Read our engineering retrospective and AI-augmented methodology in [`HOWTO.md`](HOWTO.md).

---

## 🏛️ Architectural Overview & Design Decisions

### 1. Multi-Database DAO (`internal/dao`)
* **Dual Wire & Dialect Support**:
  * **SQLite**: Uses the pure-Go `modernc.org/sqlite` driver (zero CGO dependencies), enabling instant in-memory (`:memory:`) test execution without requiring Docker or C compilers.
  * **PostgreSQL & CockroachDB**: CockroachDB natively implements the PostgreSQL wire protocol and SQL semantics. The `PostgresDialect` supports both systems transparently using positional parameters (`$1, $2, ...`) via `github.com/lib/pq`.
* **Atomic Transactions**: User profile and credential records are inserted together inside an isolated database transaction (`BeginTx` / `Commit` / `Rollback`), guaranteeing data consistency and preventing orphaned profiles if credential hashing or validation fails.
* **Dialect Abstraction**: A `Dialect` interface cleanly handles parameter re-binding (`?` to `$1`), table creation DDL, and index provisioning without polluting business logic with SQL string concatenation.
* **Security-First Credentials**: Passwords are never stored in plaintext. Passwords are salted and hashed using `golang.org/x/crypto/bcrypt` under the standard cost factor with an explicit `method: "bcrypt"` identifier.

### 2. RESTful API Web Service (`internal/api`)
* **Standard Library Routing**: Built on Go 1.22+ `http.ServeMux` pattern routing (`GET /api/v1/profiles/{id}`, `GET /api/v1/profiles`), keeping the binary lean and eliminating heavyweight external routing frameworks.
* **Liveness vs. Readiness Probes**:
  * `GET /api/v1/health`: Lightweight liveness probe confirming the HTTP listener is operational.
  * `GET /api/v1/ready`: Comprehensive readiness probe verifying downstream database connectivity via atomic connection pool ping (`dao.Ping(ctx)`). Returns HTTP 503 if database is unreachable.
* **OpenAPI 3.1 & Interactive Documentation**:
  * Complete specification maintained at [`docs/openapi.yaml`](docs/openapi.yaml).
  * Directly served by the running service at `GET /api/v1/openapi.yaml` via standard library `go:embed`.
* **Security & Authentication Middleware**:
  * `WithAuth`: Enforces Bearer token authentication (`Authorization: Bearer <token>`). Tokens are validated using `crypto/subtle.ConstantTimeCompare` to defend against timing side-channel attacks.
  * `WithRecovery`: Traps panics within handlers and returns standardized RFC-style JSON 500 responses without crashing the HTTP listener.
  * `WithLogging`: Emits structured JSON access logs using Go's standard `log/slog`.
  * `WithRateLimit`: In-memory IP-based token bucket rate limiter to prevent denial-of-service and brute-force attacks.
* **Parameterized Search**: Full filtering across `name`, `phone`, `locality`, `region`, and `country` with clamped pagination (`limit <= 100`, `offset <= 10000`) to prevent memory exhaustion and ensure bounded query execution.

### 3. 3rd-Party Identity Provider Connector (`internal/connector`)
* **Normalized Vendor Protocol**:
  * `POST /auth`: Exchanges client credentials `{ "username": "...", "password": "..." }` for `{ "access_token": "..." }`.
  * `POST /identity`: Uses `Authorization: Bearer <token>` to verify personal data from `{ "phone": "...", "name": "..." }` and retrieve structured PII.
* **Singleflight Concurrency Collapsing**: Under high traffic, multiple concurrent requests for unauthenticated sessions could overwhelm vendor `/auth` endpoints (the "thundering herd" problem). The connector uses `golang.org/x/sync/singleflight` to collapse concurrent token requests into a single upstream call.
* **Resilient HTTP Client & Auto-Retry**:
  * Configurable connection pooling (`MaxIdleConns: 100`) and request timeouts (10s default).
  * Automatically caches access tokens with a TTL safety margin.
  * If a vendor returns HTTP 401 Unauthorized during an `/identity` query (e.g. token revoked or prematurely expired), the connector invalidates its local cache, re-authenticates, and transparently retries the query once.
* **PII Redaction In Logs**: Domain models implement `slog.LogValuer`, masking phone numbers (`***-***-1234`) and street addresses in structured logs to prevent PII leakage into log sinks.

---

## 🛠️ Tools, Frameworks & AI Attribution

*As requested by the prompt ("describe any tool, framework, or AI used"):*

| Component | Tool / Library / Framework | Rationale |
|---|---|---|
| **Language & Runtime** | Go 1.26.6+ | Modern, high-performance concurrency with standard library HTTP routing and structured logging. Requires Go 1.26.6+ toolchain. |
| **SQLite Driver** | `modernc.org/sqlite` | Pure-Go SQLite implementation. Zero CGO dependencies, enabling fast, isolated, cross-platform in-memory testing. |
| **Postgres / Cockroach Driver** | `github.com/lib/pq` | Battle-tested PostgreSQL driver compatible with both PostgreSQL and CockroachDB wire protocols. |
| **Concurrency & Synchronization** | `golang.org/x/sync/singleflight` | Prevents duplicate upstream authentications during cache stampedes. |
| **Password Hashing** | `golang.org/x/crypto/bcrypt` | Cryptographically secure, salted adaptive password hashing. |
| **Static Analysis & Linting** | `golangci-lint` (`errcheck`, `staticcheck`, `unused`, `gosec`) | Enforces rigorous Go formatting, error checking, and security guidelines. |
| **Containerization** | Docker & Docker Compose | Multi-container orchestration for PostgreSQL and CockroachDB verification. |
| **AI Assistance** | Google Antigravity (Gemini 3.8 Flash) | Pair-programmed architecture scaffolding, test suite generation, and dialect compatibility design. |

---

## 🚀 Quick Start

### 1. Run Unit Tests & Verify Coverage (Strict 91%+ Threshold)
```bash
make check-coverage
```
*Runs all tests with `-race`, generates `coverage.out`, and verifies test statement coverage meets or exceeds the **91.0%** threshold.*

### 2. Run Lint & Code Checks
```bash
make lint
```

### 3. Run Vulnerability Check (`govulncheck`)
```bash
make vuln
```
*Scans Go source code and dependencies against the Go Vulnerability Database for known CVEs.*

### 4. Run Live System Integration Tests
```bash
make test-system
```
*Compiles `./bin/lid-server`, auto-spawns an ephemeral instance, executes all 17 API integration checks, and tests live 3rd-party vendor connector integration.*

### 5. Build & Run Server Locally (SQLite Default)
```bash
# Build binary to ./bin/lid-server
make build

# Run the server locally on :8080 (SQLite default, persists to lid.db, runs in development mode)
make run
# or directly with go:
APP_ENV=development go run ./cmd/server/main.go
```

### 6. Run Multi-Database Integration (PostgreSQL & CockroachDB)
```bash
# Start PostgreSQL (5432) and CockroachDB (26257) containers
make docker-up

# Run with PostgreSQL in development mode
APP_ENV=development DB_DRIVER=postgres DB_DSN="postgres://lid_user:lid_password@localhost:5432/lid_db?sslmode=disable" go run ./cmd/server/main.go

# Run with CockroachDB in development mode
APP_ENV=development DB_DRIVER=cockroach DB_DSN="postgres://root@localhost:26257/defaultdb?sslmode=disable" go run ./cmd/server/main.go

# Tear down containers
make docker-down
```

---

## 🪐 Running Locally with OrbStack

[OrbStack](https://orbstack.dev) is a lightweight, high-performance container and Linux machine runtime for macOS. It offers native Apple Silicon performance, battery efficiency, and zero-configuration local networking.

### Option 1: Full Containerized Stack via OrbStack
Run the API server alongside PostgreSQL and CockroachDB inside Docker containers managed by OrbStack:

```bash
# 1. Provide an administrative authentication secret (required by docker-compose)
export AUTH_SECRET="dev-secret-token"

# 2. (Optional) Custom vendor credentials if connecting to live upstream vendor endpoints
export VENDOR_ABC_PASSWORD="secret_abc"
export VENDOR_XYZ_PASSWORD="secret_xyz"

# 3. Start all services (server, postgres, cockroach) in background
make docker-up
# or directly:
AUTH_SECRET="dev-secret-token" docker compose up -d
```

#### OrbStack Native Domain Routing
OrbStack automatically provisions local `.orb.local` domains without modifying `/etc/hosts` or causing port conflicts:
* **API Server**: [`http://lid-server.orb.local:8080`](http://lid-server.orb.local:8080) or `http://localhost:8080`
* **CockroachDB Web Console**: [`http://lid-cockroach.orb.local:8081`](http://lid-cockroach.orb.local:8081) or `http://localhost:8081`
* **PostgreSQL Port**: `lid-postgres.orb.local:5432` or `localhost:5432`

#### Verify health in OrbStack:
```bash
curl -X GET http://localhost:8080/api/v1/health
# or using OrbStack DNS:
curl -X GET http://lid-server.orb.local:8080/api/v1/health
```

### Option 2: Hybrid Development (Databases in OrbStack, Go on Host)
For interactive debugging with instant re-compilation, run the database backends in OrbStack and execute the Go binary locally on your host:

```bash
# 1. Start only database containers
docker compose up -d postgres cockroach

# 2. Run the Go server against OrbStack's PostgreSQL in development mode
APP_ENV=development DB_DRIVER=postgres DB_DSN="postgres://lid_user:lid_password@localhost:5432/lid_db?sslmode=disable" go run ./cmd/server/main.go

# 3. Or run against OrbStack's CockroachDB in development mode
APP_ENV=development DB_DRIVER=cockroach DB_DSN="postgres://root@localhost:26257/defaultdb?sslmode=disable" go run ./cmd/server/main.go

# 4. Or run zero-container SQLite in development mode
make run
```

### Container Management & Cleanup
```bash
# Stream server logs
docker compose logs -f server

# Stop and tear down all OrbStack containers
make docker-down
```

---

## 📡 API Endpoints & Usage Examples

### 1. Health Check
```bash
curl -X GET http://localhost:8080/api/v1/health
```
```json
{
  "status": "healthy",
  "timestamp": "2026-09-11T21:30:00Z"
}
```

### 2. Register / Create User Profile & Credentials
```bash
curl -X POST http://localhost:8080/api/v1/users \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Daniel Borch",
    "phone": "3035551234",
    "address": {
      "street_address": "100 Innovation Way",
      "locality": "Denver",
      "region": "CO",
      "postal_code": "80202",
      "country": "USA"
    },
    "username": "daniel",
    "password": "CorrectHorseBatteryStaple!"
  }'
```
```json
{
  "user_id": "c7162985-782a-4ce5-b77a-c32ab5d32c01",
  "profile": {
    "id": "c7162985-782a-4ce5-b77a-c32ab5d32c01",
    "name": "Daniel Borch",
    "phone": "3035551234",
    "address": {
      "street_address": "100 Innovation Way",
      "locality": "Denver",
      "region": "CO",
      "postal_code": "80202",
      "country": "USA"
    },
    "created_at": "2026-09-11T21:30:00Z",
    "updated_at": "2026-09-11T21:30:00Z"
  }
}
```

### 3. Authenticate / Login
```bash
curl -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "username": "daniel",
    "password": "CorrectHorseBatteryStaple!"
  }'
```
```json
{
  "access_token": "<signed-user-token: payload.signature>",
  "profile": {
    "id": "c7162985-782a-4ce5-b77a-c32ab5d32c01",
    "name": "Daniel Borch",
    "phone": "3035551234",
    "address": { ... }
  }
}
```

### 4. Retrieve Profile by ID (Authenticated User or Admin)
> **Authorization Note**: Standard user tokens (`$AUTH_TOKEN`) are authorized to retrieve their own profile matching the token subject (`claims.UserID == id`). Administrative access via master secret (`$AUTH_SECRET`) can retrieve any profile. Attempting cross-user profile retrieval with a user token returns `403 Forbidden`.

```bash
curl -X GET http://localhost:8080/api/v1/profiles/c7162985-782a-4ce5-b77a-c32ab5d32c01 \
  -H "Authorization: Bearer $AUTH_TOKEN"
```

### 5. Search Profiles with Filters & Pagination (Administrative)
> **Authorization Note**: Directory search queries and returns profile PII across all users and is strictly restricted to administrative/master credentials (`$AUTH_SECRET`). Standard user tokens receive `403 Forbidden` to prevent mass PII scraping and user enumeration.
>
> **Indexing Note**: The `name` and `phone` filters perform case-insensitive substring matching with parameterized SQL and wildcard escaping. In high-volume PostgreSQL/CockroachDB production deployments, trigram GIN indexes (`pg_trgm`) or Full-Text Search (FTS) eliminate sequential table scans for substring queries. Standard B-Tree indexes (`idx_user_profile_name_lower`, `idx_user_profile_phone`) serve exact and prefix lookups.

```bash
curl -X GET "http://localhost:8080/api/v1/profiles?name=Daniel&locality=Denver&limit=10&offset=0" \
  -H "Authorization: Bearer $AUTH_SECRET"
```
```json
{
  "data": [
    {
      "id": "c7162985-782a-4ce5-b77a-c32ab5d32c01",
      "name": "Daniel Borch",
      "phone": "3035551234",
      "address": {
        "street_address": "100 Innovation Way",
        "locality": "Denver",
        "region": "CO",
        "postal_code": "80202",
        "country": "USA"
      }
    }
  ],
  "count": 1,
  "limit": 10,
  "offset": 0
}
```
