package auth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
)

func TestHashAndVerifyAPIKey(t *testing.T) {
	hash, err := auth.HashAPIKey("test-key-123")
	require.NoError(t, err)
	assert.NotEmpty(t, hash)

	valid, err := auth.VerifyAPIKey("test-key-123", hash)
	require.NoError(t, err)
	assert.True(t, valid)

	valid, err = auth.VerifyAPIKey("wrong-key", hash)
	require.NoError(t, err)
	assert.False(t, valid)
}

func TestJWTIssueAndValidate(t *testing.T) {
	mgr, err := auth.NewJWTManager("", "", 1*time.Hour)
	require.NoError(t, err)

	agent := model.Agent{
		AgentID: "test-agent",
		Name:    "Test",
		Role:    model.RoleAgent,
	}
	agent.ID = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	token, expiresAt, err := mgr.IssueToken(agent)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.True(t, expiresAt.After(time.Now()))

	claims, err := mgr.ValidateToken(token)
	require.NoError(t, err)
	assert.Equal(t, "test-agent", claims.AgentID)
	assert.Equal(t, model.RoleAgent, claims.Role)
}

// newTestJWTManagerWithKey creates a JWTManager backed by a real Ed25519 key pair
// written to temp PEM files, and returns the raw private key for forging tokens.
func newTestJWTManagerWithKey(t *testing.T) (*auth.JWTManager, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	dir := t.TempDir()

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	privPath := filepath.Join(dir, "priv.pem")
	require.NoError(t, os.WriteFile(privPath, privPEM, 0600))

	pubBytes, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	pubPath := filepath.Join(dir, "pub.pem")
	require.NoError(t, os.WriteFile(pubPath, pubPEM, 0600))

	mgr, err := auth.NewJWTManager(privPath, pubPath, time.Hour)
	require.NoError(t, err)
	return mgr, priv
}

// forgeToken signs a JWT with the given private key and claims.
func forgeToken(t *testing.T, privKey ed25519.PrivateKey, claims jwt.Claims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	signed, err := token.SignedString(privKey)
	require.NoError(t, err)
	return signed
}

func TestValidateToken_WrongIssuer(t *testing.T) {
	mgr, privKey := newTestJWTManagerWithKey(t)

	now := time.Now().UTC()
	token := forgeToken(t, privKey, &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.New().String(),
			Issuer:    "not-akashi",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			ID:        uuid.New().String(),
		},
		AgentID: "test-agent",
		Role:    model.RoleAgent,
	})

	_, err := mgr.ValidateToken(token)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid issuer")
}

func TestValidateToken_EmptyIssuer(t *testing.T) {
	mgr, privKey := newTestJWTManagerWithKey(t)

	now := time.Now().UTC()
	token := forgeToken(t, privKey, &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.New().String(),
			Issuer:    "",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			ID:        uuid.New().String(),
		},
		AgentID: "test-agent",
		Role:    model.RoleAgent,
	})

	_, err := mgr.ValidateToken(token)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid issuer")
}

func TestIssueScopedToken(t *testing.T) {
	mgr, err := auth.NewJWTManager("", "", 24*time.Hour)
	require.NoError(t, err)

	admin := model.Agent{
		ID:      uuid.New(),
		AgentID: "admin",
		OrgID:   uuid.New(),
		Role:    model.RoleAdmin,
	}
	target := model.Agent{
		ID:      uuid.New(),
		AgentID: "reviewer",
		OrgID:   admin.OrgID,
		Role:    model.RoleReader,
	}

	t.Run("claims carry target identity and scoped_by", func(t *testing.T) {
		token, expiresAt, err := mgr.IssueScopedToken(admin.AgentID, target, 5*time.Minute)
		require.NoError(t, err)
		assert.NotEmpty(t, token)
		assert.True(t, expiresAt.After(time.Now()))
		assert.True(t, expiresAt.Before(time.Now().Add(6*time.Minute)))

		claims, err := mgr.ValidateToken(token)
		require.NoError(t, err)
		assert.Equal(t, "reviewer", claims.AgentID)
		assert.Equal(t, model.RoleReader, claims.Role)
		assert.Equal(t, target.OrgID, claims.OrgID)
		assert.Equal(t, "admin", claims.ScopedBy)
	})

	t.Run("TTL is capped at MaxScopedTokenTTL", func(t *testing.T) {
		token, expiresAt, err := mgr.IssueScopedToken(admin.AgentID, target, 48*time.Hour)
		require.NoError(t, err)
		assert.NotEmpty(t, token)
		// Should expire within MaxScopedTokenTTL, not 48 hours.
		assert.True(t, expiresAt.Before(time.Now().Add(auth.MaxScopedTokenTTL+time.Minute)),
			"expiry should be capped at MaxScopedTokenTTL")
	})

	t.Run("zero TTL defaults to MaxScopedTokenTTL", func(t *testing.T) {
		token, expiresAt, err := mgr.IssueScopedToken(admin.AgentID, target, 0)
		require.NoError(t, err)
		assert.NotEmpty(t, token)
		assert.True(t, expiresAt.After(time.Now()))
	})

	t.Run("token is valid and passes ValidateToken", func(t *testing.T) {
		token, _, err := mgr.IssueScopedToken(admin.AgentID, target, 5*time.Minute)
		require.NoError(t, err)
		claims, err := mgr.ValidateToken(token)
		require.NoError(t, err)
		assert.Equal(t, target.ID.String(), claims.Subject)
		assert.Equal(t, "akashi", claims.Issuer)
	})
}

func TestValidateToken_MalformedSubject(t *testing.T) {
	mgr, privKey := newTestJWTManagerWithKey(t)

	now := time.Now().UTC()
	token := forgeToken(t, privKey, &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "not-a-uuid",
			Issuer:    "akashi",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			ID:        uuid.New().String(),
		},
		AgentID: "test-agent",
		Role:    model.RoleAgent,
	})

	_, err := mgr.ValidateToken(token)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid subject")
}
