# Spec 05e: Billing (Stripe Integration)

**Status**: Ready for implementation
**Phase**: 5 of 5 (Multi-Tenancy)
**Depends on**: Phase 4 (05d — signup flow, org creation)

## Goal

Integrate Stripe for subscription billing. Implement usage metering (per-org decision counter) and quota enforcement (reject writes when limits exceeded). Handle Stripe webhooks for plan changes.

## Deliverables

1. `internal/billing/billing.go` — Stripe client wrapper, plan definitions
2. `internal/billing/webhooks.go` — Stripe webhook handler
3. `internal/billing/metering.go` — usage counting and quota checks
4. Billing HTTP endpoints (checkout, portal, webhooks, usage)
5. Quota enforcement in trace hot path
6. Configuration for Stripe env vars
7. `go.mod` dependency: `github.com/stripe/stripe-go/v82`
8. Tests for metering and webhook handling

---

## 1. Configuration

### `internal/config/config.go`

Add Stripe fields:

```go
type Config struct {
    // ... existing fields ...

    // Stripe billing settings.
    StripeSecretKey    string
    StripeWebhookSecret string
    StripePriceIDPro   string // Price ID for Pro plan
}
```

In `Load()`:

```go
StripeSecretKey:     envStr("STRIPE_SECRET_KEY", ""),
StripeWebhookSecret: envStr("STRIPE_WEBHOOK_SECRET", ""),
StripePriceIDPro:    envStr("STRIPE_PRO_PRICE_ID", ""),
```

Stripe is optional. If `StripeSecretKey` is empty, billing endpoints return 503 and quota enforcement is disabled.

---

## 2. Plan Definitions

### `internal/billing/billing.go`

```go
package billing

import (
    "fmt"

    "github.com/stripe/stripe-go/v82"
    "github.com/stripe/stripe-go/v82/checkout/session"
    "github.com/stripe/stripe-go/v82/billingportal/session" // alias as portalsession
    "github.com/stripe/stripe-go/v82/webhook"
)

type Plan struct {
    Name          string
    PriceID       string // Stripe Price ID (empty for free/enterprise)
    DecisionLimit int    // 0 = unlimited (set per-org for enterprise)
    AgentLimit    int    // 0 = unlimited
}

type Service struct {
    plans           map[string]Plan
    webhookSecret   string
    proPriceID      string
    enabled         bool
}

func New(secretKey, webhookSecret, proPriceID string) *Service {
    enabled := secretKey != ""
    if enabled {
        stripe.Key = secretKey
    }

    return &Service{
        plans: map[string]Plan{
            "free": {
                Name:          "Free",
                DecisionLimit: 1_000,
                AgentLimit:    1,
            },
            "pro": {
                Name:          "Pro",
                PriceID:       proPriceID,
                DecisionLimit: 50_000,
                AgentLimit:    0, // unlimited
            },
            "enterprise": {
                Name:          "Enterprise",
                DecisionLimit: 0, // custom, set per-org
                AgentLimit:    0, // custom
            },
        },
        webhookSecret: webhookSecret,
        proPriceID:    proPriceID,
        enabled:       enabled,
    }
}

func (s *Service) Enabled() bool { return s.enabled }

func (s *Service) GetPlan(name string) (Plan, bool) {
    p, ok := s.plans[name]
    return p, ok
}
```

---

## 3. Checkout and Portal

### `internal/billing/billing.go` (continued)

```go
// CreateCheckoutSession creates a Stripe Checkout session for plan upgrade.
func (s *Service) CreateCheckoutSession(orgID, orgEmail, successURL, cancelURL string) (string, error) {
    if !s.enabled {
        return "", fmt.Errorf("billing: stripe not configured")
    }

    params := &stripe.CheckoutSessionParams{
        Mode:       stripe.String(string(stripe.CheckoutSessionModeSubscription)),
        CustomerEmail: stripe.String(orgEmail),
        SuccessURL: stripe.String(successURL),
        CancelURL:  stripe.String(cancelURL),
        LineItems: []*stripe.CheckoutSessionLineItemParams{
            {
                Price:    stripe.String(s.proPriceID),
                Quantity: stripe.Int64(1),
            },
        },
        Metadata: map[string]string{
            "org_id": orgID,
        },
    }

    sess, err := session.New(params)
    if err != nil {
        return "", fmt.Errorf("billing: create checkout session: %w", err)
    }
    return sess.URL, nil
}

// CreatePortalSession creates a Stripe billing portal session for subscription management.
func (s *Service) CreatePortalSession(stripeCustomerID, returnURL string) (string, error) {
    if !s.enabled {
        return "", fmt.Errorf("billing: stripe not configured")
    }

    params := &stripe.BillingPortalSessionParams{
        Customer:  stripe.String(stripeCustomerID),
        ReturnURL: stripe.String(returnURL),
    }

    sess, err := portalsession.New(params)
    if err != nil {
        return "", fmt.Errorf("billing: create portal session: %w", err)
    }
    return sess.URL, nil
}
```

---

## 4. Webhook Handler

### `internal/billing/webhooks.go`

```go
package billing

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log/slog"
    "net/http"

    "github.com/google/uuid"
    "github.com/stripe/stripe-go/v82"
    "github.com/stripe/stripe-go/v82/webhook"

    "github.com/ashita-ai/akashi/internal/storage"
)

// HandleWebhook processes Stripe webhook events.
// Returns the HTTP status code and error message (if any).
func (s *Service) HandleWebhook(ctx context.Context, db *storage.DB, logger *slog.Logger, body []byte, sigHeader string) (int, error) {
    event, err := webhook.ConstructEvent(body, sigHeader, s.webhookSecret)
    if err != nil {
        return http.StatusBadRequest, fmt.Errorf("billing: invalid webhook signature: %w", err)
    }

    switch event.Type {
    case "checkout.session.completed":
        return s.handleCheckoutCompleted(ctx, db, logger, event)
    case "customer.subscription.updated":
        return s.handleSubscriptionUpdated(ctx, db, logger, event)
    case "customer.subscription.deleted":
        return s.handleSubscriptionDeleted(ctx, db, logger, event)
    case "invoice.payment_failed":
        return s.handlePaymentFailed(ctx, db, logger, event)
    default:
        // Ignore unknown event types.
        return http.StatusOK, nil
    }
}

func (s *Service) handleCheckoutCompleted(ctx context.Context, db *storage.DB, logger *slog.Logger, event stripe.Event) (int, error) {
    var sess stripe.CheckoutSession
    if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
        return http.StatusBadRequest, fmt.Errorf("billing: unmarshal checkout session: %w", err)
    }

    orgIDStr, ok := sess.Metadata["org_id"]
    if !ok {
        return http.StatusBadRequest, fmt.Errorf("billing: missing org_id in checkout metadata")
    }
    orgID, err := uuid.Parse(orgIDStr)
    if err != nil {
        return http.StatusBadRequest, fmt.Errorf("billing: invalid org_id: %w", err)
    }

    // Look up org and update plan.
    org, err := db.GetOrganization(ctx, orgID)
    if err != nil {
        return http.StatusInternalServerError, fmt.Errorf("billing: get org: %w", err)
    }

    // Update org with Stripe IDs and Pro plan limits.
    proPlan := s.plans["pro"]
    org.Plan = "pro"
    org.StripeCustomerID = &sess.Customer.ID
    subID := sess.Subscription.ID
    org.StripeSubscriptionID = &subID
    org.DecisionLimit = proPlan.DecisionLimit
    org.AgentLimit = proPlan.AgentLimit

    if err := db.UpdateOrganization(ctx, org); err != nil {
        return http.StatusInternalServerError, fmt.Errorf("billing: update org: %w", err)
    }

    logger.Info("billing: checkout completed, upgraded to pro",
        "org_id", orgID,
        "customer_id", sess.Customer.ID,
    )
    return http.StatusOK, nil
}

func (s *Service) handleSubscriptionUpdated(ctx context.Context, db *storage.DB, logger *slog.Logger, event stripe.Event) (int, error) {
    var sub stripe.Subscription
    if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
        return http.StatusBadRequest, fmt.Errorf("billing: unmarshal subscription: %w", err)
    }

    // Find org by stripe_customer_id.
    org, err := db.GetOrganizationByStripeCustomer(ctx, sub.Customer.ID)
    if err != nil {
        logger.Warn("billing: subscription updated for unknown customer", "customer_id", sub.Customer.ID)
        return http.StatusOK, nil // Don't fail — might be a different product
    }

    // Determine the new plan from the price ID.
    newPlan := "free"
    for name, plan := range s.plans {
        if plan.PriceID != "" && len(sub.Items.Data) > 0 && sub.Items.Data[0].Price.ID == plan.PriceID {
            newPlan = name
            break
        }
    }

    plan := s.plans[newPlan]
    org.Plan = newPlan
    org.DecisionLimit = plan.DecisionLimit
    org.AgentLimit = plan.AgentLimit

    if err := db.UpdateOrganization(ctx, org); err != nil {
        return http.StatusInternalServerError, fmt.Errorf("billing: update org plan: %w", err)
    }

    logger.Info("billing: subscription updated",
        "org_id", org.ID,
        "plan", newPlan,
    )
    return http.StatusOK, nil
}

func (s *Service) handleSubscriptionDeleted(ctx context.Context, db *storage.DB, logger *slog.Logger, event stripe.Event) (int, error) {
    var sub stripe.Subscription
    if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
        return http.StatusBadRequest, fmt.Errorf("billing: unmarshal subscription: %w", err)
    }

    org, err := db.GetOrganizationByStripeCustomer(ctx, sub.Customer.ID)
    if err != nil {
        logger.Warn("billing: subscription deleted for unknown customer", "customer_id", sub.Customer.ID)
        return http.StatusOK, nil
    }

    // Downgrade to free.
    freePlan := s.plans["free"]
    org.Plan = "free"
    org.DecisionLimit = freePlan.DecisionLimit
    org.AgentLimit = freePlan.AgentLimit
    org.StripeSubscriptionID = nil

    if err := db.UpdateOrganization(ctx, org); err != nil {
        return http.StatusInternalServerError, fmt.Errorf("billing: downgrade org: %w", err)
    }

    logger.Info("billing: subscription deleted, downgraded to free",
        "org_id", org.ID,
    )
    return http.StatusOK, nil
}

func (s *Service) handlePaymentFailed(ctx context.Context, db *storage.DB, logger *slog.Logger, event stripe.Event) (int, error) {
    var invoice stripe.Invoice
    if err := json.Unmarshal(event.Data.Raw, &invoice); err != nil {
        return http.StatusBadRequest, fmt.Errorf("billing: unmarshal invoice: %w", err)
    }

    logger.Warn("billing: payment failed",
        "customer_id", invoice.Customer.ID,
        "amount_due", invoice.AmountDue,
        "attempt_count", invoice.AttemptCount,
    )

    // Future: implement grace period, send notification
    return http.StatusOK, nil
}
```

---

## 5. Metering + Quota Enforcement

### `internal/billing/metering.go`

```go
package billing

import (
    "context"
    "fmt"
    "time"

    "github.com/google/uuid"

    "github.com/ashita-ai/akashi/internal/model"
    "github.com/ashita-ai/akashi/internal/storage"
)

// CurrentPeriod returns the current billing period string (YYYY-MM).
func CurrentPeriod() string {
    return time.Now().UTC().Format("2006-01")
}

// CheckDecisionQuota checks if an org has exceeded its monthly decision limit.
// Returns nil if allowed, or an error describing the quota violation.
func (s *Service) CheckDecisionQuota(ctx context.Context, db *storage.DB, orgID uuid.UUID) error {
    if !s.enabled {
        return nil // No billing = no quota
    }

    org, err := db.GetOrganization(ctx, orgID)
    if err != nil {
        return fmt.Errorf("billing: get org for quota check: %w", err)
    }

    // Enterprise and unlimited orgs (decision_limit = 0) skip the check.
    if org.DecisionLimit == 0 || org.DecisionLimit >= 2147483647 {
        return nil
    }

    usage, err := db.GetUsage(ctx, orgID, CurrentPeriod())
    if err != nil {
        // No usage record = 0 decisions this period.
        return nil
    }

    if usage.DecisionCount >= org.DecisionLimit {
        return fmt.Errorf("quota exceeded: %d/%d decisions this period", usage.DecisionCount, org.DecisionLimit)
    }
    return nil
}

// CheckAgentQuota checks if an org has exceeded its agent limit.
func (s *Service) CheckAgentQuota(ctx context.Context, db *storage.DB, orgID uuid.UUID) error {
    if !s.enabled {
        return nil
    }

    org, err := db.GetOrganization(ctx, orgID)
    if err != nil {
        return fmt.Errorf("billing: get org for agent quota: %w", err)
    }

    if org.AgentLimit == 0 || org.AgentLimit >= 2147483647 {
        return nil
    }

    count, err := db.CountAgents(ctx, orgID)
    if err != nil {
        return fmt.Errorf("billing: count agents: %w", err)
    }

    if count >= org.AgentLimit {
        return fmt.Errorf("agent limit exceeded: %d/%d agents", count, org.AgentLimit)
    }
    return nil
}

// IncrementDecisionCount atomically increments the usage counter after a successful trace.
func (s *Service) IncrementDecisionCount(ctx context.Context, db *storage.DB, orgID uuid.UUID) error {
    _, err := db.IncrementUsage(ctx, orgID, CurrentPeriod())
    return err
}
```

---

## 6. Storage: Stripe Customer Lookup

### `internal/storage/organizations.go` (additions)

```go
// GetOrganizationByStripeCustomer retrieves an org by its Stripe customer ID.
func (db *DB) GetOrganizationByStripeCustomer(ctx context.Context, customerID string) (model.Organization, error) {
    var org model.Organization
    err := db.pool.QueryRow(ctx,
        `SELECT id, name, slug, plan, stripe_customer_id, stripe_subscription_id,
         decision_limit, agent_limit, email, email_verified, created_at, updated_at
         FROM organizations WHERE stripe_customer_id = $1`,
        customerID,
    ).Scan(
        &org.ID, &org.Name, &org.Slug, &org.Plan,
        &org.StripeCustomerID, &org.StripeSubscriptionID,
        &org.DecisionLimit, &org.AgentLimit,
        &org.Email, &org.EmailVerified,
        &org.CreatedAt, &org.UpdatedAt,
    )
    if err != nil {
        return model.Organization{}, fmt.Errorf("storage: org not found for customer: %s", customerID)
    }
    return org, nil
}
```

---

## 7. HTTP Endpoints

### `internal/server/handlers_billing.go` (new file)

```go
package server

import (
    "io"
    "net/http"

    "github.com/ashita-ai/akashi/internal/model"
)

// HandleBillingCheckout handles POST /billing/checkout.
func (h *Handlers) HandleBillingCheckout(w http.ResponseWriter, r *http.Request) {
    claims := ClaimsFromContext(r.Context())

    if !model.RoleAtLeast(claims.Role, model.RoleOrgOwner) {
        writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "only org owners can manage billing")
        return
    }

    if !h.billingSvc.Enabled() {
        writeError(w, r, http.StatusServiceUnavailable, model.ErrCodeInternalError, "billing not configured")
        return
    }

    org, err := h.db.GetOrganization(r.Context(), claims.OrgID)
    if err != nil {
        h.logger.Error("billing checkout: get org", "error", err, "org_id", claims.OrgID)
        writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "internal error")
        return
    }

    var req struct {
        SuccessURL string `json:"success_url"`
        CancelURL  string `json:"cancel_url"`
    }
    if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
        writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
        return
    }

    url, err := h.billingSvc.CreateCheckoutSession(
        claims.OrgID.String(), org.Email, req.SuccessURL, req.CancelURL,
    )
    if err != nil {
        h.logger.Error("billing checkout: create session", "error", err, "org_id", claims.OrgID)
        writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create checkout session")
        return
    }

    writeJSON(w, r, http.StatusOK, map[string]string{"checkout_url": url})
}

// HandleBillingPortal handles POST /billing/portal.
func (h *Handlers) HandleBillingPortal(w http.ResponseWriter, r *http.Request) {
    claims := ClaimsFromContext(r.Context())

    if !model.RoleAtLeast(claims.Role, model.RoleOrgOwner) {
        writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "only org owners can manage billing")
        return
    }

    if !h.billingSvc.Enabled() {
        writeError(w, r, http.StatusServiceUnavailable, model.ErrCodeInternalError, "billing not configured")
        return
    }

    org, err := h.db.GetOrganization(r.Context(), claims.OrgID)
    if err != nil {
        h.logger.Error("billing portal: get org", "error", err, "org_id", claims.OrgID)
        writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "internal error")
        return
    }

    if org.StripeCustomerID == nil {
        writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "no active subscription")
        return
    }

    var req struct {
        ReturnURL string `json:"return_url"`
    }
    if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
        writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
        return
    }

    url, err := h.billingSvc.CreatePortalSession(*org.StripeCustomerID, req.ReturnURL)
    if err != nil {
        h.logger.Error("billing portal: create session", "error", err, "org_id", claims.OrgID)
        writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create portal session")
        return
    }

    writeJSON(w, r, http.StatusOK, map[string]string{"portal_url": url})
}

// HandleBillingWebhook handles POST /billing/webhooks (Stripe signature verification, no JWT auth).
func (h *Handlers) HandleBillingWebhook(w http.ResponseWriter, r *http.Request) {
    body, err := io.ReadAll(io.LimitReader(r.Body, 65536))
    if err != nil {
        writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "failed to read body")
        return
    }

    sigHeader := r.Header.Get("Stripe-Signature")
    status, err := h.billingSvc.HandleWebhook(r.Context(), h.db, h.logger, body, sigHeader)
    if err != nil {
        h.logger.Error("billing webhook failed", "error", err, "status", status)
        writeError(w, r, status, model.ErrCodeInternalError, err.Error())
        return
    }

    w.WriteHeader(http.StatusOK)
}

// HandleUsage handles GET /v1/usage.
func (h *Handlers) HandleUsage(w http.ResponseWriter, r *http.Request) {
    claims := ClaimsFromContext(r.Context())

    if !model.RoleAtLeast(claims.Role, model.RoleAdmin) {
        writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "insufficient permissions")
        return
    }

    org, err := h.db.GetOrganization(r.Context(), claims.OrgID)
    if err != nil {
        h.logger.Error("usage: get org", "error", err, "org_id", claims.OrgID)
        writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "internal error")
        return
    }

    usage, _ := h.db.GetUsage(r.Context(), claims.OrgID, billing.CurrentPeriod())

    writeJSON(w, r, http.StatusOK, map[string]any{
        "org_id":         claims.OrgID,
        "plan":           org.Plan,
        "period":         billing.CurrentPeriod(),
        "decision_count": usage.DecisionCount,
        "decision_limit": org.DecisionLimit,
        "agent_limit":    org.AgentLimit,
    })
}
```

### Route Registration

In `internal/server/server.go`:

```go
// Billing endpoints.
ownerOnly := requireRole(model.RolePlatformAdmin, model.RoleOrgOwner)
mux.Handle("POST /billing/checkout", ownerOnly(http.HandlerFunc(h.HandleBillingCheckout)))
mux.Handle("POST /billing/portal", ownerOnly(http.HandlerFunc(h.HandleBillingPortal)))
mux.Handle("POST /billing/webhooks", http.HandlerFunc(h.HandleBillingWebhook)) // No JWT auth — Stripe signature
mux.Handle("GET /v1/usage", allRoles(http.HandlerFunc(h.HandleUsage)))
```

Add `/billing/webhooks` to auth middleware skip list.

---

## 8. Quota Enforcement in Trace Path

### `internal/service/decisions/service.go`

Add billing service as dependency:

```go
type Service struct {
    db         *storage.DB
    embedder   embedding.Provider
    billingSvc *billing.Service   // NEW (may be nil if billing disabled)
    logger     *slog.Logger
}
```

In `Trace` method, check quota before writing:

```go
func (s *Service) Trace(ctx context.Context, orgID uuid.UUID, input TraceInput) (TraceResult, error) {
    // Quota check (before any DB writes or embedding calls).
    if s.billingSvc != nil {
        if err := s.billingSvc.CheckDecisionQuota(ctx, s.db, orgID); err != nil {
            return TraceResult{}, fmt.Errorf("trace: %w", err)
        }
    }

    // ... existing embedding + write logic ...

    // After successful commit, increment usage counter.
    if s.billingSvc != nil {
        if err := s.billingSvc.IncrementDecisionCount(ctx, s.db, orgID); err != nil {
            s.logger.Warn("trace: increment usage counter failed (non-fatal)", "error", err, "org_id", orgID)
        }
    }

    return TraceResult{...}, nil
}
```

### Agent Quota in CreateAgent

In `HandleCreateAgent` (handlers_admin.go):

```go
// Check agent quota before creating.
if h.billingSvc != nil && h.billingSvc.Enabled() {
    if err := h.billingSvc.CheckAgentQuota(r.Context(), h.db, claims.OrgID); err != nil {
        writeError(w, r, http.StatusTooManyRequests, "quota_exceeded", err.Error())
        return
    }
}
```

---

## 9. Handlers Struct Updates

Add `billingSvc` to `Handlers`:

```go
type Handlers struct {
    // ... existing fields ...
    billingSvc *billing.Service
}
```

### Main Wiring

In `cmd/akashi/main.go`:

```go
billingSvc := billing.New(cfg.StripeSecretKey, cfg.StripeWebhookSecret, cfg.StripePriceIDPro)
```

Pass to `NewHandlers` and `decisions.New`.

---

## Files Changed

| File | Action |
|------|--------|
| `internal/billing/billing.go` | **Create** — Stripe client, plan defs, checkout/portal |
| `internal/billing/webhooks.go` | **Create** — webhook handler |
| `internal/billing/metering.go` | **Create** — quota checks, usage counter |
| `internal/server/handlers_billing.go` | **Create** — billing HTTP endpoints |
| `internal/storage/organizations.go` | Modify — add GetOrganizationByStripeCustomer |
| `internal/config/config.go` | Modify — add Stripe config fields |
| `internal/server/server.go` | Modify — add billing routes |
| `internal/server/middleware.go` | Modify — skip auth for billing webhooks |
| `internal/server/handlers.go` | Modify — wire billing service |
| `internal/server/handlers_admin.go` | Modify — agent quota check |
| `internal/service/decisions/service.go` | Modify — quota check + usage increment in Trace |
| `cmd/akashi/main.go` | Modify — wire billing service |
| `go.mod` | Modify — add stripe-go/v82 |

## Success Criteria

1. `POST /billing/checkout` returns a Stripe Checkout URL
2. `POST /billing/webhooks` with valid Stripe signature updates org plan
3. `POST /billing/webhooks` with invalid signature returns 400
4. Free tier org is rejected after 1000 decisions/month (429)
5. Pro tier org can trace up to 50,000 decisions/month
6. Enterprise org has no quota limits
7. `GET /v1/usage` returns current period stats
8. Agent creation respects agent limit (free = 1)
9. Usage counter is atomic (no race conditions under concurrent traces)
10. All existing tests pass
11. `go test -race ./...` passes
