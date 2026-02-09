// Package billing integrates Stripe for subscription management, usage metering,
// and quota enforcement. If Stripe is not configured (no secret key), billing
// endpoints return 503 and quota enforcement is disabled.
package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	stripe "github.com/stripe/stripe-go/v84"

	"github.com/ashita-ai/akashi/internal/storage"
)

// Sentinel errors for quota enforcement.
var (
	ErrQuotaExceeded      = errors.New("monthly decision quota exceeded")
	ErrAgentLimitExceeded = errors.New("agent limit exceeded")
	ErrBillingDisabled    = errors.New("billing not configured")
)

// Plan defines limits for an org's subscription tier.
type Plan struct {
	Name          string
	PriceID       string // Stripe Price ID (empty for free/enterprise).
	DecisionLimit int    // 0 = unlimited.
	AgentLimit    int    // 0 = unlimited.
}

// Service wraps Stripe API calls and provides quota enforcement.
type Service struct {
	client        *stripe.Client
	db            *storage.DB
	logger        *slog.Logger
	plans         map[string]Plan
	webhookSecret string
	proPriceID    string
	enabled       bool
}

// Config holds Stripe configuration.
type Config struct {
	SecretKey     string
	WebhookSecret string
	PriceIDPro    string
}

// New creates a billing service. If cfg.SecretKey is empty, the service
// operates in disabled mode (no quota enforcement, billing endpoints 503).
// Returns an error if billing is enabled but required fields are missing.
func New(db *storage.DB, cfg Config, logger *slog.Logger) (*Service, error) {
	enabled := cfg.SecretKey != ""

	if enabled {
		if cfg.WebhookSecret == "" {
			return nil, fmt.Errorf("billing: STRIPE_WEBHOOK_SECRET is required when billing is enabled")
		}
		if cfg.PriceIDPro == "" {
			return nil, fmt.Errorf("billing: STRIPE_PRO_PRICE_ID is required when billing is enabled")
		}
	}

	var client *stripe.Client
	if enabled {
		client = stripe.NewClient(cfg.SecretKey)
	}

	return &Service{
		client: client,
		db:     db,
		logger: logger,
		plans: map[string]Plan{
			"free": {
				Name:          "Free",
				DecisionLimit: 1_000,
				AgentLimit:    5,
			},
			"pro": {
				Name:          "Pro",
				PriceID:       cfg.PriceIDPro,
				DecisionLimit: 50_000,
				AgentLimit:    0, // unlimited
			},
			"enterprise": {
				Name:          "Enterprise",
				DecisionLimit: 0, // custom per-org
				AgentLimit:    0,
			},
		},
		webhookSecret: cfg.WebhookSecret,
		proPriceID:    cfg.PriceIDPro,
		enabled:       enabled,
	}, nil
}

// Enabled returns true if Stripe is configured.
func (s *Service) Enabled() bool { return s.enabled }

// GetPlan returns the plan definition for a given plan name.
func (s *Service) GetPlan(name string) (Plan, bool) {
	p, ok := s.plans[name]
	return p, ok
}

// CreateCheckoutSession creates a Stripe Checkout session for plan upgrade.
func (s *Service) CreateCheckoutSession(ctx context.Context, orgID, orgEmail, successURL, cancelURL string) (string, error) {
	if !s.enabled {
		return "", ErrBillingDisabled
	}

	sess, err := s.client.V1CheckoutSessions.Create(ctx, &stripe.CheckoutSessionCreateParams{
		Mode:          stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		CustomerEmail: stripe.String(orgEmail),
		SuccessURL:    stripe.String(successURL),
		CancelURL:     stripe.String(cancelURL),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{
				Price:    stripe.String(s.proPriceID),
				Quantity: stripe.Int64(1),
			},
		},
		Metadata: map[string]string{
			"org_id": orgID,
		},
	})
	if err != nil {
		return "", fmt.Errorf("billing: create checkout session: %w", err)
	}
	return sess.URL, nil
}

// CreatePortalSession creates a Stripe billing portal session for subscription management.
func (s *Service) CreatePortalSession(ctx context.Context, stripeCustomerID, returnURL string) (string, error) {
	if !s.enabled {
		return "", ErrBillingDisabled
	}

	sess, err := s.client.V1BillingPortalSessions.Create(ctx, &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(stripeCustomerID),
		ReturnURL: stripe.String(returnURL),
	})
	if err != nil {
		return "", fmt.Errorf("billing: create portal session: %w", err)
	}
	return sess.URL, nil
}
