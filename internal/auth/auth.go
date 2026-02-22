// Package auth provides JWT-based authentication and RBAC authorization for Akashi.
//
// Uses Ed25519 (EdDSA) for JWT signing. Keys can be loaded from PEM files
// or auto-generated for development.
package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// Claims extends jwt.RegisteredClaims with Akashi-specific fields.
type Claims struct {
	jwt.RegisteredClaims
	AgentID  string          `json:"agent_id"`
	OrgID    uuid.UUID       `json:"org_id"`
	Role     model.AgentRole `json:"role"`
	APIKeyID *uuid.UUID      `json:"api_key_id,omitempty"` // Set when authenticated via a managed API key.
	ScopedBy string          `json:"scoped_by,omitempty"`  // Set when issued via POST /auth/scoped-token; contains the issuing admin's agent_id.
}

// MaxScopedTokenTTL is the maximum lifetime of a scoped token.
const MaxScopedTokenTTL = time.Hour

// JWTManager handles JWT creation and validation using Ed25519.
type JWTManager struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	expiration time.Duration
}

// NewJWTManager creates a JWTManager from PEM key files.
// If paths are empty, generates an ephemeral key pair (for development).
func NewJWTManager(privateKeyPath, publicKeyPath string, expiration time.Duration) (*JWTManager, error) {
	if privateKeyPath == "" || publicKeyPath == "" {
		slog.Warn("auth: no JWT key files configured, generating ephemeral key pair (not for production)")
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("auth: generate key pair: %w", err)
		}
		return &JWTManager{privateKey: priv, publicKey: pub, expiration: expiration}, nil
	}

	privPEM, err := os.ReadFile(privateKeyPath) //nolint:gosec // paths come from validated config, not user input
	if err != nil {
		return nil, fmt.Errorf("auth: read private key: %w", err)
	}
	block, _ := pem.Decode(privPEM)
	if block == nil {
		return nil, fmt.Errorf("auth: decode private key PEM")
	}
	privKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("auth: parse private key: %w", err)
	}
	edPriv, ok := privKey.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("auth: private key is not Ed25519")
	}

	pubPEM, err := os.ReadFile(publicKeyPath) //nolint:gosec // paths come from validated config, not user input
	if err != nil {
		return nil, fmt.Errorf("auth: read public key: %w", err)
	}
	pubBlock, _ := pem.Decode(pubPEM)
	if pubBlock == nil {
		return nil, fmt.Errorf("auth: decode public key PEM")
	}
	pubKey, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("auth: parse public key: %w", err)
	}
	edPub, ok := pubKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("auth: public key is not Ed25519")
	}

	// Verify the public key matches the private key to catch misconfiguration
	// (e.g., deploying a private key from one environment with a public key from another).
	derivedPub := edPriv.Public().(ed25519.PublicKey)
	if !bytes.Equal(derivedPub, edPub) {
		return nil, fmt.Errorf("auth: public key does not match private key")
	}

	return &JWTManager{privateKey: edPriv, publicKey: edPub, expiration: expiration}, nil
}

// IssueToken creates a signed JWT for the given agent.
func (m *JWTManager) IssueToken(agent model.Agent) (string, time.Time, error) {
	now := time.Now().UTC()
	exp := now.Add(m.expiration)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   agent.ID.String(),
			Issuer:    "akashi",
			Audience:  jwt.ClaimStrings{"akashi"},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        uuid.New().String(),
		},
		AgentID: agent.AgentID,
		OrgID:   agent.OrgID,
		Role:    agent.Role,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	signed, err := token.SignedString(m.privateKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign token: %w", err)
	}
	return signed, exp, nil
}

// IssueScopedToken issues a short-lived token that acts as targetAgent but
// carries the issuing admin's agent_id in the ScopedBy claim. TTL is capped
// at MaxScopedTokenTTL regardless of the requested value.
func (m *JWTManager) IssueScopedToken(issuingAdminAgentID string, target model.Agent, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 || ttl > MaxScopedTokenTTL {
		ttl = MaxScopedTokenTTL
	}

	now := time.Now().UTC()
	exp := now.Add(ttl)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   target.ID.String(),
			Issuer:    "akashi",
			Audience:  jwt.ClaimStrings{"akashi"},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        uuid.New().String(),
		},
		AgentID:  target.AgentID,
		OrgID:    target.OrgID,
		Role:     target.Role,
		ScopedBy: issuingAdminAgentID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	signed, err := token.SignedString(m.privateKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign scoped token: %w", err)
	}
	return signed, exp, nil
}

// ValidateToken parses and validates a JWT, returning the claims.
func (m *JWTManager) ValidateToken(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(
		tokenStr,
		&Claims{},
		func(token *jwt.Token) (any, error) {
			if _, ok := token.Method.(*jwt.SigningMethodEd25519); !ok {
				return nil, fmt.Errorf("auth: unexpected signing method: %v", token.Header["alg"])
			}
			return m.publicKey, nil
		},
		jwt.WithAudience("akashi"),
	)
	if err != nil {
		return nil, fmt.Errorf("auth: validate token: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("auth: invalid token claims")
	}

	if claims.Issuer != "akashi" {
		return nil, fmt.Errorf("auth: invalid issuer: %s", claims.Issuer)
	}

	if _, err := uuid.Parse(claims.Subject); err != nil {
		return nil, fmt.Errorf("auth: invalid subject (expected UUID): %w", err)
	}

	return claims, nil
}
