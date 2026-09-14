package api

import (
	_ "embed"
	"net/http"
)

// openapi.yaml is synchronized directly from the canonical docs/openapi.yaml specification.
//
//go:embed openapi.yaml
var openAPISpec []byte

// handleOpenAPI serves the embedded OpenAPI 3.1 specification YAML.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPISpec)
}
