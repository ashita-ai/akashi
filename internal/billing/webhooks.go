package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	stripe "github.com/stripe/stripe-go/v84"
)

// HandleWebhook processes a Stripe webhook event. Returns the HTTP status code
// to respond with and any error. Verifies the webhook signature, then dispatches
// to the appropriate handler based on event type.
func (s *Service) HandleWebhook(ctx context.Context, body []byte, sigHeader string) (int, error) {
	event, err := stripe.ConstructEvent(body, sigHeader, s.webhookSecret)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("billing: invalid webhook signature: %w", err)
	}

	switch event.Type {
	case "checkout.session.completed":
		return s.handleCheckoutCompleted(ctx, event)
	case "customer.subscription.updated":
		return s.handleSubscriptionUpdated(ctx, event)
	case "customer.subscription.deleted":
		return s.handleSubscriptionDeleted(ctx, event)
	case "invoice.payment_failed":
		return s.handlePaymentFailed(ctx, event)
	default:
		return http.StatusOK, nil
	}
}

func (s *Service) handleCheckoutCompleted(ctx context.Context, event stripe.Event) (int, error) {
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

	org, err := s.db.GetOrganization(ctx, orgID)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("billing: get org: %w", err)
	}

	proPlan := s.plans["pro"]
	org.Plan = "pro"
	if sess.Customer != nil {
		org.StripeCustomerID = &sess.Customer.ID
	}
	if sess.Subscription != nil {
		org.StripeSubscriptionID = &sess.Subscription.ID
	}
	org.DecisionLimit = proPlan.DecisionLimit
	org.AgentLimit = proPlan.AgentLimit

	if err := s.db.UpdateOrganization(ctx, org); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("billing: update org: %w", err)
	}

	customerID := ""
	if sess.Customer != nil {
		customerID = sess.Customer.ID
	}
	s.logger.Info("billing: checkout completed, upgraded to pro",
		"org_id", orgID,
		"customer_id", customerID,
	)
	return http.StatusOK, nil
}

func (s *Service) handleSubscriptionUpdated(ctx context.Context, event stripe.Event) (int, error) {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return http.StatusBadRequest, fmt.Errorf("billing: unmarshal subscription: %w", err)
	}

	customerID := ""
	if sub.Customer != nil {
		customerID = sub.Customer.ID
	}
	org, err := s.db.GetOrganizationByStripeCustomer(ctx, customerID)
	if err != nil {
		s.logger.Warn("billing: subscription updated for unknown customer", "customer_id", customerID)
		return http.StatusOK, nil // Don't fail — might be a different product.
	}

	newPlan := "free"
	for name, plan := range s.plans {
		if plan.PriceID != "" && sub.Items != nil && len(sub.Items.Data) > 0 && sub.Items.Data[0].Price != nil && sub.Items.Data[0].Price.ID == plan.PriceID {
			newPlan = name
			break
		}
	}

	plan := s.plans[newPlan]
	org.Plan = newPlan
	org.DecisionLimit = plan.DecisionLimit
	org.AgentLimit = plan.AgentLimit

	if err := s.db.UpdateOrganization(ctx, org); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("billing: update org plan: %w", err)
	}

	s.logger.Info("billing: subscription updated", "org_id", org.ID, "plan", newPlan)
	return http.StatusOK, nil
}

func (s *Service) handleSubscriptionDeleted(ctx context.Context, event stripe.Event) (int, error) {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return http.StatusBadRequest, fmt.Errorf("billing: unmarshal subscription: %w", err)
	}

	customerID := ""
	if sub.Customer != nil {
		customerID = sub.Customer.ID
	}
	org, err := s.db.GetOrganizationByStripeCustomer(ctx, customerID)
	if err != nil {
		s.logger.Warn("billing: subscription deleted for unknown customer", "customer_id", customerID)
		return http.StatusOK, nil
	}

	freePlan := s.plans["free"]
	org.Plan = "free"
	org.DecisionLimit = freePlan.DecisionLimit
	org.AgentLimit = freePlan.AgentLimit
	org.StripeSubscriptionID = nil

	if err := s.db.UpdateOrganization(ctx, org); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("billing: downgrade org: %w", err)
	}

	s.logger.Info("billing: subscription deleted, downgraded to free", "org_id", org.ID)
	return http.StatusOK, nil
}

func (s *Service) handlePaymentFailed(ctx context.Context, event stripe.Event) (int, error) {
	var invoice stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &invoice); err != nil {
		return http.StatusBadRequest, fmt.Errorf("billing: unmarshal invoice: %w", err)
	}

	customerID := ""
	if invoice.Customer != nil {
		customerID = invoice.Customer.ID
	}
	s.logger.Warn("billing: payment failed",
		"customer_id", customerID,
		"amount_due", invoice.AmountDue,
		"attempt_count", invoice.AttemptCount,
	)

	return http.StatusOK, nil
}
