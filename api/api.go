// Package api embeds the public API contract and its documentation page so
// the gateway serves exactly the spec it was built with.
package api

import _ "embed"

// OpenAPI is api/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte

// DocsHTML is the Swagger UI page served at /docs. It loads the vendored
// swagger-ui files from /docs/ and the spec from /openapi.yaml.
//
//go:embed docs.html
var DocsHTML []byte
