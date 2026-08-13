package akashi

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/storage"
)

func (a *App) integrityProofLoop(ctx context.Context) {
	a.runLoop(ctx, "integrityProof", a.cfg.IntegrityProofInterval, func(ctx context.Context) {
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		buildIntegrityProofs(opCtx, a.db, a.logger)
		if hasNull, err := a.db.HasDecisionsWithNullSearchVector(opCtx); err == nil && hasNull {
			a.logger.Warn("decisions with NULL search_vector detected — FTS excludes these rows; check trigger and migration 022 backfill")
		}
	})
}

func (a *App) integrityAuditLoop(ctx context.Context) {
	// Jitter the first tick so multiple replicas don't audit at the same wall-clock time.
	jitter := time.Duration(rand.IntN(int(a.cfg.IntegrityAuditInterval))) //nolint:gosec // jitter, not security
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	ticker := time.NewTicker(a.cfg.IntegrityAuditInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, a.cfg.IntegrityAuditTimeout)
			a.auditIntegrityProofs(opCtx)
			cancel()
		}
	}
}

// integrityFullAuditLoop runs an exhaustive integrity audit across all orgs
// at a lower frequency (default: every 24h). Unlike the sampling audit, this
// checks IntegrityFullAuditProofs proofs per org for every org.
// Disabled when IntegrityFullAuditInterval is 0.
func (a *App) integrityFullAuditLoop(ctx context.Context) {
	if a.cfg.IntegrityFullAuditInterval <= 0 {
		return
	}

	// Jitter the first tick so multiple replicas don't all run the full sweep simultaneously.
	jitter := time.Duration(rand.IntN(int(a.cfg.IntegrityFullAuditInterval))) //nolint:gosec // jitter, not security
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	ticker := time.NewTicker(a.cfg.IntegrityFullAuditInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, a.cfg.IntegrityAuditTimeout)
			a.fullIntegrityAudit(opCtx)
			cancel()
		}
	}
}

// auditIntegrityProofs picks one org per tick (sampling-based, not exhaustive)
// and verifies its most recent Merkle proofs by:
// (1) recomputing each root from decision content hashes, and
// (2) checking chain linkage via previous_root.
// Coverage is probabilistic: with N orgs, each org is audited roughly every
// N * IntegrityAuditInterval. Only the 10 newest proofs per org are checked.
// All results (pass and fail) are persisted to integrity_audit_results.
func (a *App) auditIntegrityProofs(ctx context.Context) {
	// Use offset-based selection instead of loading the full org table.
	// CountOrganizations is a cheap count(*) on the PK index.
	orgCount, err := a.db.CountOrganizations(ctx)
	if err != nil {
		a.logger.Warn("integrity audit: count orgs failed", "error", err)
		return
	}
	if orgCount == 0 {
		return
	}

	// Round-robin through orgs using atomic counter + offset.
	idx := a.auditOrgCounter.Add(1) - 1
	offset := int(idx % uint64(orgCount)) //nolint:gosec // orgCount is validated positive above; modulo result fits int
	orgID, err := a.db.GetOrgIDByOffset(ctx, offset)
	if err != nil {
		a.logger.Warn("integrity audit: get org by offset failed", "offset", offset, "error", err)
		return
	}
	if orgID == uuid.Nil {
		// Org count changed between count and offset query — skip this tick.
		return
	}

	proofs, err := a.db.GetRecentIntegrityProofs(ctx, orgID, 10)
	if err != nil {
		a.logger.Warn("integrity audit: failed to fetch proofs", "org_id", orgID, "error", err)
		return
	}
	if len(proofs) == 0 {
		return
	}

	results := a.verifyProofsForOrg(ctx, orgID, proofs, "sample")

	// Persist results with a dedicated context so that a timeout during
	// verification doesn't cause us to lose the audit record. An audit that
	// detected tampering but failed to persist it is worse than one that
	// never ran — it creates a false sense of coverage.
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer persistCancel()
	if err := a.db.InsertIntegrityAuditResults(persistCtx, results); err != nil {
		a.logger.Error("integrity audit: FAILED TO PERSIST results — audit ran but paper trail is missing",
			"org_id", orgID, "proofs_checked", len(proofs), "error", err)
	}

	a.logger.Info("integrity audit completed", "org_id", orgID, "proofs_checked", len(proofs))
}

// fullIntegrityAudit checks every org exhaustively using IntegrityFullAuditProofs
// proofs per org. All results are persisted. Each org gets its own timeout
// (IntegrityAuditTimeout) so that slow orgs don't starve later ones.
func (a *App) fullIntegrityAudit(ctx context.Context) {
	orgIDs, err := a.db.ListOrganizationIDs(ctx)
	if err != nil {
		a.logger.Warn("integrity full audit: list orgs failed", "error", err)
		return
	}

	var totalProofs, totalFailures int
	for _, orgID := range orgIDs {
		if ctx.Err() != nil {
			a.logger.Warn("integrity full audit: cancelled before completing all orgs",
				"completed", totalProofs, "remaining_orgs", len(orgIDs))
			return
		}

		// Per-org timeout prevents one large org from consuming the entire sweep budget.
		orgCtx, orgCancel := context.WithTimeout(ctx, a.cfg.IntegrityAuditTimeout)

		proofs, err := a.db.GetRecentIntegrityProofs(orgCtx, orgID, a.cfg.IntegrityFullAuditProofs)
		if err != nil {
			a.logger.Warn("integrity full audit: failed to fetch proofs",
				"org_id", orgID, "error", err)
			orgCancel()
			continue
		}
		if len(proofs) == 0 {
			orgCancel()
			continue
		}

		results := a.verifyProofsForOrg(orgCtx, orgID, proofs, "full")
		orgCancel()

		// Persist with a dedicated context — same rationale as the sampling audit:
		// verification results must survive even if the verification context expired.
		persistCtx, persistCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := a.db.InsertIntegrityAuditResults(persistCtx, results); err != nil {
			a.logger.Error("integrity full audit: FAILED TO PERSIST results — audit ran but paper trail is missing",
				"org_id", orgID, "proofs_checked", len(proofs), "error", err)
		}
		persistCancel()

		totalProofs += len(proofs)
		for _, r := range results {
			if !r.Passed {
				totalFailures++
			}
		}
	}

	a.logger.Info("integrity full audit completed",
		"orgs_checked", len(orgIDs), "proofs_checked", totalProofs, "failures", totalFailures)
}

// verifyProofsForOrg checks Merkle roots and chain linkage for the given proofs,
// returning audit results for every check. Both the sampling and full-sweep
// audit loops call this.
func (a *App) verifyProofsForOrg(ctx context.Context, orgID uuid.UUID, proofs []storage.IntegrityProof, sweepType string) []storage.IntegrityAuditResult {
	now := time.Now().UTC()
	var results []storage.IntegrityAuditResult

	for i, p := range proofs {
		// Verify Merkle root.
		// Prefer snapshotted proof_leaves (survives retention purge and GDPR erasure).
		// Fall back to re-querying decisions for proofs created before migration 082.
		hashes, err := a.db.GetProofLeaves(ctx, orgID, p.ID)
		if err != nil {
			a.logger.Warn("integrity audit: failed to fetch proof leaves",
				"org_id", orgID, "proof_id", p.ID, "error", err)
			results = append(results, storage.IntegrityAuditResult{
				OrgID: orgID, ProofID: p.ID, CheckType: "merkle_root",
				Passed: false, SweepType: sweepType,
				Detail: fmt.Sprintf("failed to fetch proof leaves: %v", err), CheckedAt: now,
			})
			continue
		}
		if len(hashes) == 0 {
			// No snapshotted leaves — fall back to decisions table (pre-082 proofs).
			hashes, err = a.db.GetDecisionHashesForBatch(ctx, orgID, p.BatchStart, p.BatchEnd)
			if err != nil {
				a.logger.Warn("integrity audit: failed to fetch hashes for batch",
					"org_id", orgID, "proof_id", p.ID, "error", err)
				results = append(results, storage.IntegrityAuditResult{
					OrgID: orgID, ProofID: p.ID, CheckType: "merkle_root",
					Passed: false, SweepType: sweepType,
					Detail: fmt.Sprintf("failed to fetch hashes: %v", err), CheckedAt: now,
				})
				continue
			}
		}

		ok, err := integrity.VerifyBatchProof(p.RootHash, hashes)
		if err != nil {
			a.logger.Error("integrity audit: merkle verification error",
				"org_id", orgID, "proof_id", p.ID, "error", err)
			results = append(results, storage.IntegrityAuditResult{
				OrgID: orgID, ProofID: p.ID, CheckType: "merkle_root",
				Passed: false, SweepType: sweepType,
				Detail: fmt.Sprintf("verification error: %v", err), CheckedAt: now,
			})
			continue
		}
		if !ok {
			a.logger.Error("INTEGRITY VIOLATION: Merkle root mismatch — stored root does not match recomputed root",
				"org_id", orgID, "proof_id", p.ID,
				"stored_root", p.RootHash, "decision_count", p.DecisionCount)
			a.integrityViolations.Add(ctx, 1, otelmetric.WithAttributes(
				attribute.String("violation_type", "merkle_root_mismatch"),
				attribute.String("sweep_type", sweepType),
			))
			a.persistViolation(ctx, orgID, p.ID, "merkle_root_mismatch", map[string]any{
				"stored_root":    p.RootHash,
				"decision_count": p.DecisionCount,
				"batch_start":    p.BatchStart,
				"batch_end":      p.BatchEnd,
				"leaf_count":     len(hashes),
			})
		}
		results = append(results, storage.IntegrityAuditResult{
			OrgID: orgID, ProofID: p.ID, CheckType: "merkle_root",
			Passed: ok, SweepType: sweepType, CheckedAt: now,
		})

		// Verify chain linkage: this proof's previous_root should match the
		// next-older proof's root_hash (proofs are newest-first).
		if i+1 < len(proofs) {
			older := proofs[i+1]
			var linkPassed bool
			var detail string
			switch {
			case p.PreviousRoot == nil:
				a.logger.Warn("integrity audit: proof has nil previous_root but older proof exists — chain may be broken",
					"org_id", orgID, "proof_id", p.ID, "older_proof_id", older.ID)
				detail = fmt.Sprintf("nil previous_root but older proof %s exists", older.ID)
				a.integrityViolations.Add(ctx, 1, otelmetric.WithAttributes(
					attribute.String("violation_type", "chain_linkage_broken"),
					attribute.String("sweep_type", sweepType),
				))
				a.persistViolation(ctx, orgID, p.ID, "chain_linkage_nil_previous", map[string]any{
					"older_proof_id": older.ID,
					"older_root":     older.RootHash,
				})
			case *p.PreviousRoot != older.RootHash:
				a.logger.Error("INTEGRITY VIOLATION: chain linkage broken — previous_root does not match prior proof",
					"org_id", orgID, "proof_id", p.ID,
					"expected_previous", older.RootHash, "actual_previous", *p.PreviousRoot)
				detail = fmt.Sprintf("expected %s, got %s", older.RootHash, *p.PreviousRoot)
				a.integrityViolations.Add(ctx, 1, otelmetric.WithAttributes(
					attribute.String("violation_type", "chain_linkage_broken"),
					attribute.String("sweep_type", sweepType),
				))
				a.persistViolation(ctx, orgID, p.ID, "chain_linkage_broken", map[string]any{
					"expected_previous": older.RootHash,
					"actual_previous":   *p.PreviousRoot,
					"older_proof_id":    older.ID,
				})
			default:
				linkPassed = true
			}
			results = append(results, storage.IntegrityAuditResult{
				OrgID: orgID, ProofID: p.ID, CheckType: "chain_linkage",
				Passed: linkPassed, SweepType: sweepType, Detail: detail, CheckedAt: now,
			})
		}
	}

	return results
}

// persistViolation writes an integrity violation to the database with retry.
// This is the durable counterpart to the INTEGRITY VIOLATION log messages —
// the log is for operators, this record survives log rotation and is queryable
// for incidents.
//
// Because the entire purpose of the violations table is to provide a durable
// record that survives log rotation, a transient Postgres error (connection
// blip, disk full) must not silently swallow the violation. We retry up to 3
// times with exponential backoff before giving up. The violation payload is
// small and the insert is idempotent (UUID is generated once, before retries).
func (a *App) persistViolation(ctx context.Context, orgID, proofID uuid.UUID, violationType string, details map[string]any) {
	v := storage.IntegrityViolation{
		ID:            uuid.New(),
		OrgID:         orgID,
		ProofID:       proofID,
		ViolationType: violationType,
		Details:       details,
		CreatedAt:     time.Now(),
	}

	const maxRetries = 3
	backoff := 500 * time.Millisecond

	var lastErr error
	for attempt := range maxRetries {
		if err := a.db.CreateIntegrityViolation(ctx, v); err != nil {
			lastErr = err
			a.logger.Warn("integrity audit: violation persist attempt failed, will retry",
				"org_id", orgID, "proof_id", proofID, "violation_type", violationType,
				"attempt", attempt+1, "max_retries", maxRetries, "error", err)

			select {
			case <-ctx.Done():
				a.logger.Error("integrity audit: context cancelled during violation persist retry — violation detected but not durably stored",
					"org_id", orgID, "proof_id", proofID, "violation_type", violationType, "error", ctx.Err())
				return
			case <-time.After(backoff):
				backoff *= 2
			}
			continue
		}
		return // success
	}

	a.logger.Error("integrity audit: exhausted retries for violation persist — violation detected but not durably stored",
		"org_id", orgID, "proof_id", proofID, "violation_type", violationType,
		"attempts", maxRetries, "last_error", lastErr)
}

func buildIntegrityProofs(ctx context.Context, db *storage.DB, logger *slog.Logger) {
	orgIDs, err := db.ListOrganizationIDs(ctx)
	if err != nil {
		logger.Warn("integrity proof: list orgs failed", "error", err)
		return
	}

	now := time.Now().UTC()

	for _, orgID := range orgIDs {
		latest, err := db.GetLatestIntegrityProof(ctx, orgID)
		if err != nil {
			logger.Warn("integrity proof: get latest failed", "error", err, "org_id", orgID)
			continue
		}

		batchStart := time.Time{}
		var previousRoot *string
		if latest != nil {
			batchStart = latest.BatchEnd
			previousRoot = &latest.RootHash
		}

		hashes, err := db.GetDecisionHashesForBatch(ctx, orgID, batchStart, now)
		if err != nil {
			logger.Warn("integrity proof: get hashes failed", "error", err, "org_id", orgID)
			continue
		}
		if len(hashes) == 0 {
			continue
		}

		root, err := integrity.BuildMerkleRoot(hashes)
		if err != nil {
			logger.Warn("integrity proof: merkle root construction failed", "error", err, "org_id", orgID)
			continue
		}

		proofID := uuid.New()
		proof := storage.IntegrityProof{
			ID:            proofID,
			OrgID:         orgID,
			BatchStart:    batchStart,
			BatchEnd:      now,
			DecisionCount: len(hashes),
			RootHash:      root,
			PreviousRoot:  previousRoot,
			CreatedAt:     now,
		}

		if err := db.CreateIntegrityProof(ctx, proof); err != nil {
			logger.Warn("integrity proof: create failed", "error", err, "org_id", orgID)
			continue
		}

		// Snapshot the leaf hashes so verification survives retention purge
		// and GDPR erasure (which delete or mutate the decisions table).
		if err := db.CreateProofLeaves(ctx, proofID, orgID, hashes); err != nil {
			logger.Warn("integrity proof: save leaves failed", "error", err, "org_id", orgID, "proof_id", proofID)
			// The proof itself was created; leaves can be backfilled later.
			// Verification will fall back to GetDecisionHashesForBatch.
		}

		logger.Info("integrity proof created",
			"org_id", orgID,
			"decisions", len(hashes),
			"root_hash", root[:16]+"...",
		)
	}
}
