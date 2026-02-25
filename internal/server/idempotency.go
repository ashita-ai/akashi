package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

type idempotencyHandle struct {
	key      string
	endpoint string
	agentID  string
}

func idempotencyKey(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("Idempotency-Key"))
}

func requestHash(payload any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// beginIdempotentWrite checks/reuses/reserves an idempotency key.
// Returns (nil, true) when no idempotency key is present and caller should proceed normally.
func (h *Handlers) beginIdempotentWrite(
	w http.ResponseWriter,
	r *http.Request,
	orgID uuid.UUID,
	agentID, endpoint string,
	payload any,
) (*idempotencyHandle, bool) {
	key := idempotencyKey(r)
	if key == "" {
		return nil, true
	}

	hash, err := requestHash(payload)
	if err != nil {
		h.writeInternalError(w, r, "failed to hash idempotency payload", err)
		return nil, false
	}

	lookup, err := h.db.BeginIdempotency(
		r.Context(),
		orgID,
		agentID,
		endpoint,
		key,
		hash,
	)
	switch {
	case err == nil:
		if lookup.Completed {
			var replay any
			if len(lookup.ResponseData) > 0 {
				if uErr := json.Unmarshal(lookup.ResponseData, &replay); uErr != nil {
					h.writeInternalError(w, r, "failed to unmarshal idempotent replay payload", uErr)
					return nil, false
				}
			}
			status := lookup.StatusCode
			if status == 0 {
				status = http.StatusOK
			}
			writeJSON(w, r, status, replay)
			return nil, false
		}
		return &idempotencyHandle{key: key, endpoint: endpoint, agentID: agentID}, true
	case errors.Is(err, storage.ErrIdempotencyPayloadMismatch):
		writeError(w, r, http.StatusConflict, model.ErrCodeConflict, "idempotency key reused with different payload")
		return nil, false
	case errors.Is(err, storage.ErrIdempotencyInProgress):
		writeError(w, r, http.StatusConflict, model.ErrCodeConflict, "request with this idempotency key is already in progress")
		return nil, false
	default:
		h.writeInternalError(w, r, "idempotency lookup failed", err)
		return nil, false
	}
}

func (h *Handlers) completeIdempotentWrite(
	orgID uuid.UUID,
	idem *idempotencyHandle,
	statusCode int,
	data any,
) error {
	if idem == nil {
		return nil
	}

	// Finish idempotency in a bounded background context to avoid tying
	// correctness to request cancellation at the edge of a timeout.
	// Use a generous timeout because failed finalization can cause replay gaps.
	writeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := h.db.CompleteIdempotency(writeCtx, orgID, idem.agentID, idem.endpoint, idem.key, statusCode, data); err == nil {
			return nil
		} else {
			lastErr = err
			h.logger.Warn("idempotency finalize attempt failed",
				"attempt", attempt,
				"error", err,
				"endpoint", idem.endpoint,
				"agent_id", idem.agentID,
			)
		}

		// Short backoff between retries, bounded by writeCtx.
		select {
		case <-time.After(time.Duration(attempt) * 50 * time.Millisecond):
		case <-writeCtx.Done():
			return fmt.Errorf("idempotency finalize context expired: %w", lastErr)
		}
	}

	return fmt.Errorf("failed to complete idempotency record after retries: %w", lastErr)
}

// completeIdempotentWriteBestEffort finalizes an idempotency key without failing
// the already-committed mutation response path.
//
// If finalization fails after retries, the key is cleared so that future retries
// start fresh instead of receiving 409 for the duration of the abandoned TTL.
// The trade-off is that a retry will re-execute the mutation (potential duplicate),
// which is preferable to a 24h window of stuck 409 responses (#73).
func (h *Handlers) completeIdempotentWriteBestEffort(
	r *http.Request,
	orgID uuid.UUID,
	idem *idempotencyHandle,
	statusCode int,
	data any,
) {
	if err := h.completeIdempotentWrite(orgID, idem, statusCode, data); err != nil {
		h.logger.Error("failed to finalize idempotency record after committed mutation — clearing key to unblock retries",
			"error", err,
			"org_id", orgID,
			"endpoint", idem.endpoint,
			"agent_id", idem.agentID,
			"request_id", RequestIDFromContext(r.Context()),
		)
		// Use a background context because the request context may already be
		// cancelled (the mutation succeeded but the response write or finalize
		// timed out). A stuck in_progress key is worse than a cleared one.
		clearCtx, clearCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if clearErr := h.db.ClearInProgressIdempotency(clearCtx, orgID, idem.agentID, idem.endpoint, idem.key); clearErr != nil {
			h.logger.Error("failed to clear stuck idempotency key — key will remain in_progress until cleanup job runs",
				"error", clearErr,
				"endpoint", idem.endpoint,
				"agent_id", idem.agentID,
			)
		}
		clearCancel()
	}
}

func (h *Handlers) clearIdempotentWrite(r *http.Request, orgID uuid.UUID, idem *idempotencyHandle) {
	if idem == nil {
		return
	}
	if err := h.db.ClearInProgressIdempotency(r.Context(), orgID, idem.agentID, idem.endpoint, idem.key); err != nil {
		h.logger.Error("failed to clear idempotency record",
			"error", err,
			"endpoint", idem.endpoint,
			"agent_id", idem.agentID,
		)
	}
}

func appendEventsEndpoint(runID uuid.UUID) string {
	return fmt.Sprintf("POST:/v1/runs/%s/events", runID)
}

func completeRunEndpoint(runID uuid.UUID) string {
	return fmt.Sprintf("POST:/v1/runs/%s/complete", runID)
}
