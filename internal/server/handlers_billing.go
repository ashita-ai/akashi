package server

import (
	"io"
	"net/http"

	"github.com/ashita-ai/akashi/internal/billing"
	"github.com/ashita-ai/akashi/internal/model"
)

// HandleBillingCheckout handles POST /billing/checkout (org_owner+).
// Creates a Stripe Checkout session for upgrading to the Pro plan.
func (h *Handlers) HandleBillingCheckout(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	if h.billingSvc == nil || !h.billingSvc.Enabled() {
		writeError(w, r, http.StatusServiceUnavailable, model.ErrCodeInternalError, "billing not configured")
		return
	}

	org, err := h.db.GetOrganization(r.Context(), orgID)
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

	if req.SuccessURL == "" || req.CancelURL == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "success_url and cancel_url are required")
		return
	}

	url, err := h.billingSvc.CreateCheckoutSession(r.Context(), orgID.String(), org.Email, req.SuccessURL, req.CancelURL)
	if err != nil {
		h.logger.Error("billing checkout: create session", "error", err, "org_id", orgID)
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create checkout session")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]string{"checkout_url": url})
}

// HandleBillingPortal handles POST /billing/portal (org_owner+).
// Creates a Stripe Billing Portal session for managing an existing subscription.
func (h *Handlers) HandleBillingPortal(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	if h.billingSvc == nil || !h.billingSvc.Enabled() {
		writeError(w, r, http.StatusServiceUnavailable, model.ErrCodeInternalError, "billing not configured")
		return
	}

	org, err := h.db.GetOrganization(r.Context(), orgID)
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

	if req.ReturnURL == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "return_url is required")
		return
	}

	url, err := h.billingSvc.CreatePortalSession(r.Context(), *org.StripeCustomerID, req.ReturnURL)
	if err != nil {
		h.logger.Error("billing portal: create session", "error", err, "org_id", orgID)
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create portal session")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]string{"portal_url": url})
}

// HandleBillingWebhook handles POST /billing/webhooks.
// This endpoint is NOT protected by JWT auth — Stripe signs the payload with
// its webhook secret. The billing service verifies the signature.
func (h *Handlers) HandleBillingWebhook(w http.ResponseWriter, r *http.Request) {
	if h.billingSvc == nil || !h.billingSvc.Enabled() {
		writeError(w, r, http.StatusServiceUnavailable, model.ErrCodeInternalError, "billing not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, h.maxRequestBodyBytes))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "failed to read body")
		return
	}

	sigHeader := r.Header.Get("Stripe-Signature")
	status, whErr := h.billingSvc.HandleWebhook(r.Context(), body, sigHeader)
	if whErr != nil {
		h.logger.Error("billing webhook failed", "error", whErr, "status", status)
		writeError(w, r, status, model.ErrCodeInternalError, whErr.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
}

// HandleUsage handles GET /v1/usage.
// Returns the current billing period's usage statistics for the caller's org.
func (h *Handlers) HandleUsage(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	org, err := h.db.GetOrganization(r.Context(), orgID)
	if err != nil {
		h.logger.Error("usage: get org", "error", err, "org_id", orgID)
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "internal error")
		return
	}

	period := billing.CurrentPeriod()
	usage, _ := h.db.GetUsage(r.Context(), orgID, period)

	writeJSON(w, r, http.StatusOK, map[string]any{
		"org_id":         orgID,
		"plan":           org.Plan,
		"period":         period,
		"decision_count": usage.DecisionCount,
		"decision_limit": org.DecisionLimit,
		"agent_limit":    org.AgentLimit,
	})
}
