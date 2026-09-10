package api

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.yaml
var openapiSpec []byte

// redocHTML renders the embedded OpenAPI spec with Redoc. The viewer script is
// loaded from a CDN (the spec itself is served locally and is the source of
// truth, usable offline or with any OpenAPI tooling via /api/openapi.yaml).
const redocHTML = `<!DOCTYPE html>
<html>
  <head>
    <title>Quetzal API</title>
    <meta charset="utf-8"/>
    <meta name="viewport" content="width=device-width, initial-scale=1"/>
    <style>body { margin: 0; padding: 0; }</style>
  </head>
  <body>
    <redoc spec-url="openapi.yaml"></redoc>
    <script src="https://cdn.jsdelivr.net/npm/redoc@2/bundles/redoc.standalone.js"></script>
  </body>
</html>`

// handleOpenAPISpec serves the machine-readable API contract.
func (s *Server) handleOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(openapiSpec)
}

// docsCSP relaxes the panel policy for this one page: Redoc comes from a CDN and
// styles itself at runtime. It stays framing- and object-free, and grants nothing
// to the rest of the panel.
const docsCSP = "default-src 'self'; " +
	"script-src 'self' https://cdn.jsdelivr.net 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https:; " +
	"font-src 'self' data: https:; " +
	"worker-src 'self' blob:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'; " +
	"object-src 'none'"

// handleDocs serves human-readable API docs rendered from the spec.
func (s *Server) handleDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", docsCSP)
	_, _ = w.Write([]byte(redocHTML))
}
