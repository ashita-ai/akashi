// Package signup implements self-serve organization signup with email verification.
package signup

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// Sentinel errors returned by validation and signup logic.
var (
	ErrInvalidEmail    = errors.New("invalid email format")
	ErrWeakPassword    = errors.New("password must be at least 12 characters with uppercase, lowercase, and digit")
	ErrOrgNameRequired = errors.New("org_name is required")
)

// Service handles organization signup and email verification.
type Service struct {
	db       *storage.DB
	logger   *slog.Logger
	smtpHost string
	smtpPort int
	smtpUser string
	smtpPass string
	smtpFrom string
	baseURL  string
}

// Config holds SMTP and base URL settings for the signup service.
type Config struct {
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string
	BaseURL  string
}

// New creates a signup service.
func New(db *storage.DB, cfg Config, logger *slog.Logger) *Service {
	return &Service{
		db:       db,
		logger:   logger,
		smtpHost: cfg.SMTPHost,
		smtpPort: cfg.SMTPPort,
		smtpUser: cfg.SMTPUser,
		smtpPass: cfg.SMTPPass,
		smtpFrom: cfg.SMTPFrom,
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
	}
}

// SignupInput is the validated input for a signup request.
type SignupInput struct {
	Email    string
	Password string
	OrgName  string
}

// SignupResult is returned on successful signup.
type SignupResult struct {
	OrgID   uuid.UUID `json:"org_id"`
	AgentID string    `json:"agent_id"`
	Message string    `json:"message"`
}

// Signup creates a new organization with an owner agent and sends a verification email.
func (s *Service) Signup(ctx context.Context, input SignupInput) (SignupResult, error) {
	if err := validateEmail(input.Email); err != nil {
		return SignupResult{}, err
	}
	if err := validatePassword(input.Password); err != nil {
		return SignupResult{}, err
	}
	if strings.TrimSpace(input.OrgName) == "" {
		return SignupResult{}, ErrOrgNameRequired
	}

	slug := slugify(input.OrgName)

	org, err := s.db.CreateOrganization(ctx, model.Organization{
		Name:          input.OrgName,
		Slug:          slug,
		Plan:          "free",
		DecisionLimit: 1000,
		AgentLimit:    5,
		Email:         input.Email,
		EmailVerified: false,
	})
	if err != nil {
		return SignupResult{}, fmt.Errorf("signup: create org: %w", err)
	}

	hash, err := auth.HashAPIKey(input.Password)
	if err != nil {
		return SignupResult{}, fmt.Errorf("signup: hash password: %w", err)
	}

	agentID := "owner@" + slug
	_, err = s.db.CreateAgent(ctx, model.Agent{
		AgentID:    agentID,
		OrgID:      org.ID,
		Name:       input.OrgName + " Owner",
		Role:       model.RoleOrgOwner,
		APIKeyHash: &hash,
	})
	if err != nil {
		return SignupResult{}, fmt.Errorf("signup: create owner agent: %w", err)
	}

	token, err := generateToken(32)
	if err != nil {
		return SignupResult{}, fmt.Errorf("signup: generate token: %w", err)
	}

	if err := s.db.CreateEmailVerification(ctx, org.ID, token, time.Now().Add(24*time.Hour)); err != nil {
		return SignupResult{}, fmt.Errorf("signup: create verification: %w", err)
	}

	verifyURL := fmt.Sprintf("%s/auth/verify?token=%s", s.baseURL, token)
	if err := s.sendVerificationEmail(input.Email, verifyURL); err != nil {
		// Log but don't fail — the user can request a resend later.
		s.logger.Error("signup: send verification email failed", "error", err, "email", input.Email)
	}

	return SignupResult{
		OrgID:   org.ID,
		AgentID: agentID,
		Message: "check your email to verify your account",
	}, nil
}

// Verify validates a verification token and marks the org's email as verified.
func (s *Service) Verify(ctx context.Context, token string) error {
	return s.db.VerifyEmail(ctx, token)
}

func (s *Service) sendVerificationEmail(to, verifyURL string) error {
	if s.smtpHost == "" {
		s.logger.Info("signup: verification email (dev mode — SMTP not configured)",
			"to", to,
			"verify_url", verifyURL,
		)
		return nil
	}

	subject := "Verify your Akashi account"
	body := fmt.Sprintf(
		"Welcome to Akashi!\r\n\r\nClick the link below to verify your email:\r\n\r\n%s\r\n\r\nThis link expires in 24 hours.",
		verifyURL,
	)

	msg := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		s.smtpFrom, to, subject, body,
	)

	addr := fmt.Sprintf("%s:%d", s.smtpHost, s.smtpPort)
	return sendMailTLS(addr, s.smtpHost, s.smtpUser, s.smtpPass, s.smtpFrom, []string{to}, []byte(msg))
}

// sendMailTLS connects to an SMTP server and enforces STARTTLS before authenticating.
// This prevents credentials from being sent in plaintext over the wire.
func sendMailTLS(addr, host, user, pass, from string, recipients []string, msg []byte) error {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("smtp: dial %s: %w", addr, err)
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp: new client: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Hello("localhost"); err != nil {
		return fmt.Errorf("smtp: hello: %w", err)
	}

	// Require STARTTLS — refuse to send credentials on an unencrypted connection.
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return fmt.Errorf("smtp: server %s does not support STARTTLS, refusing to send credentials", host)
	}
	tlsCfg := &tls.Config{ServerName: host} //nolint:gosec // ServerName is set, this is safe
	if err := client.StartTLS(tlsCfg); err != nil {
		return fmt.Errorf("smtp: starttls: %w", err)
	}

	if user != "" {
		if err := client.Auth(smtp.PlainAuth("", user, pass, host)); err != nil {
			return fmt.Errorf("smtp: auth: %w", err)
		}
	}

	if err := client.Mail(from); err != nil {
		return fmt.Errorf("smtp: mail from: %w", err)
	}
	for _, rcpt := range recipients {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp: rcpt to %s: %w", rcpt, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp: data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: close data: %w", err)
	}

	return client.Quit()
}

// --- Validation helpers ---

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

func validateEmail(email string) error {
	if !emailRegex.MatchString(email) {
		return ErrInvalidEmail
	}
	return nil
}

func validatePassword(password string) error {
	if len(password) < 12 {
		return ErrWeakPassword
	}
	var hasUpper, hasLower, hasDigit bool
	for _, c := range password {
		switch {
		case unicode.IsUpper(c):
			hasUpper = true
		case unicode.IsLower(c):
			hasLower = true
		case unicode.IsDigit(c):
			hasDigit = true
		}
	}
	if !hasUpper || !hasLower || !hasDigit {
		return ErrWeakPassword
	}

	// Reject common passwords that pass character-class checks.
	lower := strings.ToLower(password)
	for _, common := range commonPasswords {
		if lower == common {
			return ErrWeakPassword
		}
	}

	// Reject passwords with excessive repetition (e.g., "Aaa111111111").
	if hasExcessiveRepetition(password) {
		return ErrWeakPassword
	}

	return nil
}

// hasExcessiveRepetition returns true if any single character makes up more
// than half the password (e.g., "Aaa111111111" is 75% '1').
func hasExcessiveRepetition(password string) bool {
	if len(password) == 0 {
		return false
	}
	counts := make(map[rune]int)
	total := 0
	for _, c := range password {
		counts[c]++
		total++
	}
	threshold := total / 2
	for _, count := range counts {
		if count > threshold {
			return true
		}
	}
	return false
}

// commonPasswords is a short list of passwords that pass 12-char + mixed-case +
// digit checks but are still trivially guessable. Checked case-insensitively.
var commonPasswords = []string{
	"password1234",
	"password123!",
	"abcdefghij12",
	"qwertyuiop12",
	"admin1234567",
	"changeme1234",
	"welcome12345",
	"letmein12345",
	"iloveyou1234",
	"trustno1trust",
	"abc123456789",
	"password12345",
	"administrator1",
	"qwerty1234567",
}

var multiHyphen = regexp.MustCompile(`-{2,}`)

func slugify(name string) string {
	s := strings.ToLower(name)
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' {
			return r
		}
		if r == ' ' || r == '_' {
			return '-'
		}
		return -1
	}, s)
	s = multiHyphen.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func generateToken(length int) (string, error) {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
