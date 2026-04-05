package server

import (
	"errors"
	"net/http"

	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/model"
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
	writeJSON(w, r, http.StatusOK, map[string]any{
		"decision_id":  decisionID,
		"content_hash": contentHash,
		"proof_id":     proof.ID,
		"root_hash":    rootHash,
		"batch_start":  proof.BatchStart,
		"batch_end":    proof.BatchEnd,
		"proof_path":   steps,
		"verified":     rootHash == proof.RootHash,
	})
}
