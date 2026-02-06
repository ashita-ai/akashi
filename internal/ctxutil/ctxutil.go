// Package ctxutil provides shared context key accessors.
//
// This package exists to break the circular dependency between server and mcp:
// server imports mcp for MCP server setup, and mcp needs to read JWT claims
// from the context that server's auth middleware populates. Both packages
// import ctxutil instead of each other.
package ctxutil

import (
	"context"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/auth"
)

type contextKey string

const (
	keyClaims contextKey = "claims"
	keyOrgID  contextKey = "org_id"
)

// WithClaims returns a new context carrying the given claims.
func WithClaims(ctx context.Context, claims *auth.Claims) context.Context {
	ctx = context.WithValue(ctx, keyClaims, claims)
	ctx = context.WithValue(ctx, keyOrgID, claims.OrgID)
	return ctx
}

// ClaimsFromContext extracts the JWT claims from the context.
func ClaimsFromContext(ctx context.Context) *auth.Claims {
	if v, ok := ctx.Value(keyClaims).(*auth.Claims); ok {
		return v
	}
	return nil
}

// OrgIDFromContext extracts the org_id from the context.
func OrgIDFromContext(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(keyOrgID).(uuid.UUID); ok {
		return v
	}
	return uuid.Nil
}
