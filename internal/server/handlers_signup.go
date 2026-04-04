package server

import (
	"net/http"
	"net/mail"
	"strings"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// HandleSignup handles POST /auth/signup (unauthenticated, gated by config).
// Atomically creates an org, its owner agent, and a managed API key.
// The raw key is returned exactly once in the response.
func (h *Handlers) HandleSignup(w http.ResponseWriter, r *http.Request) {
	// Dedicated rate limit: 1 RPS / burst 5 per source IP.
	if h.signupLimiter != nil {
		ip := clientIP(r, h.trustProxy)
		res, err := h.signupLimiter.Allow(r.Context(), "signup:"+ip)
		if err != nil {
			h.logger.Warn("signup rate limiter error, failing open", "error", err)
		} else {
			setRateLimitHeaders(w, res)
			if !res.Allowed {
				w.Header().Set("Retry-After", "1")
				writeError(w, r, http.StatusTooManyRequests, model.ErrCodeRateLimited, "rate limit exceeded")
				return
			}
		}
	}

	var req model.SignupRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	// --- Input validation ---
	req.OrgName = strings.TrimSpace(req.OrgName)
	req.AgentID = strings.TrimSpace(req.AgentID)
	req.Email = strings.TrimSpace(req.Email)

	if req.OrgName == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "org_name is required")
		return
	}
	if req.AgentID == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "agent_id is required")
		return
	}
	if err := model.ValidateAgentID(req.AgentID); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}
	if model.IsReservedAgentID(req.AgentID) {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"agent_id \""+req.AgentID+"\" is reserved and cannot be used")
		return
	}
	if req.Email == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "email is required")
		return
	}
	if _, err := mail.ParseAddress(req.Email); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid email address")
		return
	}

	slug := model.Slugify(req.OrgName)
	if slug == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"org_name must contain at least one alphanumeric character")
		return
	}

	// --- Generate managed API key ---
	rawKey, prefix, err := model.GenerateRawKey()
	if err != nil {
		h.writeInternalError(w, r, "failed to generate api key", err)
		return
	}
	hash, err := auth.HashAPIKey(rawKey)
	if err != nil {
		h.writeInternalError(w, r, "failed to hash api key", err)
		return
	}

	// --- Build audit entries ---
	// No auth claims exist for signup; the actor is the new agent being created.
	reqID := RequestIDFromContext(r.Context())
	makeAudit := func(operation, resourceType string) storage.MutationAuditEntry {
		return storage.MutationAuditEntry{
			RequestID:    reqID,
			ActorAgentID: req.AgentID,
			ActorRole:    string(model.RoleOrgOwner),
			HTTPMethod:   r.Method,
			Endpoint:     r.URL.Path,
			Operation:    operation,
			ResourceType: resourceType,
		}
	}

	org, _, apiKey, err := h.db.CreateOrgWithOwnerAndKeyTx(r.Context(),
		model.Organization{
			Name: req.OrgName,
			Slug: slug,
			Plan: "oss",
		},
		model.Agent{
			AgentID: req.AgentID,
			Name:    req.AgentID, // use agent_id as display name
			Role:    model.RoleOrgOwner,
			Email:   &req.Email,
		},
		model.APIKey{
			Prefix:    prefix,
			KeyHash:   hash,
			AgentID:   req.AgentID,
			Label:     "default",
			CreatedBy: req.AgentID,
		},
		makeAudit("create_organization", "organization"),
		makeAudit("create_agent", "agent"),
		makeAudit("create_api_key", "api_key"),
	)
	if err != nil {
		if isDuplicateKeyError(err) {
			writeError(w, r, http.StatusConflict, model.ErrCodeConflict,
				"org name, agent_id, or email already in use")
			return
		}
		h.writeInternalError(w, r, "failed to create organization", err)
		return
	}

	// --- Build MCP config snippet ---
	scheme := "https"
	if r.TLS == nil {
		if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
			scheme = fwd
		} else {
			scheme = "http"
		}
	}
	baseURL := scheme + "://" + r.Host

	writeJSON(w, r, http.StatusCreated, model.SignupResponse{
		OrgID:   org.ID,
		OrgSlug: org.Slug,
		AgentID: req.AgentID,
		APIKey:  rawKey,
		MCPConfig: model.MCPConfigSnippet{
			URL:    baseURL + "/mcp",
			Header: "ApiKey " + req.AgentID + ":" + rawKey,
		},
	})

	h.logger.Info("self-serve signup completed",
		"org_id", org.ID,
		"org_slug", org.Slug,
		"agent_id", req.AgentID,
		"api_key_id", apiKey.ID,
		"request_id", reqID,
	)
}
