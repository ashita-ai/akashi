// genkey generates an Ed25519 key pair for Akashi JWT signing.
//
// Usage (run from the repo root):
//
//	go run scripts/genkey/main.go
//
// Writes:
//
//	data/jwt_private.pem  (mode 0600 — keep this secret)
//	data/jwt_public.pem   (mode 0600)
//
// These paths match the defaults wired in docker-compose.yml. The data/
// directory is gitignored. Run once before first launch; keys persist across
// container rebuilds and restarts.
//
// The server auto-generates ephemeral keys when AKASHI_JWT_PRIVATE_KEY is
// unset, but those are discarded on every restart, invalidating all existing
// tokens and browser sessions. Persistent keys prevent that.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	dir := "data"
	privPath := filepath.Join(dir, "jwt_private.pem")
	pubPath := filepath.Join(dir, "jwt_public.pem")

	if err := os.MkdirAll(dir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create %s: %v\n", dir, err)
		os.Exit(1)
	}

	// Refuse to overwrite existing keys — prevents accidental invalidation of
	// live tokens.
	for _, path := range []string{privPath, pubPath} {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(os.Stderr, "error: %s already exists — delete it first if you want to rotate keys\n", path)
			os.Exit(1)
		}
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: generate key: %v\n", err)
		os.Exit(1)
	}

	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: marshal private key: %v\n", err)
		os.Exit(1)
	}

	privFile, err := os.OpenFile(privPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create %s: %v\n", privPath, err)
		os.Exit(1)
	}
	if err := pem.Encode(privFile, &pem.Block{Type: "PRIVATE KEY", Bytes: privDER}); err != nil {
		fmt.Fprintf(os.Stderr, "error: write private key: %v\n", err)
		os.Exit(1)
	}
	privFile.Close()

	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: marshal public key: %v\n", err)
		os.Exit(1)
	}

	pubFile, err := os.OpenFile(pubPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create %s: %v\n", pubPath, err)
		os.Exit(1)
	}
	if err := pem.Encode(pubFile, &pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}); err != nil {
		fmt.Fprintf(os.Stderr, "error: write public key: %v\n", err)
		os.Exit(1)
	}
	pubFile.Close()

	fmt.Printf("wrote %s\n", privPath)
	fmt.Printf("wrote %s\n", pubPath)
	fmt.Println("Keys are ready. docker compose up -d will use them automatically.")
}
