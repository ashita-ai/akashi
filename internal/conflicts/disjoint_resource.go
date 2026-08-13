//go:build !lite

package conflicts

import (
	"strings"

	"github.com/ashita-ai/akashi/internal/model"
)

// isDisjointResource returns true when two operational / investigation decisions
// on the same project act on entirely different infrastructure resources —
// different connector_/org_ identifiers — and therefore cannot contradict each
// other. A diagnosis of connector_57a42328 and a recovery of connector_9b38b47d
// touch different data planes; they routinely share dense incident vocabulary
// (restart_storm, kafka2pg, replication slot, wal_status, snapshot) that places
// them close in embedding space, and the validator then manufactures a
// CONTRADICTION between two unrelated incidents.
//
// This is the resource-identity sibling of isDisjointWorkItem. That filter keys
// on PR/ticket refs, which live operational traces do not carry — they name a
// connector_/org_ token instead — so it could never see this class. Together
// they are the membership-parity pair: work-item identity for code reviews and
// plans, resource identity for incident recovery. It is the dominant live FP
// class (2026-06-24 cross-connector restart-storm over-clustering) and the
// reason operational conflicts pile up despite the work-item filter.
//
// It also fills the resource-identity gap that isOperationalStateProgression
// documents as deferred: that filter suppresses same-project operational pairs
// only once they are >= operationalProgressionWindow apart, so different-resource
// pairs inside the same incident window (hours apart) slip past it. This filter
// has no time requirement — different resources cannot contradict regardless of
// when the two decisions were traced.
//
// Deliberately narrow, with the same discipline as isDisjointWorkItem:
//   - both decisions must be resourceScopedTypes. architecture / trade_off /
//     design forks are never eligible — a cross-cutting design disagreement can
//     legitimately span resources and must reach the validator.
//   - same non-empty project (operational traces are all project "mono"; an
//     untagged pair is never suppressed), mirroring the sibling filters.
//   - both must expose at least one extractable resource ref, and the two ref
//     sets must be provably disjoint by namespace (see
//     resourceRefsProvablyDisjoint). Disjointness is only judged WITHIN a
//     namespace: a different connector vs connector, or org vs org, is disjoint;
//     a connector vs an org is NOT comparable — the connector may be owned by
//     that org, and the scorer holds no connector→org mapping — so that pair
//     reaches the validator. Any shared connector/org, or any namespace present
//     on only one side, also sends the pair to the validator — a shared
//     resource is exactly where a genuine operational clash lives.
//   - not precedent-linked — an explicit lineage cite is left to the LLM.
//
// No data-safety guard here (removed 2026-07-23). The guard is load-bearing and
// deliberately chosen in the TEMPORAL sibling isOperationalStateProgression
// (decision efc42730): two decisions on the SAME writer a week apart can be one
// incident disagreeing with itself — verified against the Acme
// connector_redacted01 DATALOSS pause. That rationale cannot hold here, because a
// connector_/org_ id is the PHYSICAL identity of a data plane: connector_57a42328
// and connector_f924df29 are different incidents by construction, so a DATALOSS
// finding on one can neither be the same incident as, nor contradict, a finding
// on the other. The guard was copied here for "membership parity" with the
// temporal filter, but it only ever governed provably-disjoint pairs (this
// function returns true solely when resourceRefsProvablyDisjoint) and, measured
// against the live corpus, was a false-positive driver: "quarantine"/"corrupt"
// are ordinary pgstream design vocabulary, so the guard re-admitted the exact
// connector-disjoint over-clustering this filter exists to kill. Same-writer
// data-safety disagreements stay protected where the verified evidence put them,
// in isOperationalStateProgression. (isDisjointWorkItem keeps its guard on
// purpose: a ticket is an administrative label, not a physical identity, so two
// disjoint tickets CAN describe one incident — a deliberate, documented
// difference, not drift.)
//
// Failure mode is under-suppression: a decision that names its resource only by
// customer name ("Trellis") and not by a connector_/org_ token yields no ref,
// which disables the filter and sends the pair to the validator. So does a
// cross-namespace pair (a connector on one side, an org on the other) and any
// pair sharing a resource — never a wrongful suppression, because disjointness
// is only ever declared between same-namespace identifiers that genuinely
// differ.
//
// Scope: lives only in the cloud scorer (this file is !lite), alongside the rest
// of the structural suppression family.
func isDisjointResource(d, cand model.Decision) bool {
	if !resourceScopedTypes[strings.ToLower(d.DecisionType)] {
		return false
	}
	if !resourceScopedTypes[strings.ToLower(cand.DecisionType)] {
		return false
	}
	projA, projB := derefString(d.Project), derefString(cand.Project)
	if projA != projB || (projA == "" && projB == "") {
		return false
	}
	if isPrecedentLinked(d, cand) {
		return false
	}
	refsA := extractResourceRefs(d)
	refsB := extractResourceRefs(cand)
	if len(refsA) == 0 || len(refsB) == 0 {
		return false
	}
	return resourceRefsProvablyDisjoint(refsA, refsB)
}

// resourceRefsProvablyDisjoint reports whether two non-empty canonical
// resource-reference sets ("CONNECTOR-<hex>", "ORG-<hex>") describe provably
// different infrastructure — so a pair naming only these resources cannot be
// acting on the same data plane and is safe to suppress pre-LLM.
//
// Disjointness is only provable WITHIN a namespace. CONNECTOR-* names a single
// pipeline; ORG-* names a whole tenant, which owns many connectors. Two
// different connector ids are different pipelines, and two different org ids are
// different tenants — both provably disjoint. But a connector id and an org id
// are NOT comparable: the connector may be owned by that very org, and the
// scorer holds no connector→org mapping to rule it out. The original check
// flattened both namespaces into one set and ran a plain overlap test, so a
// decision naming only connector_57a42328 and one naming only its owning
// org_54b2e846 looked "disjoint" (no string in common) and were suppressed —
// even though they may describe the same incident on the same data plane. That
// is a wrongful pre-LLM suppression, the one failure mode this family exists to
// avoid.
//
// The fix types the refs by namespace and declares disjointness only when the
// comparison is apples-to-apples in every namespace:
//   - both sides must populate exactly the same set of namespaces. A
//     connector-only side and an org-only side — or a connector-only side and a
//     connector+org side — are not comparable, because there is a ref on one
//     side whose ownership relative to the other side is unknown, so the pair
//     reaches the validator.
//   - within every populated namespace the two id sets must be fully disjoint.
//     Any shared connector or shared org makes the pair overlap and reach the
//     validator.
//
// This is strictly more conservative than the flattened check: it can only send
// MORE pairs to the validator, never fewer (the safe, under-suppressing
// direction the whole family is tuned to). The dominant live FP class —
// connector-vs-connector, both sides naming only connectors — is unaffected:
// same namespace set {CONNECTOR}, disjoint ids, still suppressed.
func resourceRefsProvablyDisjoint(refsA, refsB []string) bool {
	byNsA := groupResourceRefsByNamespace(refsA)
	byNsB := groupResourceRefsByNamespace(refsB)
	if len(byNsA) != len(byNsB) {
		return false // different namespaces populated → not comparable
	}
	for ns, idsA := range byNsA {
		idsB, ok := byNsB[ns]
		if !ok {
			return false // a namespace present on one side only → not comparable
		}
		for id := range idsA {
			if _, shared := idsB[id]; shared {
				return false // a shared resource in this namespace → overlap
			}
		}
	}
	return true
}

// groupResourceRefsByNamespace partitions canonical resource refs into id sets
// keyed by their namespace prefix ("CONNECTOR", "ORG"). extractResourceRefs
// produces every ref as "<PREFIX>-<hex>" with exactly one hyphen, so the split
// is unambiguous; a malformed ref carrying no hyphen is skipped rather than
// mis-bucketed.
func groupResourceRefsByNamespace(refs []string) map[string]map[string]struct{} {
	byNs := make(map[string]map[string]struct{}, 2)
	for _, ref := range refs {
		ns, id, ok := strings.Cut(ref, "-")
		if !ok {
			continue
		}
		ids, exists := byNs[ns]
		if !exists {
			ids = make(map[string]struct{}, 1)
			byNs[ns] = ids
		}
		ids[id] = struct{}{}
	}
	return byNs
}
