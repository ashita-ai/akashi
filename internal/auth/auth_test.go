package auth_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
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
	// Include correct audience so aud check passes and issuer check fires.
	token := forgeToken(t, privKey, &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.New().String(),
			Issuer:    "not-akashi",
			Audience:  jwt.ClaimStrings{"akashi"},
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
	// Include correct audience so aud check passes and issuer check fires.
	token := forgeToken(t, privKey, &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.New().String(),
			Issuer:    "",
			Audience:  jwt.ClaimStrings{"akashi"},
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

func TestValidateToken_WrongAudience(t *testing.T) {
	mgr, privKey := newTestJWTManagerWithKey(t)

	now := time.Now().UTC()
	token := forgeToken(t, privKey, &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.New().String(),
			Issuer:    "akashi",
			Audience:  jwt.ClaimStrings{"other-service"},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			ID:        uuid.New().String(),
		},
		AgentID: "test-agent",
		Role:    model.RoleAgent,
	})

	_, err := mgr.ValidateToken(token)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aud")
}

func TestValidateToken_MissingAudience(t *testing.T) {
	mgr, privKey := newTestJWTManagerWithKey(t)

	now := time.Now().UTC()
	// No Audience field — simulates pre-M3 tokens or tokens from another service.
	token := forgeToken(t, privKey, &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.New().String(),
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
	assert.Contains(t, err.Error(), "aud")
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

// ---------- NewJWTManager error path tests ----------

func TestNewJWTManager_PrivateKeyFileNotFound(t *testing.T) {
	_, err := auth.NewJWTManager("/nonexistent/priv.pem", "/nonexistent/pub.pem", time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read private key")
}

func TestNewJWTManager_PublicKeyFileNotFound(t *testing.T) {
	// Write a valid private key, but point to a missing public key file.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_ = pub

	dir := t.TempDir()
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	privPath := filepath.Join(dir, "priv.pem")
	require.NoError(t, os.WriteFile(privPath, privPEM, 0600))

	_, err = auth.NewJWTManager(privPath, "/nonexistent/pub.pem", time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read public key")
}

func TestNewJWTManager_InvalidPrivateKeyPEM(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "priv.pem")
	pubPath := filepath.Join(dir, "pub.pem")
	require.NoError(t, os.WriteFile(privPath, []byte("not a pem block"), 0600))
	require.NoError(t, os.WriteFile(pubPath, []byte("not a pem block"), 0600))

	_, err := auth.NewJWTManager(privPath, pubPath, time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode private key PEM")
}

func TestNewJWTManager_InvalidPublicKeyPEM(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_ = pub

	dir := t.TempDir()
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	privPath := filepath.Join(dir, "priv.pem")
	require.NoError(t, os.WriteFile(privPath, privPEM, 0600))

	pubPath := filepath.Join(dir, "pub.pem")
	require.NoError(t, os.WriteFile(pubPath, []byte("not a pem block"), 0600))

	_, err = auth.NewJWTManager(privPath, pubPath, time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode public key PEM")
}

func TestNewJWTManager_MismatchedKeys(t *testing.T) {
	// Generate two independent key pairs and swap the public key.
	_, priv1, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pub2, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	dir := t.TempDir()

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv1)
	require.NoError(t, err)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	privPath := filepath.Join(dir, "priv.pem")
	require.NoError(t, os.WriteFile(privPath, privPEM, 0600))

	pubBytes, err := x509.MarshalPKIXPublicKey(pub2)
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	pubPath := filepath.Join(dir, "pub.pem")
	require.NoError(t, os.WriteFile(pubPath, pubPEM, 0600))

	_, err = auth.NewJWTManager(privPath, pubPath, time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "public key does not match private key")
}

func TestNewJWTManager_NonEd25519PrivateKey(t *testing.T) {
	// Write an ECDSA key in PKCS8 format — should be rejected as "not Ed25519".
	dir := t.TempDir()

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ecKeyBytes, err := x509.MarshalPKCS8PrivateKey(ecKey)
	require.NoError(t, err)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecKeyBytes})
	privPath := filepath.Join(dir, "priv.pem")
	require.NoError(t, os.WriteFile(privPath, privPEM, 0600))

	pubPath := filepath.Join(dir, "pub.pem")
	require.NoError(t, os.WriteFile(pubPath, []byte("placeholder"), 0600))

	_, err = auth.NewJWTManager(privPath, pubPath, time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not Ed25519")
}

func TestNewJWTManager_InvalidPrivateKeyDER(t *testing.T) {
	// Valid PEM block but garbage DER content inside it.
	dir := t.TempDir()
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage der bytes")})
	privPath := filepath.Join(dir, "priv.pem")
	require.NoError(t, os.WriteFile(privPath, privPEM, 0600))

	pubPath := filepath.Join(dir, "pub.pem")
	require.NoError(t, os.WriteFile(pubPath, []byte("placeholder"), 0600))

	_, err := auth.NewJWTManager(privPath, pubPath, time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse private key")
}

func TestNewJWTManager_NonEd25519PublicKey(t *testing.T) {
	// Valid Ed25519 private key but ECDSA public key — should fail key match.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	dir := t.TempDir()
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	privPath := filepath.Join(dir, "priv.pem")
	require.NoError(t, os.WriteFile(privPath, privPEM, 0600))

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ecPubBytes, err := x509.MarshalPKIXPublicKey(&ecKey.PublicKey)
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: ecPubBytes})
	pubPath := filepath.Join(dir, "pub.pem")
	require.NoError(t, os.WriteFile(pubPath, pubPEM, 0600))

	_, err = auth.NewJWTManager(privPath, pubPath, time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not Ed25519")
}

func TestNewJWTManager_InvalidPublicKeyDER(t *testing.T) {
	// Valid Ed25519 private key but garbage DER content in the public key PEM.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	dir := t.TempDir()
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	privPath := filepath.Join(dir, "priv.pem")
	require.NoError(t, os.WriteFile(privPath, privPEM, 0600))

	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("garbage der bytes")})
	pubPath := filepath.Join(dir, "pub.pem")
	require.NoError(t, os.WriteFile(pubPath, pubPEM, 0600))

	_, err = auth.NewJWTManager(privPath, pubPath, time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse public key")
}

// ---------- VerifyAPIKey error path tests ----------

func TestVerifyAPIKey_InvalidHashFormat(t *testing.T) {
	_, err := auth.VerifyAPIKey("some-key", "invalid-no-dollar-sign")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid hash format")
}

func TestVerifyAPIKey_InvalidBase64Salt(t *testing.T) {
	_, err := auth.VerifyAPIKey("some-key", "!!!invalid-base64$validhash")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode salt")
}

func TestVerifyAPIKey_InvalidBase64Hash(t *testing.T) {
	// Valid base64 salt but invalid base64 hash.
	_, err := auth.VerifyAPIKey("some-key", "dGVzdA==$!!!invalid-base64")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode hash")
}

func TestDummyVerify(t *testing.T) {
	// DummyVerify should not panic. It has no return value; we just verify
	// it completes without error. This exists for timing-attack resistance.
	auth.DummyVerify()
}

// ---------- ValidateToken: expired token ----------

func TestValidateToken_ExpiredToken(t *testing.T) {
	mgr, err := auth.NewJWTManager("", "", 1*time.Nanosecond)
	require.NoError(t, err)

	agent := model.Agent{
		ID:      uuid.New(),
		AgentID: "expiring-agent",
		Name:    "Expiring",
		Role:    model.RoleAgent,
	}

	token, _, err := mgr.IssueToken(agent)
	require.NoError(t, err)

	// Token is effectively already expired since TTL was 1ns.
	_, err = mgr.ValidateToken(token)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token")
}

// ---------- ValidateToken: completely garbage input ----------

func TestValidateToken_GarbageInput(t *testing.T) {
	mgr, err := auth.NewJWTManager("", "", time.Hour)
	require.NoError(t, err)

	_, err = mgr.ValidateToken("totally.not.a.jwt")
	require.Error(t, err)
}

// ---------- IssueScopedToken: negative TTL defaults ----------

func TestIssueScopedToken_NegativeTTL(t *testing.T) {
	mgr, err := auth.NewJWTManager("", "", time.Hour)
	require.NoError(t, err)

	target := model.Agent{
		ID:      uuid.New(),
		AgentID: "target-agent",
		OrgID:   uuid.New(),
		Role:    model.RoleAgent,
	}

	token, expiresAt, err := mgr.IssueScopedToken("admin", target, -5*time.Minute)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	// Negative TTL should default to MaxScopedTokenTTL.
	assert.True(t, expiresAt.Before(time.Now().Add(auth.MaxScopedTokenTTL+time.Minute)),
		"negative TTL should default to MaxScopedTokenTTL")
}

func TestValidateToken_MalformedSubject(t *testing.T) {
	mgr, privKey := newTestJWTManagerWithKey(t)

	now := time.Now().UTC()
	// Correct audience and issuer so that the subject check fires.
	token := forgeToken(t, privKey, &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "not-a-uuid",
			Issuer:    "akashi",
			Audience:  jwt.ClaimStrings{"akashi"},
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
