# Spec 05d: Signup + Email Verification

**Status**: Ready for implementation
**Phase**: 4 of 5 (Multi-Tenancy)
**Depends on**: Phase 3 (05c — handlers wired with org_id)
**Blocks**: Phase 5 (05e — billing needs org creation flow)

## Goal

Implement self-serve signup: a user provides email, password, and org name; the system creates an organization and org_owner agent, sends a verification email, and gates token issuance on email verification.

## Deliverables

1. `internal/signup/signup.go` — signup service (org creation, verification tokens, email sending)
2. `POST /auth/signup` handler
3. `GET /auth/verify` handler
4. SMTP configuration in `internal/config/config.go`
5. Integration with existing auth flow (reject unverified orgs)
6. Tests for the signup flow

---

## 1. Configuration

### `internal/config/config.go`

Add SMTP fields to `Config`:

```go
type Config struct {
    // ... existing fields ...

    // SMTP settings for email verification.
    SMTPHost     string
    SMTPPort     int
    SMTPUser     string
    SMTPPassword string
    SMTPFrom     string
    BaseURL      string // e.g., "https://akashi.example.com" for verification links
}
```

In `Load()`:

```go
SMTPHost:     envStr("AKASHI_SMTP_HOST", ""),
SMTPPort:     envInt("AKASHI_SMTP_PORT", 587),
SMTPUser:     envStr("AKASHI_SMTP_USER", ""),
SMTPPassword: envStr("AKASHI_SMTP_PASSWORD", ""),
SMTPFrom:     envStr("AKASHI_SMTP_FROM", "noreply@akashi.dev"),
BaseURL:      envStr("AKASHI_BASE_URL", "http://localhost:8080"),
```

SMTP is optional. If `SMTPHost` is empty, verification emails are logged instead of sent (development mode).

---

## 2. Signup Service

### `internal/signup/signup.go`

```go
package signup

import (
    "context"
    "crypto/rand"
    "encoding/hex"
    "fmt"
    "log/slog"
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

type Config struct {
    SMTPHost string
    SMTPPort int
    SMTPUser string
    SMTPPass string
    SMTPFrom string
    BaseURL  string
}

func New(db *storage.DB, cfg Config, logger *slog.Logger) *Service {
    return &Service{
        db:       db,
        logger:   logger,
        smtpHost: cfg.SMTPHost,
        smtpPort: cfg.SMTPPort,
        smtpUser: cfg.SMTPUser,
        smtpPass: cfg.SMTPPass,
        smtpFrom: cfg.SMTPFrom,
        baseURL:  cfg.BaseURL,
    }
}

type SignupInput struct {
    Email   string
    Password string
    OrgName  string
}

type SignupResult struct {
    OrgID   uuid.UUID
    AgentID string
    Message string
}

func (s *Service) Signup(ctx context.Context, input SignupInput) (SignupResult, error) {
    // 1. Validate inputs.
    if err := validateEmail(input.Email); err != nil {
        return SignupResult{}, fmt.Errorf("signup: %w", err)
    }
    if err := validatePassword(input.Password); err != nil {
        return SignupResult{}, fmt.Errorf("signup: %w", err)
    }
    if input.OrgName == "" {
        return SignupResult{}, fmt.Errorf("signup: org_name is required")
    }

    // 2. Generate slug from org name.
    slug := slugify(input.OrgName)

    // 3. Create organization.
    org, err := s.db.CreateOrganization(ctx, model.Organization{
        Name:          input.OrgName,
        Slug:          slug,
        Plan:          "free",
        DecisionLimit: 1000,
        AgentLimit:    1,
        Email:         input.Email,
        EmailVerified: false,
    })
    if err != nil {
        return SignupResult{}, fmt.Errorf("signup: create org: %w", err)
    }

    // 4. Hash password as API key.
    hash, err := auth.HashAPIKey(input.Password)
    if err != nil {
        return SignupResult{}, fmt.Errorf("signup: hash password: %w", err)
    }

    // 5. Create org_owner agent.
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

    // 6. Generate verification token.
    token, err := generateToken(32)
    if err != nil {
        return SignupResult{}, fmt.Errorf("signup: generate token: %w", err)
    }

    err = s.db.CreateEmailVerification(ctx, org.ID, token, time.Now().Add(24*time.Hour))
    if err != nil {
        return SignupResult{}, fmt.Errorf("signup: create verification: %w", err)
    }

    // 7. Send verification email (or log in dev mode).
    verifyURL := fmt.Sprintf("%s/auth/verify?token=%s", s.baseURL, token)
    if err := s.sendVerificationEmail(input.Email, verifyURL); err != nil {
        s.logger.Error("signup: send verification email failed", "error", err, "email", input.Email)
        // Don't fail the signup — the user can request a resend later.
    }

    return SignupResult{
        OrgID:   org.ID,
        AgentID: agentID,
        Message: "check your email to verify your account",
    }, nil
}

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
        "Welcome to Akashi!\n\nClick the link below to verify your email:\n\n%s\n\nThis link expires in 24 hours.",
        verifyURL,
    )

    msg := fmt.Sprintf(
        "From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
        s.smtpFrom, to, subject, body,
    )

    addr := fmt.Sprintf("%s:%d", s.smtpHost, s.smtpPort)
    var smtpAuth smtp.Auth
    if s.smtpUser != "" {
        smtpAuth = smtp.PlainAuth("", s.smtpUser, s.smtpPass, s.smtpHost)
    }

    return smtp.SendMail(addr, smtpAuth, s.smtpFrom, []string{to}, []byte(msg))
}

// --- Validation helpers ---

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

func validateEmail(email string) error {
    if !emailRegex.MatchString(email) {
        return fmt.Errorf("invalid email format")
    }
    return nil
}

func validatePassword(password string) error {
    if len(password) < 12 {
        return fmt.Errorf("password must be at least 12 characters")
    }
    hasUpper, hasLower, hasDigit := false, false, false
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
        return fmt.Errorf("password must contain uppercase, lowercase, and digit")
    }
    return nil
}

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
    // Collapse multiple hyphens.
    for strings.Contains(s, "--") {
        s = strings.ReplaceAll(s, "--", "-")
    }
    return strings.Trim(s, "-")
}

func generateToken(length int) (string, error) {
    b := make([]byte, length)
    if _, err := rand.Read(b); err != nil {
        return "", err
    }
    return hex.EncodeToString(b), nil
}
```

---

## 3. Storage: Email Verification

### `internal/storage/organizations.go` (additions)

```go
// CreateEmailVerification inserts a verification token for an org.
func (db *DB) CreateEmailVerification(ctx context.Context, orgID uuid.UUID, token string, expiresAt time.Time) error {
    _, err := db.pool.Exec(ctx,
        `INSERT INTO email_verifications (org_id, token, expires_at) VALUES ($1, $2, $3)`,
        orgID, token, expiresAt,
    )
    if err != nil {
        return fmt.Errorf("storage: create email verification: %w", err)
    }
    return nil
}

// VerifyEmail marks a verification token as used and sets the org's email as verified.
// Returns an error if the token is invalid, expired, or already used.
func (db *DB) VerifyEmail(ctx context.Context, token string) error {
    tx, err := db.pool.Begin(ctx)
    if err != nil {
        return fmt.Errorf("storage: begin verify tx: %w", err)
    }
    defer func() { _ = tx.Rollback(ctx) }()

    var orgID uuid.UUID
    var expiresAt time.Time
    var usedAt *time.Time
    err = tx.QueryRow(ctx,
        `SELECT org_id, expires_at, used_at FROM email_verifications WHERE token = $1`,
        token,
    ).Scan(&orgID, &expiresAt, &usedAt)
    if err != nil {
        return fmt.Errorf("storage: verification token not found")
    }

    if usedAt != nil {
        return fmt.Errorf("storage: verification token already used")
    }
    if time.Now().After(expiresAt) {
        return fmt.Errorf("storage: verification token expired")
    }

    now := time.Now().UTC()
    if _, err := tx.Exec(ctx,
        `UPDATE email_verifications SET used_at = $1 WHERE token = $2`,
        now, token,
    ); err != nil {
        return fmt.Errorf("storage: mark verification used: %w", err)
    }

    if _, err := tx.Exec(ctx,
        `UPDATE organizations SET email_verified = true, updated_at = $1 WHERE id = $2`,
        now, orgID,
    ); err != nil {
        return fmt.Errorf("storage: verify org email: %w", err)
    }

    return tx.Commit(ctx)
}
```

---

## 4. HTTP Handlers

### `internal/server/handlers.go` (new handlers)

**HandleSignup** — `POST /auth/signup`:

```go
func (h *Handlers) HandleSignup(w http.ResponseWriter, r *http.Request) {
    var req model.SignupRequest
    if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
        writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
        return
    }

    result, err := h.signupSvc.Signup(r.Context(), signup.SignupInput{
        Email:    req.Email,
        Password: req.Password,
        OrgName:  req.OrgName,
    })
    if err != nil {
        h.logger.Error("signup failed", "error", err, "email", req.Email)
        // Return user-friendly messages for known validation errors.
        writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
        return
    }

    writeJSON(w, r, http.StatusCreated, result)
}
```

**HandleVerifyEmail** — `GET /auth/verify`:

```go
func (h *Handlers) HandleVerifyEmail(w http.ResponseWriter, r *http.Request) {
    token := r.URL.Query().Get("token")
    if token == "" {
        writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "token is required")
        return
    }

    if err := h.signupSvc.Verify(r.Context(), token); err != nil {
        h.logger.Error("email verification failed", "error", err)
        writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
        return
    }

    writeJSON(w, r, http.StatusOK, map[string]string{
        "status":  "verified",
        "message": "email verified successfully, you can now authenticate",
    })
}
```

### Model Types

Add to `internal/model/api.go`:

```go
type SignupRequest struct {
    Email    string `json:"email"`
    Password string `json:"password"`
    OrgName  string `json:"org_name"`
}
```

### Route Registration

In `internal/server/server.go`:

```go
// Signup endpoints (no auth required, rate limited by IP).
mux.Handle("POST /auth/signup", authRL(http.HandlerFunc(h.HandleSignup)))
mux.Handle("GET /auth/verify", http.HandlerFunc(h.HandleVerifyEmail))
```

Add signup route to auth middleware skip list:

```go
if r.URL.Path == "/health" || r.URL.Path == "/auth/token" ||
   r.URL.Path == "/auth/signup" || r.URL.Path == "/auth/verify" {
    next.ServeHTTP(w, r)
    return
}
```

### Handlers Struct

Add `signupSvc` to `Handlers`:

```go
type Handlers struct {
    // ... existing fields ...
    signupSvc *signup.Service
}
```

Update `NewHandlers` and `New` (server constructor) to accept and wire the signup service.

### Main Wiring

In `cmd/akashi/main.go`, create the signup service and pass it to the server:

```go
signupSvc := signup.New(db, signup.Config{
    SMTPHost: cfg.SMTPHost,
    SMTPPort: cfg.SMTPPort,
    SMTPUser: cfg.SMTPUser,
    SMTPPass: cfg.SMTPPassword,
    SMTPFrom: cfg.SMTPFrom,
    BaseURL:  cfg.BaseURL,
}, logger)
```

---

## 5. Auth Flow Change

In `HandleAuthToken` (already modified in Phase 3), the email verification check rejects unverified non-default orgs. No additional changes needed beyond what Phase 3 specifies.

---

## Files Changed

| File | Action |
|------|--------|
| `internal/signup/signup.go` | **Create** — signup service |
| `internal/storage/organizations.go` | Modify — add email verification methods |
| `internal/model/api.go` | Modify — add SignupRequest |
| `internal/config/config.go` | Modify — add SMTP + BaseURL config |
| `internal/server/handlers.go` | Modify — add HandleSignup, HandleVerifyEmail |
| `internal/server/server.go` | Modify — add routes, wire signup service |
| `internal/server/middleware.go` | Modify — skip auth for signup/verify paths |
| `cmd/akashi/main.go` | Modify — wire signup service |

## Success Criteria

1. `POST /auth/signup` with valid input creates org + agent + verification token
2. `POST /auth/signup` with bad email/password returns 400 with specific message
3. `GET /auth/verify?token=<valid>` sets `email_verified = true`
4. `GET /auth/verify?token=<expired>` returns 400
5. `POST /auth/token` for unverified org returns 403
6. `POST /auth/token` for verified org returns JWT with `org_id`
7. Dev mode (no SMTP): verification URL is logged
8. All existing tests pass
