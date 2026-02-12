// Package api embeds the OpenAPI specification for serving at runtime.
package api

import _ "embed"

// OpenAPISpec is the raw OpenAPI 3.1 YAML specification.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
