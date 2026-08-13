package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// HandleGetDecisionProof handles GET /v1/integrity/proof/{id}.
// Returns a Merkle inclusion proof for a specific decision, allowing external
// auditors to verify that a decision is part of the tamper-evident audit trail
// without reconstructing the entire batch.
func (h *Handlers) HandleGetDecisionProof(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	decisionID, err := parsePathUUID(r, "id")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid decision ID")
		return
	}

	// 1. Find the proof batch that covers this decision.
	proof, contentHash, err := h.db.FindProofForDecision(r.Context(), orgID, decisionID)
	if err != nil {
		h.writeInternalError(w, r, "failed to find proof for decision", err)
		return
	}
	if proof == nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "no integrity proof covers this decision")
		return
	}

	// 2. Get all leaves for this proof. Prefer the snapshot in proof_leaves;
	//    fall back to re-querying decisions for pre-migration-082 proofs.
	leaves, err := h.db.GetProofLeaves(r.Context(), orgID, proof.ID)
	if err != nil {
		h.writeInternalError(w, r, "failed to get proof leaves", err)
		return
	}
	if len(leaves) == 0 {
		leaves, err = h.db.GetDecisionHashesForBatch(r.Context(), orgID, proof.BatchStart, proof.BatchEnd)
		if err != nil {
			h.writeInternalError(w, r, "failed to get decision hashes for batch", err)
			return
		}
	}

	if len(leaves) == 0 {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "no leaves found for proof batch")
		return
	}

	// 3. Generate the Merkle inclusion proof.
	steps, rootHash, err := integrity.GenerateMerkleProof(leaves, contentHash)
	if err != nil {
		if errors.Is(err, integrity.ErrLeafNotFound) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound,
				"decision content hash not found in proof batch leaves")
			return
		}
		h.writeInternalError(w, r, "failed to generate Merkle proof", err)
		return
	}

	// 4. Return the proof with a self-verification check.
	writeJSON(w, r, http.StatusOK, model.DecisionProofResponse{
		DecisionID:  decisionID,
		ContentHash: contentHash,
		ProofID:     proof.ID,
		RootHash:    rootHash,
		BatchStart:  proof.BatchStart,
		BatchEnd:    proof.BatchEnd,
		ProofPath:   steps,
		Verified:    rootHash == proof.RootHash,
	})
}

// HandleVerifyDecision handles GET /v1/verify/{id}.
// Recomputes the SHA-256 content hash from stored fields and compares to the stored hash.
func (h *Handlers) HandleVerifyDecision(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	id, err := parsePathUUID(r, "id")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid decision ID")
		return
	}

	claims := ClaimsFromContext(r.Context())

	d, err := h.db.GetDecision(r.Context(), orgID, id, storage.GetDecisionOpts{})
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "decision not found")
		return
	}

	ok, err := canAccessAgent(r.Context(), h.db, claims, d.AgentID)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}
	if !ok {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "no access to this decision")
		return
	}

	resp := model.VerifyDecisionResponse{DecisionID: id}

	switch {
	case d.ValidTo != nil:
		resp.Status = "retracted"
		resp.RetractedAt = d.ValidTo.UTC().Format(time.RFC3339Nano)
		if d.ContentHash == "" {
			verified := false
			resp.Verified = &verified
			resp.Message = "this decision was created before content hashing was enabled"
		} else {
			valid := integrity.VerifyContentHash(d.ContentHash, d.ID, d.DecisionType, d.Outcome, d.Confidence, d.Reasoning, d.ValidFrom)
			resp.Verified = &valid
			resp.ContentHash = d.ContentHash
		}
	case d.ContentHash == "":
		resp.Status = "no_hash"
		resp.Message = "this decision was created before content hashing was enabled"
	default:
		erasure, erasureErr := h.db.GetDecisionErasure(r.Context(), orgID, id)
		switch {
		case erasureErr == nil:
			valid := integrity.VerifyContentHash(d.ContentHash, d.ID, d.DecisionType, d.Outcome, d.Confidence, d.Reasoning, d.ValidFrom)
			resp.Status = "erased"
			resp.Valid = &valid
			resp.ContentHash = d.ContentHash
			resp.OriginalHash = erasure.OriginalHash
			resp.ErasedAt = &erasure.ErasedAt
			resp.ErasedBy = erasure.ErasedBy
		case !isNotFoundError(erasureErr):
			h.writeInternalError(w, r, "failed to check erasure status", erasureErr)
			return
		default:
			valid := integrity.VerifyContentHash(d.ContentHash, d.ID, d.DecisionType, d.Outcome, d.Confidence, d.Reasoning, d.ValidFrom)
			resp.Valid = &valid
			if valid {
				resp.Status = "verified"
			} else {
				resp.Status = "tampered"
			}
			resp.ContentHash = d.ContentHash
		}
	}

	writeJSON(w, r, http.StatusOK, resp)
}

// HandleListIntegrityViolations handles GET /v1/integrity/violations.
// Returns recent integrity violations for the caller's organization, ordered
// newest-first. This exposes the durable audit trail written by the background
// integrity audit loop (auditIntegrityProofs). Admin-only because violations
// indicate potential tampering and are part of incident response.
func (h *Handlers) HandleListIntegrityViolations(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "limit must be between 1 and 200")
			return
		}
		limit = n
	}

	violations, err := h.db.GetIntegrityViolations(r.Context(), orgID, limit)
	if err != nil {
		h.writeInternalError(w, r, "failed to list integrity violations", err)
		return
	}

	writeJSON(w, r, http.StatusOK, model.IntegrityViolationsResponse{
		Violations: violations,
		Count:      len(violations),
	})
}
