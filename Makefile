BINARY_NAME=lid-server
BUILD_DIR=./bin
COVERAGE_FILE=coverage.out
COVERAGE_THRESHOLD=91.0

.PHONY: all build test test-system check-coverage lint vuln fmt tidy clean docker-up docker-down run sync-openapi

## Default target: fmt, tidy, sync-openapi, lint, check-coverage, build
all: fmt tidy sync-openapi lint check-coverage build

## Synchronize embedded OpenAPI specification from canonical docs/openapi.yaml
sync-openapi:
	@cmp -s docs/openapi.yaml internal/api/openapi.yaml || cp docs/openapi.yaml internal/api/openapi.yaml

## Build the server binary
build: sync-openapi
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/server

## Run the server locally with SQLite
run: sync-openapi
	APP_ENV="$${APP_ENV:-development}" go run ./cmd/server/main.go

## Run live system integration tests
test-system: build
	./scripts/system_test.sh
	go run ./scripts/test_connector_live.go

## Run all unit tests with race detector and coverage
test:
	go test -race -coverprofile=$(COVERAGE_FILE) -covermode=atomic ./internal/...

## Verify unit test coverage meets the strict threshold
check-coverage: test
	@go tool cover -func=$(COVERAGE_FILE) | awk -v min="$(COVERAGE_THRESHOLD)" ' \
		BEGIN {matched=0} \
		/total:/ { \
			matched=1; \
			print $$0; \
			gsub("%","",$$NF); \
			if ($$NF < min) { \
				print "FAIL: coverage " $$NF "% is below threshold " min "%"; \
				exit 1; \
			} else { \
				print "PASS: coverage " $$NF "% meets threshold " min "%"; \
				exit 0; \
			} \
		} \
		END { \
			if (matched==0) { \
				print "Error: total coverage line not found"; \
				exit 1; \
			} \
		}'

## Run linter tools
lint:
	@GOBIN=$$(go env GOBIN); \
	GOPATH=$$(go env GOPATH); \
	if [ -z "$$GOBIN" ]; then GOBIN=$$GOPATH/bin; fi; \
	if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	elif [ -f "$$GOBIN/golangci-lint" ]; then \
		$$GOBIN/golangci-lint run; \
	else \
		echo "golangci-lint not found in PATH or GOBIN. Installing pinned version @v2.12.2..."; \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2; \
		if [ -f "$$GOBIN/golangci-lint" ]; then \
			$$GOBIN/golangci-lint run; \
		else \
			echo "Error: failed to install or execute golangci-lint" >&2; \
			exit 1; \
		fi; \
	fi

## Run vulnerability check
vuln:
	@echo "Checking for vulnerabilities..."
	@GOBIN=$$(go env GOBIN); \
	GOPATH=$$(go env GOPATH); \
	if [ -z "$$GOBIN" ]; then GOBIN=$$GOPATH/bin; fi; \
	if [ ! -f "$$GOBIN/govulncheck" ]; then \
		echo "Installing govulncheck..."; \
		go install golang.org/x/vuln/cmd/govulncheck@v1.1.4; \
	fi; \
	$$GOBIN/govulncheck ./...

## Format all Go source files
fmt:
	go fmt ./...

## Tidy go.mod and go.sum
tidy:
	go mod tidy

## Remove build artifacts
clean:
	rm -rf $(BUILD_DIR) $(COVERAGE_FILE)

## Start all services in Docker containers (via OrbStack / Docker)
docker-up:
	docker compose up --build -d

## Stop Docker containers
docker-down:
	docker compose down
