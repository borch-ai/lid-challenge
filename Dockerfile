# Build Stage
FROM golang:1.26.6-alpine AS builder

WORKDIR /app

# Install git and ca-certificates
# hadolint ignore=DL3018
RUN apk add --no-cache git ca-certificates

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Synchronize embedded OpenAPI specification and build statically linked binary (pure Go, zero CGO)
RUN cp docs/openapi.yaml internal/api/openapi.yaml && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/lid-server ./cmd/server

# Production Stage
FROM alpine:3.21

# Install ca-certificates and curl for health check
# hadolint ignore=DL3018
RUN apk add --no-cache ca-certificates curl tzdata

# Create non-root user
RUN addgroup -g 10001 -S appgroup && adduser -u 10001 -S appuser -G appgroup

WORKDIR /app

# Copy binary from builder
COPY --from=builder /bin/lid-server /app/lid-server

# Change ownership
RUN chown -R 10001:10001 /app

USER 10001:10001

EXPOSE 8080

HEALTHCHECK --interval=5s --timeout=3s --retries=3 \
  CMD ["curl", "-f", "http://localhost:8080/api/v1/ready"]

ENTRYPOINT ["/app/lid-server"]
