package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/storage"
)

func (h *Handlers) recordMutationAudit(
	r *http.Request,
	orgID uuid.UUID,
	operation, resourceType, resourceID string,
	beforeData, afterData any,
	metadata map[string]any,
) error {
	claims := ClaimsFromContext(r.Context())
	actorID := "unknown"
	actorRole := "unknown"
	if claims != nil {
		actorID = claims.AgentID
		actorRole = string(claims.Role)
	}

	entry := storage.MutationAuditEntry{
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
