package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/ctxutil"
	"github.com/ashita-ai/akashi/internal/storage"
)

// buildAuditEntry constructs a MutationAuditEntry from the current HTTP request.
// Used by handlers that pass the entry into transactional *WithAudit storage methods.
func (h *Handlers) buildAuditEntry(
	r *http.Request,
	orgID uuid.UUID,
	operation, resourceType, resourceID string,
	beforeData, afterData any,
	metadata map[string]any,
) storage.MutationAuditEntry {
	claims := ClaimsFromContext(r.Context())
	actorID := "unknown"
	actorRole := "unknown"
	if claims != nil {
		actorID = claims.AgentID
		actorRole = string(claims.Role)
	}

	return storage.MutationAuditEntry{
		RequestID:    RequestIDFromContext(r.Context()),
		OrgID:        orgID,
		ActorAgentID: actorID,
		ActorRole:    actorRole,
		HTTPMethod:   r.Method,
		Endpoint:     r.URL.Path,
		Operation:    operation,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		BeforeData:   beforeData,
		AfterData:    afterData,
		Metadata:     metadata,
	}
}

// buildAuditMeta constructs the AuditMeta that the service layer needs to
// build an audit entry inside CreateTraceTx.
func (h *Handlers) buildAuditMeta(r *http.Request, orgID uuid.UUID) *ctxutil.AuditMeta {
	claims := ClaimsFromContext(r.Context())
	actorID := "unknown"
	actorRole := "unknown"
	if claims != nil {
		actorID = claims.AgentID
		actorRole = string(claims.Role)
	}
	return &ctxutil.AuditMeta{
		RequestID:    RequestIDFromContext(r.Context()),
		OrgID:        orgID,
		ActorAgentID: actorID,
		ActorRole:    actorRole,
		HTTPMethod:   r.Method,
		Endpoint:     r.URL.Path,
	}
}

// recordMutationAuditBestEffort appends a mutation audit event outside any
// transaction. Only used for HandleAppendEvents where the COPY buffer system
// is architecturally incompatible with transactional audit. All other
// mutation endpoints use atomic *WithAudit storage methods instead.
func (h *Handlers) recordMutationAuditBestEffort(
	r *http.Request,
	orgID uuid.UUID,
	operation, resourceType, resourceID string,
	beforeData, afterData any,
	metadata map[string]any,
) error {
	entry := h.buildAuditEntry(r, orgID, operation, resourceType, resourceID, beforeData, afterData, metadata)

	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := h.db.InsertMutationAudit(writeCtx, entry); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-time.After(time.Duration(attempt) * 50 * time.Millisecond):
		case <-writeCtx.Done():
			return fmt.Errorf("mutation audit write context expired: %w", lastErr)
		}
	}
	return fmt.Errorf("mutation audit write failed after retries: %w", lastErr)
}
