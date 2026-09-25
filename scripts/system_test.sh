#!/usr/bin/env bash
set -euo pipefail

USE_EXISTING_SERVER="${USE_EXISTING_SERVER:-false}"
TEST_PORT="${TEST_PORT:-8088}"

if [ "$USE_EXISTING_SERVER" = "true" ]; then
  BASE_URL="${BASE_URL:-http://localhost:8080}"
else
  BASE_URL="http://localhost:${TEST_PORT}"
fi

APP_ENV="${APP_ENV:-test}"
NORMALIZED_ENV="$(echo "$APP_ENV" | tr '[:upper:]' '[:lower:]' | xargs)"
if [ -z "${AUTH_SECRET:-}" ]; then
  if [ "$NORMALIZED_ENV" = "development" ] || [ "$NORMALIZED_ENV" = "dev" ] || [ "$NORMALIZED_ENV" = "test" ]; then
    AUTH_SECRET="dev-secret-token"
  else
    echo "Error: AUTH_SECRET must be explicitly provided when APP_ENV is not development, dev, or test."
    exit 1
  fi
fi

RUN_ID="$(date +%s)_$RANDOM"
TEST_DB="system_test_${RUN_ID}.db"
SERVER_PID=""
ACTUAL_DRIVER="${DB_DRIVER:-}"
ACTUAL_DSN="${DB_DSN:-}"

cleanup() {
  if [ -n "$SERVER_PID" ]; then
    echo "Stopping isolated test server (PID $SERVER_PID)..."
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -f "$TEST_DB"*
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "========================================="
echo "🚀 Running LID Service Live System Tests"
echo "Target: $BASE_URL | Run ID: $RUN_ID"
echo "========================================="

if [ "$USE_EXISTING_SERVER" = "true" ]; then
  ALLOW_MUTATING_TESTS="${ALLOW_MUTATING_TESTS:-false}"
  if [ "$ALLOW_MUTATING_TESTS" != "true" ]; then
    echo "Error: USE_EXISTING_SERVER=true will mutate the target database by creating and modifying users."
    echo "To run tests against an existing server, explicitly set ALLOW_MUTATING_TESTS=true."
    exit 1
  fi
  if ! curl -s --connect-timeout 2 --max-time 5 -f "$BASE_URL/api/v1/health" >/dev/null 2>&1; then
    echo "Error: USE_EXISTING_SERVER=true specified, but no server is reachable at $BASE_URL."
    exit 1
  fi
  echo "Targeting existing server at $BASE_URL (opt-in mode, ALLOW_MUTATING_TESTS=true)."
else
  echo "Starting isolated test server on port $TEST_PORT..."
  if curl -s --connect-timeout 2 --max-time 5 -f "$BASE_URL/api/v1/health" >/dev/null 2>&1; then
    echo "Error: Port $TEST_PORT is already occupied by a running service at $BASE_URL. Please set TEST_PORT to an unused port or terminate the existing process."
    exit 1
  fi
  echo "Compiling ./bin/lid-server from current source..."
  mkdir -p ./bin
  go build -o ./bin/lid-server ./cmd/server
  if [ -z "$ACTUAL_DRIVER" ]; then
    ACTUAL_DRIVER="sqlite"
  fi
  NORMALIZED_DRIVER="$(echo "$ACTUAL_DRIVER" | tr '[:upper:]' '[:lower:]' | xargs)"
  if [ -n "$ACTUAL_DSN" ]; then
    ALLOW_MUTATING_TESTS="${ALLOW_MUTATING_TESTS:-false}"
    if [ "$ALLOW_MUTATING_TESTS" != "true" ]; then
      echo "Error: Custom DB_DSN supplied in isolated server mode."
      echo "To run tests against a custom database, explicitly set ALLOW_MUTATING_TESTS=true."
      exit 1
    fi
  elif [ "$NORMALIZED_DRIVER" = "sqlite" ] || [ "$NORMALIZED_DRIVER" = "sqlite3" ]; then
    ACTUAL_DSN="$TEST_DB"
  else
    echo "Error: DB_DRIVER='$ACTUAL_DRIVER' requires an explicit DB_DSN connection string and ALLOW_MUTATING_TESTS=true."
    exit 1
  fi
  echo "Using database driver: $ACTUAL_DRIVER"
  APP_ENV="${APP_ENV:-test}" SERVER_PORT="$TEST_PORT" DB_DRIVER="$ACTUAL_DRIVER" DB_DSN="$ACTUAL_DSN" AUTH_SECRET="$AUTH_SECRET" ./bin/lid-server &
  SERVER_PID=$!
  echo "Started isolated server with PID $SERVER_PID. Waiting for health check..."
  READY=0
  for attempt in $(seq 1 30); do
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then
      echo "Error: Isolated server process (PID $SERVER_PID) terminated unexpectedly during startup."
      exit 1
    fi
    if curl -s --connect-timeout 2 --max-time 5 -f "$BASE_URL/api/v1/health" >/dev/null 2>&1; then
      READY=1
      echo "Server is healthy and ready (attempt $attempt)!"
      break
    fi
    sleep 0.2
  done
  if [ "$READY" -ne 1 ]; then
    echo "Error: Isolated server failed to become healthy at $BASE_URL within 6s."
    exit 1
  fi
fi

# 1. Health Check
echo -n "1. Testing Health Check... "
HEALTH_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" "$BASE_URL/api/v1/health")
HTTP_CODE=$(echo "$HEALTH_RESP" | tail -n1)
BODY=$(echo "$HEALTH_RESP" | sed '$d')
if [ "$HTTP_CODE" -ne 200 ]; then
  echo "FAIL (HTTP $HTTP_CODE): $BODY"
  exit 1
fi
echo "PASS (HTTP 200) -> $BODY"

# 1b. Readiness Probe (Database connectivity verification)
echo -n "1b. Testing Readiness Probe (DB Ping)... "
READY_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" "$BASE_URL/api/v1/ready")
HTTP_CODE=$(echo "$READY_RESP" | tail -n1)
BODY=$(echo "$READY_RESP" | sed '$d')
if [ "$HTTP_CODE" -ne 200 ]; then
  echo "FAIL (HTTP $HTTP_CODE): $BODY"
  exit 1
fi
if ! echo "$BODY" | grep -q '"database":"healthy"'; then
  echo "FAIL: Expected database: healthy, got: $BODY"
  exit 1
fi
echo "PASS (HTTP 200) -> $BODY"

# 1c. OpenAPI Specification Endpoint
echo -n "1c. Testing OpenAPI Spec (GET /api/v1/openapi.yaml)... "
OPENAPI_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" "$BASE_URL/api/v1/openapi.yaml")
HTTP_CODE=$(echo "$OPENAPI_RESP" | tail -n1)
BODY=$(echo "$OPENAPI_RESP" | sed '$d')
if [ "$HTTP_CODE" -ne 200 ]; then
  echo "FAIL (HTTP $HTTP_CODE): $BODY"
  exit 1
fi
if ! echo "$BODY" | grep -q 'openapi: 3.1.0'; then
  echo "FAIL: Expected openapi: 3.1.0 in response"
  exit 1
fi
echo "PASS (HTTP 200) -> OpenAPI 3.1.0 Spec Served"

# 2. Create User Profile & Credentials (Alice)
ALICE_USER="alice_${RUN_ID}"
ALICE_NAME="Alice_${RUN_ID}"
echo -n "2. Creating User ($ALICE_USER)... "
CREATE_PAYLOAD="{
  \"name\": \"$ALICE_NAME\",
  \"phone\": \"3035551111\",
  \"address\": {
    \"street_address\": \"123 Denver St\",
    \"locality\": \"Denver\",
    \"region\": \"CO\",
    \"postal_code\": \"80202\",
    \"country\": \"USA\"
  },
  \"username\": \"$ALICE_USER\",
  \"password\": \"Password123!\"
}"
CREATE_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -X POST "$BASE_URL/api/v1/users" \
  -H "Content-Type: application/json" \
  -d "$CREATE_PAYLOAD")
HTTP_CODE=$(echo "$CREATE_RESP" | tail -n1)
BODY=$(echo "$CREATE_RESP" | sed '$d')
if [ "$HTTP_CODE" -ne 201 ]; then
  echo "FAIL (HTTP $HTTP_CODE): $BODY"
  exit 1
fi
ALICE_ID=$(echo "$BODY" | grep -o '"user_id":"[^"]*' | cut -d'"' -f4)
echo "PASS (HTTP 201) -> Created User ID: $ALICE_ID"

# 3. Conflict Check (Duplicate Username)
echo -n "3. Testing Duplicate User Conflict... "
DUP_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -X POST "$BASE_URL/api/v1/users" \
  -H "Content-Type: application/json" \
  -d "$CREATE_PAYLOAD")
HTTP_CODE=$(echo "$DUP_RESP" | tail -n1)
if [ "$HTTP_CODE" -ne 409 ]; then
  echo "FAIL: Expected 409 Conflict, got $HTTP_CODE"
  exit 1
fi
echo "PASS (HTTP 409 Conflict properly rejected)"

# 4. Create Second User (Bob)
BOB_USER="bob_${RUN_ID}"
BOB_NAME="Bob_${RUN_ID}"
BOB_LOC="Locality_${RUN_ID}"
echo -n "4. Creating Second User ($BOB_USER)... "
BOB_PAYLOAD="{
  \"name\": \"$BOB_NAME\",
  \"phone\": \"3035552222\",
  \"address\": {
    \"street_address\": \"456 Boulder Way\",
    \"locality\": \"$BOB_LOC\",
    \"region\": \"CO\",
    \"postal_code\": \"80302\",
    \"country\": \"USA\"
  },
  \"username\": \"$BOB_USER\",
  \"password\": \"CanWeFixIt123!\"
}"
BOB_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -X POST "$BASE_URL/api/v1/users" \
  -H "Content-Type: application/json" \
  -d "$BOB_PAYLOAD")
HTTP_CODE=$(echo "$BOB_RESP" | tail -n1)
if [ "$HTTP_CODE" -ne 201 ]; then
  echo "FAIL (HTTP $HTTP_CODE)"
  exit 1
fi
BOB_ID=$(echo "$BOB_RESP" | sed '$d' | grep -o '"user_id":"[^"]*' | cut -d'"' -f4)
echo "PASS (HTTP 201) -> Created User ID: $BOB_ID"

# 5. Authenticate / Login
echo -n "5. Testing Valid Authentication... "
LOGIN_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -X POST "$BASE_URL/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"username\": \"$ALICE_USER\", \"password\": \"Password123!\"}")
HTTP_CODE=$(echo "$LOGIN_RESP" | tail -n1)
BODY=$(echo "$LOGIN_RESP" | sed '$d')
if [ "$HTTP_CODE" -ne 200 ]; then
  echo "FAIL (HTTP $HTTP_CODE): $BODY"
  exit 1
fi
ALICE_TOKEN=$(echo "$BODY" | grep -o '"access_token":"[^"]*' | cut -d'"' -f4)
if [ -z "$ALICE_TOKEN" ]; then
  echo "FAIL: Missing access_token in login response: $BODY"
  exit 1
fi
echo "PASS (HTTP 200) -> Acquired User Token"

echo -n "6. Testing Invalid Authentication... "
BAD_LOGIN_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -X POST "$BASE_URL/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"username\": \"$ALICE_USER\", \"password\": \"WrongPassword!\"}")
HTTP_CODE=$(echo "$BAD_LOGIN_RESP" | tail -n1)
if [ "$HTTP_CODE" -ne 401 ]; then
  echo "FAIL: Expected 401 Unauthorized, got $HTTP_CODE"
  exit 1
fi
echo "PASS (HTTP 401 properly rejected)"

# 7. Security: Unauthenticated profile access
echo -n "7. Testing Unauthorized Profile Access... "
NO_AUTH_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" "$BASE_URL/api/v1/profiles/$ALICE_ID")
HTTP_CODE=$(echo "$NO_AUTH_RESP" | tail -n1)
if [ "$HTTP_CODE" -ne 401 ]; then
  echo "FAIL: Expected 401 Unauthorized, got $HTTP_CODE"
  exit 1
fi
echo "PASS (HTTP 401 missing token rejected)"

# 8. Security: Invalid Bearer token
echo -n "8. Testing Invalid Bearer Token... "
INVALID_TOKEN="invalid_sample_token"
BAD_TOKEN_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -H "Authorization: Bearer $INVALID_TOKEN" "$BASE_URL/api/v1/profiles/$ALICE_ID")
HTTP_CODE=$(echo "$BAD_TOKEN_RESP" | tail -n1)
if [ "$HTTP_CODE" -ne 401 ]; then
  echo "FAIL: Expected 401 Unauthorized, got $HTTP_CODE"
  exit 1
fi
echo "PASS (HTTP 401 invalid token rejected)"

# 9. Authenticated Profile Retrieval (Using User's Issued Token)
echo -n "9. Testing Authenticated Profile Retrieval (User Token)... "
GET_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -H "Authorization: Bearer $ALICE_TOKEN" "$BASE_URL/api/v1/profiles/$ALICE_ID")
HTTP_CODE=$(echo "$GET_RESP" | tail -n1)
BODY=$(echo "$GET_RESP" | sed '$d')
if [ "$HTTP_CODE" -ne 200 ]; then
  echo "FAIL (HTTP $HTTP_CODE): $BODY"
  exit 1
fi
if ! echo "$BODY" | grep -q "$ALICE_NAME"; then
  echo "FAIL: Expected $ALICE_NAME in profile response: $BODY"
  exit 1
fi
echo "PASS (HTTP 200) -> Retrieved profile for $ALICE_NAME using signed user token"

# 9b. Authenticated Profile Retrieval (Using Master Service AuthSecret)
echo -n "9b. Testing Authenticated Profile Retrieval (Master Secret)... "
GET_MASTER_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -H "Authorization: Bearer $AUTH_SECRET" "$BASE_URL/api/v1/profiles/$ALICE_ID")
HTTP_CODE=$(echo "$GET_MASTER_RESP" | tail -n1)
if [ "$HTTP_CODE" -ne 200 ]; then
  echo "FAIL (HTTP $HTTP_CODE)"
  exit 1
fi
echo "PASS (HTTP 200) -> Master service secret verified"

# 9c. Security: User token forbidden on cross-user profile retrieval
echo -n "9c. Testing User Token Forbidden on Cross-User Profile... "
FORBIDDEN_PROFILE_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -H "Authorization: Bearer $ALICE_TOKEN" "$BASE_URL/api/v1/profiles/$BOB_ID")
HTTP_CODE=$(echo "$FORBIDDEN_PROFILE_RESP" | tail -n1)
if [ "$HTTP_CODE" -ne 403 ]; then
  echo "FAIL: Expected 403 Forbidden for cross-user profile retrieval, got $HTTP_CODE"
  exit 1
fi
echo "PASS (HTTP 403 properly rejected)"

# 9d. Security: User token forbidden on directory search
echo -n "9d. Testing User Token Forbidden on Directory Search... "
FORBIDDEN_SEARCH_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -H "Authorization: Bearer $ALICE_TOKEN" "$BASE_URL/api/v1/profiles?name=$BOB_NAME")
HTTP_CODE=$(echo "$FORBIDDEN_SEARCH_RESP" | tail -n1)
if [ "$HTTP_CODE" -ne 403 ]; then
  echo "FAIL: Expected 403 Forbidden for user token search, got $HTTP_CODE"
  exit 1
fi
echo "PASS (HTTP 403 properly rejected)"

# 10. Profile Search by Name (Administrative)
echo -n "10. Testing Profile Search by Name ($ALICE_NAME)... "
SEARCH_NAME_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -H "Authorization: Bearer $AUTH_SECRET" "$BASE_URL/api/v1/profiles?name=$ALICE_NAME")
HTTP_CODE=$(echo "$SEARCH_NAME_RESP" | tail -n1)
BODY=$(echo "$SEARCH_NAME_RESP" | sed '$d')
if [ "$HTTP_CODE" -ne 200 ]; then
  echo "FAIL (HTTP $HTTP_CODE): $BODY"
  exit 1
fi
COUNT=$(echo "$BODY" | grep -o '"count":[0-9]*' | cut -d: -f2)
if [ "$COUNT" -ne 1 ]; then
  echo "FAIL: Expected count 1, got $COUNT: $BODY"
  exit 1
fi
echo "PASS (HTTP 200) -> Matched 1 record"

# 11. Profile Search by Locality (Administrative)
echo -n "11. Testing Profile Search by Locality ($BOB_LOC)... "
SEARCH_LOC_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -H "Authorization: Bearer $AUTH_SECRET" "$BASE_URL/api/v1/profiles?locality=$BOB_LOC")
HTTP_CODE=$(echo "$SEARCH_LOC_RESP" | tail -n1)
BODY=$(echo "$SEARCH_LOC_RESP" | sed '$d')
COUNT=$(echo "$BODY" | grep -o '"count":[0-9]*' | cut -d: -f2)
if [ "$COUNT" -ne 1 ]; then
  echo "FAIL: Expected count 1 for $BOB_LOC, got $COUNT"
  exit 1
fi
echo "PASS (HTTP 200) -> Matched $BOB_NAME in $BOB_LOC"

# 12. Search Pagination (Administrative)
echo -n "12. Testing Pagination (limit=1, offset=0)... "
PAGE_RESP=$(curl -s --connect-timeout 2 --max-time 10 -w "\n%{http_code}" -H "Authorization: Bearer $AUTH_SECRET" "$BASE_URL/api/v1/profiles?limit=1&offset=0")
HTTP_CODE=$(echo "$PAGE_RESP" | tail -n1)
BODY=$(echo "$PAGE_RESP" | sed '$d')
COUNT=$(echo "$BODY" | grep -o '"count":[0-9]*' | cut -d: -f2)
if [ "$COUNT" -ne 1 ]; then
  echo "FAIL: Expected paginated count 1, got $COUNT"
  exit 1
fi
echo "PASS (HTTP 200) -> Pagination working as expected"

# 13. Schema Migration CLI verification
if [ -n "${ACTUAL_DSN:-}" ]; then
  echo -n "13. Testing Schema Migration CLI (status & version)... "
  if [ ! -f ./bin/lid-server ]; then
    mkdir -p ./bin
    go build -o ./bin/lid-server ./cmd/server
  fi
  CLI_DRIVER="${ACTUAL_DRIVER:-sqlite}"
  STATUS_OUT=$(APP_ENV="$APP_ENV" AUTH_SECRET="$AUTH_SECRET" DB_DRIVER="$CLI_DRIVER" DB_DSN="$ACTUAL_DSN" ./bin/lid-server migrate status 2>&1)
  if ! echo "$STATUS_OUT" | grep -q "APPLIED"; then
    echo "FAIL: expected applied migrations in status output: $STATUS_OUT"
    exit 1
  fi
  VERSION_OUT=$(APP_ENV="$APP_ENV" AUTH_SECRET="$AUTH_SECRET" DB_DRIVER="$CLI_DRIVER" DB_DSN="$ACTUAL_DSN" ./bin/lid-server migrate version 2>&1)
  if ! echo "$VERSION_OUT" | grep -q "Current schema version:"; then
    echo "FAIL: expected version in version output: $VERSION_OUT"
    exit 1
  fi
  echo "PASS -> Migration status and version verified"
else
  echo "13. Skipping Schema Migration CLI verification (USE_EXISTING_SERVER=true without local DB_DSN)."
fi

echo "========================================="
echo "✅ All Live System Tests Passed Successfully!"
echo "========================================="
