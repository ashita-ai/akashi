package conflicts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ashita-ai/akashi/internal/compact"
)

// ValidateInput holds all context needed for relationship classification.
type ValidateInput struct {
	OutcomeA string
	OutcomeB string
	TypeA    string
	TypeB    string
	AgentA   string
	AgentB   string
	CreatedA time.Time
	CreatedB time.Time

	// Enrichment fields — may be empty when context is unavailable.
	ReasoningA        string // decision reasoning
	ReasoningB        string
	ProjectA          string // from agent_context["project"] (or legacy "repo")
	ProjectB          string
	TaskA             string // from agent_context["task"]
	TaskB             string
	SessionIDA        string // UUID string
	SessionIDB        string
	FullOutcomeA      string // full outcome when OutcomeA is a claim fragment
	FullOutcomeB      string
	BranchA           string // git branch from agent_context
	BranchB           string
	TicketRefsA       []string
	TicketRefsB       []string
	TopicSimilarity   float64 // decision-level embedding similarity (0–1); 0 means unavailable
	PrecedentLinked   bool    // true when one decision cites the other via precedent_ref
	OutcomeSimilarity float64 // outcome embedding similarity (0–1); 0 means unavailable
}

// ValidationResult holds the structured output from an LLM validation call.
type ValidationResult struct {
	Relationship string // contradiction, supersession, complementary, refinement, unrelated
	Explanation  string
	Category     string // factual, assessment, strategic, temporal
	Severity     string // critical, high, medium, low

	// SharedQuestion is the single question the two decisions answer
	// differently. The prompt requires it for a CONTRADICTION verdict; a
	// verdict that cannot name one is downgraded to complementary by
	// ParseValidatorResponse. Empty for every other relationship.
	SharedQuestion string

	// SupersedingSide is the side the validator named on its REPLACES line as
	// retiring the other: "A" or "B", matching the Decision A / Decision B
	// labels formatPrompt writes. The prompt requires it for a SUPERSESSION
	// verdict; a verdict that cannot name a side is downgraded to refinement by
	// ParseValidatorResponse. Empty for every other relationship.
	//
	// This is the ONLY source of supersedes direction. It deliberately does not
	// fall back to timestamp order: the prompt refuses recency as evidence, and
	// re-deriving direction from ValidFrom one layer down reintroduced exactly
	// what the prompt rejects, breaking on backdated traces and reverts.
	SupersedingSide string

	// ReplacementEvidence is the replacement language the validator quoted
	// after the side token on the REPLACES line. Advisory: it is carried into
	// the supersedes-suggestion reason but does NOT gate the contract, because
	// free text cannot be validated and requiring it would only produce filler.
	// Empty for every other relationship.
	ReplacementEvidence string

	// DowngradedBy names the parser-enforced contract that rewrote the verdict:
	// "question" (a contradiction that named no disputed question) or "replaces"
	// (a supersession that named no superseding side). Empty when the model's
	// verdict was taken as given.
	//
	// It exists because a downgrade is otherwise indistinguishable downstream
	// from an honest complementary/refinement verdict: both land in the same
	// !IsConflict() branch, which logs at Debug and is therefore off at the
	// default info level. A prompt or parser regression that silently discards
	// valid verdicts would look exactly like a quiet corpus.
	DowngradedBy string
}

// IsConflict returns true if the relationship represents an actionable conflict.
func (r ValidationResult) IsConflict() bool {
	return r.Relationship == "contradiction" || r.Relationship == "supersession"
}

// Validator classifies the relationship between two decision outcomes.
// The embedding scorer finds candidates (cheap, fast); the validator classifies
// them (precise, slower). This two-stage design keeps false positives low
// without requiring an LLM call for every decision pair.
type Validator interface {
	Validate(ctx context.Context, input ValidateInput) (ValidationResult, error)
}

// validCategories and validSeverities define the allowed values for classification.
var validCategories = map[string]bool{"factual": true, "assessment": true, "strategic": true, "temporal": true}
var validSeverities = map[string]bool{"critical": true, "high": true, "medium": true, "low": true}

// validRelationships defines the allowed values for relationship classification.
var validRelationships = map[string]bool{
	"contradiction": true,
	"supersession":  true,
	"complementary": true,
	"refinement":    true,
	"unrelated":     true,
}

// formatPrompt builds the validation prompt with temporal, agent, project, and
// session context. The prompt is constructed dynamically to include only the
// context signals that are available, avoiding noise from empty fields.
func formatPrompt(input ValidateInput) string {
	timeDelta := input.CreatedB.Sub(input.CreatedA).Abs()
	deltaStr := formatDuration(timeDelta)

	agentContext := "the same agent"
	if input.AgentA != input.AgentB {
		agentContext = "different agents"
	}

	var b strings.Builder
	b.WriteString("You are a relationship classifier for an AI decision audit system.\n\n")

	// --- Decision by agent A ---
	// The "Decision A" / "Decision B" labels are the prompt's only stable way
	// to refer to a side: agent names are not unique (same-agent pairs are the
	// common case, see agentContext above) and timestamps are the clock the
	// REPLACES contract exists to stop treating as evidence. The DIFFERENT
	// BRANCHES hint below already used these labels without defining them.
	fmt.Fprintf(&b, "Decision A (agent %q, %s, recorded %s):\n%s\n",
		input.AgentA, input.TypeA, input.CreatedA.Format(time.RFC3339), input.OutcomeA)
	if input.FullOutcomeA != "" && input.FullOutcomeA != input.OutcomeA {
		fmt.Fprintf(&b, "[Full decision context: %s]\n", compact.Truncate(input.FullOutcomeA, 500))
	}
	if input.ReasoningA != "" {
		fmt.Fprintf(&b, "[Reasoning: %s]\n", compact.Truncate(input.ReasoningA, 300))
	}

	// --- Decision by agent B ---
	fmt.Fprintf(&b, "\nDecision B (agent %q, %s, recorded %s):\n%s\n",
		input.AgentB, input.TypeB, input.CreatedB.Format(time.RFC3339), input.OutcomeB)
	if input.FullOutcomeB != "" && input.FullOutcomeB != input.OutcomeB {
		fmt.Fprintf(&b, "[Full decision context: %s]\n", compact.Truncate(input.FullOutcomeB, 500))
	}
	if input.ReasoningB != "" {
		fmt.Fprintf(&b, "[Reasoning: %s]\n", compact.Truncate(input.ReasoningB, 300))
	}

	// --- Temporal and agent context ---
	fmt.Fprintf(&b, "\nContext: These decisions were recorded %s apart by %s.\n", deltaStr, agentContext)

	// --- Project context (#168: cross-project confusion) ---
	if input.ProjectA != "" && input.ProjectB != "" {
		if input.ProjectA != input.ProjectB {
			fmt.Fprintf(&b, "DIFFERENT PROJECTS: %q's decision is about %q, %q's decision is about %q. Decisions about different codebases are almost always UNRELATED.\n",
				input.AgentA, input.ProjectA, input.AgentB, input.ProjectB)
		} else {
			fmt.Fprintf(&b, "Same project: %s\n", input.ProjectA)
		}
	} else if input.AgentA != input.AgentB {
		// Repository names unavailable — guide the LLM to identify projects from outcome text.
		// Different agents frequently work on different codebases and use similar assessment
		// vocabulary (e.g. "comprehensive review", "aggregate score") without those reviews
		// being related. Cross-project confusion is the leading source of false positives.
		b.WriteString("PROJECT CONTEXT: Repository names are not recorded for these decisions. " +
			"Read the outcome text carefully for named codebases, products, or projects (e.g. proper nouns like product names, repository names, service names). " +
			"If the two decisions clearly refer to DIFFERENT named systems, classify as UNRELATED — different codebases cannot contradict each other. " +
			"Only classify as CONTRADICTION if both decisions are clearly about the SAME system and make incompatible claims about it.\n")
	}
	// --- Branch context (#692: cross-branch false positives) ---
	if input.BranchA != "" && input.BranchB != "" {
		if input.BranchA != input.BranchB {
			fmt.Fprintf(&b, "DIFFERENT BRANCHES: Decision A was made on branch %q; Decision B was made on branch %q. "+
				"Decisions on different branches often represent parallel work (e.g. both rebasing, both renumbering migrations) "+
				"rather than genuine disagreement. Consider whether this is parallel correct work or an actual conflict.\n",
				input.BranchA, input.BranchB)
		} else {
			fmt.Fprintf(&b, "Same branch: %s\n", input.BranchA)
		}
	} else if input.BranchA != "" || input.BranchB != "" {
		branch := input.BranchA
		if branch == "" {
			branch = input.BranchB
		}
		fmt.Fprintf(&b, "Branch context: one decision was on branch %q, the other has no branch recorded.\n", branch)
	}

	if input.TaskA != "" {
		fmt.Fprintf(&b, "Task (%s): %s\n", input.AgentA, compact.Truncate(input.TaskA, 100))
	}
	if input.TaskB != "" {
		fmt.Fprintf(&b, "Task (%s): %s\n", input.AgentB, compact.Truncate(input.TaskB, 100))
	}

	// --- Ticket context (#717 follow-up: disjoint-ticket false positives) ---
	if len(input.TicketRefsA) > 0 && len(input.TicketRefsB) > 0 {
		refsA := strings.Join(input.TicketRefsA, ", ")
		refsB := strings.Join(input.TicketRefsB, ", ")
		if ticketRefsOverlap(input.TicketRefsA, input.TicketRefsB) {
			fmt.Fprintf(&b, "Shared ticket context: %s vs %s. Shared ticket references are evidence these decisions may address the same work item.\n",
				refsA, refsB)
		} else {
			fmt.Fprintf(&b, "DIFFERENT TICKETS: %q's decision references %s; %q's decision references %s. "+
				"Non-overlapping ticket references usually mean separate work items, even inside the same repository or component. "+
				"Classify as UNRELATED or COMPLEMENTARY unless both decisions explicitly address the same specific design question, "+
				"or one decision explicitly says it supersedes/rejects the other.\n",
				input.AgentA, refsA, input.AgentB, refsB)
		}
	}

	// --- Session context (#170: temporal refinement) ---
	if input.SessionIDA != "" && input.SessionIDB != "" && input.SessionIDA == input.SessionIDB {
		b.WriteString("SAME SESSION: Both decisions were recorded in the same work session. Sequential decisions are typically REFINEMENT or COMPLEMENTARY, not contradictions.\n")
	}

	// --- Topic similarity signal ---
	// When embedding similarity is high and agents differ, flag it explicitly.
	// Bi-encoders place same-topic decisions close together regardless of stance,
	// so high similarity here means "same domain" — not "same conclusion".
	if input.TopicSimilarity >= 0.70 && input.AgentA != input.AgentB {
		fmt.Fprintf(&b, "HIGH TOPIC OVERLAP: Embeddings show %.0f%% topic similarity, meaning both decisions address the same domain. Check whether the agents take OPPOSITE STANCES on the same specific question.\n",
			input.TopicSimilarity*100)
	}

	// --- Outcome similarity signal ---
	// When outcome embeddings are highly similar, the decisions likely agree.
	// This is distinct from topic similarity (which measures domain overlap):
	// high outcome similarity means the conclusions themselves are close.
	if input.OutcomeSimilarity >= 0.80 {
		fmt.Fprintf(&b, "HIGH OUTCOME SIMILARITY: Outcome embeddings show %.0f%% similarity. "+
			"When outcomes are this similar, the decisions likely AGREE on their conclusions "+
			"even if they address different aspects or layers. "+
			"Classify as COMPLEMENTARY unless they make genuinely incompatible claims.\n",
			input.OutcomeSimilarity*100)
	}

	// --- Precedent chain supersession hint (#452) ---
	// When one decision explicitly cites the other via precedent_ref and
	// contains supersession keywords, the heuristic filter passed it through
	// for LLM validation. Hint the LLM that this is a linked pair where the
	// later decision may reverse the earlier one.
	if input.PrecedentLinked {
		b.WriteString("PRECEDENT LINK: One decision explicitly cites the other as its precedent. " +
			"The later decision may REFINE the earlier one (building on it) or SUPERSEDE it (reversing the choice). " +
			"Pay close attention to whether the later decision's outcome contradicts or reverses the earlier decision's conclusion. " +
			"If it does, classify as SUPERSESSION.\n")
	}

	// --- Decision type workflow hint ---
	// When decision types suggest a sequential workflow (e.g., a review followed
	// by a fix), inject a strong hint to reduce false positives from the LLM
	// misclassifying cause-and-effect pairs as contradictions.
	if isWorkflowPair(input.TypeA, input.TypeB) {
		fmt.Fprintf(&b, "WORKFLOW PATTERN: Decision types %q → %q suggest a sequential workflow (analysis/review followed by implementation/fix). "+
			"These are almost always REFINEMENT or COMPLEMENTARY — one decision identifies issues, the other resolves them. "+
			"Only classify as CONTRADICTION if they make genuinely incompatible claims about the same question.\n",
			input.TypeA, input.TypeB)
	}

	// --- Temporal re-measurement hint (#705) ---
	// Two review-type decisions of the same system, recorded weeks apart,
	// are re-measurements rather than contradictions. The structural filter
	// in the scorer already suppresses the strict same-project case before
	// reaching the LLM; this hint covers the borderline cases that get past
	// it (missing project metadata, cross-org alias) so the model has the
	// right interpretive frame.
	if reviewTypes[strings.ToLower(input.TypeA)] && reviewTypes[strings.ToLower(input.TypeB)] && timeDelta >= temporalReassessmentWindow {
		fmt.Fprintf(&b, "TEMPORAL RE-MEASUREMENT: Both decisions assess the same kind of subject and were recorded %s apart. "+
			"Quantitative observations (rates, percentages, scores, counts) are time-bound — different numbers in two snapshots reflect natural drift over time, NOT contradiction. "+
			"Classify as REFINEMENT (the newer updates the older) or SUPERSESSION (the newer explicitly replaces the older), never CONTRADICTION, unless the later decision EXPLICITLY states the earlier conclusion was wrong.\n",
			deltaStr)
	}

	// --- Classification instructions ---
	//
	// This section is deliberately short. Its predecessor accumulated ~10 rounds
	// of "IMPORTANT for <failure class>" hedges, each added after an audit found
	// a new false-positive shape, and the model learned to ignore all of them:
	// full-corpus gold labelling (2026-08-10, 2,772 pairs) measured the judge
	// emitting CONTRADICTION for 97.8% of pairs, including 596 of the 627 true
	// supersessions. Enumerating what is *not* a contradiction does not work;
	// giving one decisive test and forcing the model to name the disputed
	// question does. The dispute-vs-timeline framing below scored Cohen's
	// kappa 0.766 against an independent rater on the same corpus.
	b.WriteString(`
Classify the RELATIONSHIP between these two decisions:

- CONTRADICTION: Both decisions are LIVE and take incompatible positions on the same specific question. Someone must choose between them.
- SUPERSESSION: One work stream evolving through time — the later decision revises, replaces, reverts, or corrects the earlier one as normal progress. Sequential states of ONE effort.
- COMPLEMENTARY: Same topic or system, compatible positions — different subsystems, phases, incidents, tickets, or aspects. Both hold simultaneously.
- REFINEMENT: The later decision deepens or implements the earlier one without changing its position.
- UNRELATED: Shared vocabulary only; different subject matter.

APPLY THESE TESTS IN ORDER. Stop at the first one that fits.

1. SAME QUESTION? Name the single specific question both decisions answer. If they address different
   work items, subsystems, incidents, tickets, or scopes, there is no shared question →
   COMPLEMENTARY (related work) or UNRELATED (different subject matter). Stop here.

2. DID ONE DECISION EXPLICITLY RETIRE THE OTHER? Supersession requires stated evidence of
   replacement — one decision reverts, replaces, withdraws, or corrects the other's position, or
   refers to it as done, obsolete, or changed. Being recorded later is NOT evidence of replacement:
   every pair you see has a time order, so ordering alone means nothing. Name the retiring side on
   the REPLACES line. Without explicit replacement language, this is not SUPERSESSION.

3. ARE BOTH POSITIONS STILL LIVE AND INCOMPATIBLE? If the two decisions answer the same question
   differently and neither has retired the other, someone must still choose between them →
   CONTRADICTION. Two agents landing opposite answers to one question is the case this system exists
   to catch; do not soften it to REFINEMENT because the decisions are polite or sequential.

CONTRADICTION REQUIRES A NAMED QUESTION:
State on the QUESTION line the single question the two decisions answer differently, in one clause
("whether to run CreateFieldIndex at startup"). If you cannot name it, the answer is not CONTRADICTION.

SUPERSESSION REQUIRES A NAMED SIDE:
State on the REPLACES line which decision retired the other — A or B — then the replacement language
you found ("B: replaced REST v1 with gRPC"). Time order is not replacement language. If you cannot
name the side, the answer is not SUPERSESSION.

RELATIONSHIP: one of [contradiction, supersession, complementary, refinement, unrelated]
QUESTION: the single disputed question (required for contradiction; otherwise "n/a")
REPLACES: A or B, then the replacement language (required for supersession; otherwise "n/a")
CATEGORY: factual, assessment, strategic, or temporal
SEVERITY: critical, high, medium, or low
EXPLANATION: one sentence using agent names (not "Decision A" or "Decision B")`)

	return b.String()
}

func ticketRefsOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, ref := range a {
		seen[ref] = struct{}{}
	}
	for _, ref := range b {
		if _, ok := seen[ref]; ok {
			return true
		}
	}
	return false
}

// workflowPairs maps decision type pairs that represent sequential workflows
// (analysis/review followed by implementation/fix). When both types in a pair
// appear, the decisions are almost certainly cause-and-effect rather than
// contradictions. The key is the "earlier" type, the values are the "later" types.
var workflowPairs = map[string][]string{
	"code_review": {"bug_fix", "fix", "implementation", "refactor"},
	"assessment":  {"bug_fix", "fix", "implementation", "refactor", "architecture"},
	"review":      {"bug_fix", "fix", "implementation", "refactor"},
	"analysis":    {"bug_fix", "fix", "implementation", "architecture"},
	"audit":       {"bug_fix", "fix", "implementation", "refactor"},
}

// isWorkflowPair returns true if the two decision types suggest a sequential
// workflow (e.g., code_review → bug_fix) in either direction. This is used
// to inject a strong hint into the LLM prompt to reduce false positives.
func isWorkflowPair(typeA, typeB string) bool {
	a := strings.ToLower(typeA)
	b := strings.ToLower(typeB)
	if followers, ok := workflowPairs[a]; ok {
		for _, f := range followers {
			if b == f {
				return true
			}
		}
	}
	if followers, ok := workflowPairs[b]; ok {
		for _, f := range followers {
			if a == f {
				return true
			}
		}
	}
	return false
}

// formatDuration produces a human-readable duration string.
func formatDuration(d time.Duration) string {
	hours := d.Hours()
	switch {
	case hours < 1:
		return fmt.Sprintf("%.0f minutes", d.Minutes())
	case hours < 24:
		return fmt.Sprintf("%.1f hours", hours)
	default:
		return fmt.Sprintf("%.1f days", hours/24)
	}
}

// ParseValidatorResponse extracts the relationship, category, severity, and
// explanation from an LLM response. If parsing fails, returns an error to
// enforce fail-safe behavior: ambiguous responses are treated as rejections.
func ParseValidatorResponse(response string) (ValidationResult, error) {
	lines := strings.Split(strings.TrimSpace(response), "\n")

	var relationship, explanation, category, severity, question, replaces string
	// legacyVerdict marks a response that carried only the pre-taxonomy
	// "VERDICT: yes/no" form. Those responses come from a prompt that never
	// asked for a disputed question, so the contradiction contract below must
	// not be applied to them — it would silently downgrade every legacy result.
	var legacyVerdict bool
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Strip leading markdown bold/italic markers that some LLMs add.
		// e.g. "**RELATIONSHIP:** CONTRADICTION" → "RELATIONSHIP:** CONTRADICTION"
		trimmed = strings.TrimLeft(trimmed, "*_")
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "relationship:"):
			// Trim markdown markers that can appear between ":" and the value.
			relationship = strings.ToLower(strings.Trim(strings.TrimSpace(trimmed[len("relationship:"):]), "*_ "))
			// An explicit RELATIONSHIP means the model saw the current prompt
			// and owes a disputed question. Clearing the flag here keeps that
			// true regardless of whether a legacy VERDICT line came first —
			// otherwise line ordering alone decides whether the contract applies.
			legacyVerdict = false
		case strings.HasPrefix(lower, "verdict:"):
			// Backward compatibility: map old-style yes/no to relationship.
			verdict := strings.ToLower(strings.Trim(strings.TrimSpace(trimmed[len("verdict:"):]), "*_ "))
			if relationship == "" {
				switch verdict {
				case "yes":
					relationship = "contradiction"
					legacyVerdict = true
				case "no":
					relationship = "unrelated"
				}
			}
		case strings.HasPrefix(lower, "explanation:"):
			// TrimLeft only — preserve any intentional * inside the explanation text.
			explanation = strings.TrimLeft(strings.TrimSpace(trimmed[len("explanation:"):]), "*_ ")
		case strings.HasPrefix(lower, "question:"):
			question = strings.TrimLeft(strings.TrimSpace(trimmed[len("question:"):]), "*_ ")
		case strings.HasPrefix(lower, "replaces:"):
			// TrimLeft only — the value is a side token followed by free text.
			replaces = strings.TrimLeft(strings.TrimSpace(trimmed[len("replaces:"):]), "*_ ")
		case strings.HasPrefix(lower, "category:"):
			category = strings.ToLower(strings.Trim(strings.TrimSpace(trimmed[len("category:"):]), "*_ "))
		case strings.HasPrefix(lower, "severity:"):
			severity = strings.ToLower(strings.Trim(strings.TrimSpace(trimmed[len("severity:"):]), "*_ "))
		}
	}

	if relationship == "" {
		return ValidationResult{}, fmt.Errorf("validator: no RELATIONSHIP or VERDICT line found in response")
	}

	// Normalize: strip any brackets or extra text (e.g. "[contradiction]" → "contradiction").
	relationship = strings.Trim(relationship, "[] ")

	// Normalize common LLM truncations to their canonical form.
	// Some models shorten "refinement" → "refine", "supersession" → "supersede", etc.
	switch relationship {
	case "refine":
		relationship = "refinement"
	case "supersede":
		relationship = "supersession"
	case "contradict":
		relationship = "contradiction"
	case "complement":
		relationship = "complementary"
	}

	if !validRelationships[relationship] {
		return ValidationResult{}, fmt.Errorf("validator: unrecognized relationship %q", relationship)
	}

	// Normalize category and severity — ignore invalid values rather than failing.
	if !validCategories[category] {
		category = ""
	}
	if !validSeverities[severity] {
		severity = ""
	}

	question = normalizeSharedQuestion(question)

	// Parser-enforced contradiction contract. The prompt requires a
	// CONTRADICTION verdict to name the single question the two decisions
	// answer differently; a model that cannot name one has not identified a
	// dispute, only topical overlap. Downgrading here (rather than hinting
	// harder in the prompt) is deliberate: prompt hints for this failure class
	// were measured to be ignored — see the note on the classification block.
	//
	// Downgrade target is complementary, not unrelated: these pairs cleared
	// embedding similarity, so they are topically related by construction.
	var downgradedBy string
	if relationship == "contradiction" && question == "" && !legacyVerdict {
		relationship = "complementary"
		downgradedBy = "question"
		if explanation != "" {
			explanation += " "
		}
		explanation += "(downgraded: validator named no disputed question)"
	}
	if relationship != "contradiction" {
		question = ""
	}

	replacesSide, replacesEvidence := parseReplacesLine(replaces)

	// Parser-enforced supersession contract, symmetric to the contradiction
	// contract above. A SUPERSESSION verdict must name which side retired the
	// other; a model that cannot name one has observed a timeline, not a
	// replacement. Enforced here rather than in the prompt for the same
	// measured reason as the question contract.
	//
	// Downgrade target is refinement, not complementary: the model asserted one
	// decision evolves from the other, and that claim survives even when the
	// replacement cannot be located. refinement is not IsConflict(), so a
	// downgraded verdict writes nothing at all — no conflict AND no supersedes
	// suggestion. That is deliberate: recording a link in a guessed direction is
	// worse than recording nothing, because the link is durable and agent-facing.
	//
	// No legacyVerdict exemption is applied, and none is needed: the legacy
	// "VERDICT: yes/no" form can only ever yield "contradiction" or "unrelated".
	// "supersession" is reachable only from an explicit RELATIONSHIP line (or
	// its "supersede" truncation alias, likewise RELATIONSHIP-only), so
	// legacyVerdict is provably false here. Adding "&& !legacyVerdict" would be
	// dead code implying a path that cannot exist.
	if relationship == "supersession" && replacesSide == "" {
		relationship = "refinement"
		downgradedBy = "replaces"
		if explanation != "" {
			explanation += " "
		}
		explanation += "(downgraded: validator named no superseding side)"
	}
	if relationship != "supersession" {
		replacesSide, replacesEvidence = "", ""
	}

	return ValidationResult{
		Relationship:        relationship,
		Explanation:         explanation,
		Category:            category,
		Severity:            severity,
		SharedQuestion:      question,
		SupersedingSide:     replacesSide,
		ReplacementEvidence: replacesEvidence,
		DowngradedBy:        downgradedBy,
	}, nil
}

// placeholderQuestions are the values models emit to mean "no question here".
// They must not satisfy the contradiction contract.
var placeholderQuestions = map[string]bool{
	"n/a": true, "na": true, "none": true, "null": true, "nil": true,
	"not applicable": true, "-": true, "": true,
}

// normalizeValue strips the markdown, bracket, and quote wrapping models put
// around a field value. Three interleaved passes, so "**[n/a]**" unwraps: the
// markers can nest either way round.
func normalizeValue(v string) string {
	v = strings.Trim(strings.TrimSpace(v), "*_ ")
	v = strings.Trim(strings.TrimSpace(v), "[]\"'")
	return strings.TrimSpace(strings.Trim(v, "*_ "))
}

// normalizeSharedQuestion trims a QUESTION value and collapses placeholders to
// the empty string so the contradiction contract treats them as unanswered.
// A bold-wrapped placeholder ("**n/a**") must still be recognised — the key
// parses fine, so a survivor would satisfy the contract with no question at all.
func normalizeSharedQuestion(q string) string {
	q = normalizeValue(q)
	if placeholderQuestions[strings.ToLower(strings.TrimRight(q, ".!"))] {
		return ""
	}
	return q
}

// replacesSeparators are the characters a model may put between the side token
// and its quoted replacement language.
//
// The markdown and double-quote characters are here because normalizeValue only
// unwraps a value's outer ends: `**B**: replaced X` normalizes to `B**: replaced X`,
// leaving the closing `**` interior. Rejecting that discarded the exact shape the
// prompt asks for, and the discard was silent — the verdict was downgraded to
// refinement and the judge's finding thrown away.
//
// Deliberately absent: `/`, so `A/B` still fails, and `'`, so an apostrophe
// ("A's decision replaces B's") fails rather than parsing with mangled evidence.
// Both failures are downgrades, which is the safe direction.
const replacesSeparators = ":-–—|,.;\t *_\""

// replacesCoordinators open a remainder that continues an enumeration rather
// than quoting replacement language.
var replacesCoordinators = map[string]bool{
	"and": true, "or": true, "&": true, "+": true,
	"then": true, "vs": true, "versus": true, "plus": true,
}

// replacesRetractions are standalone words by which a model takes back the
// finding it just made. "A: no explicit replacement language found" names a side
// and then withdraws it; storing that as the justification for a durable link is
// worse than storing nothing.
var replacesRetractions = map[string]bool{
	"neither": true, "none": true, "n/a": true, "na": true, "nothing": true,
	"unclear": true, "ambiguous": true, "unknown": true, "no": true,
	"not": true, "cannot": true,
}

// replacesWords splits evidence into comparable words, trimming the punctuation
// models attach to them so "B," and "**B**" both compare as "b".
func replacesWords(evidence string) []string {
	fields := strings.Fields(strings.ToLower(evidence))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, strings.Trim(f, "*_[]\"'`,.;:!?()—–-"))
	}
	return out
}

// namesOppositeSide reports whether the evidence names the side that was NOT
// selected as a standalone word. The value's job is to pick one of two
// decisions, so a remainder that names the other one is an enumeration
// ("A, B", "A or B"), not a selection.
func namesOppositeSide(evidence, side string) bool {
	other := "b"
	if strings.EqualFold(side, "B") {
		other = "a"
	}
	words := replacesWords(evidence)
	for i, w := range words {
		if w != other {
			continue
		}
		// "decision b" is the same claim spelled longer.
		if i > 0 && words[i-1] == "decision" {
			return true
		}
		return true
	}
	return false
}

// enumeratesOrRetracts reports whether the evidence continues an enumeration or
// withdraws the finding instead of quoting replacement language.
func enumeratesOrRetracts(evidence string) bool {
	words := replacesWords(evidence)
	if len(words) == 0 {
		return false
	}
	if replacesCoordinators[words[0]] {
		return true
	}
	for _, w := range words {
		if replacesRetractions[w] {
			return true
		}
	}
	return false
}

// parseReplacesLine splits a REPLACES value into the side the validator named
// and the replacement language it quoted.
//
// The accepted side set is closed ("A" or "B"), which is what makes the
// supersession contract enforceable: unlike QUESTION, "non-placeholder implies
// valid" is not sound here, because the value's whole job is to select one of
// two decisions. Anything that is not unambiguously one side — "n/a", "both",
// "A/B", "ambiguous", a bare separator — fails the contract and returns "".
// Note the placeholder table above is subsumed: every placeholder spelling
// starts with 'n', '-', or is empty, none of which can be a side token.
func parseReplacesLine(raw string) (side, evidence string) {
	v := normalizeValue(raw)
	// Models sometimes spell the side as "Decision A"; strip the noise word so
	// "Decision B — replaced X" still resolves to B.
	if len(v) >= len("decision ") && strings.EqualFold(v[:len("decision ")], "decision ") {
		v = strings.TrimSpace(v[len("decision "):])
	}
	if v == "" {
		return "", ""
	}
	// Take the WHOLE leading run of letters as the candidate token. Testing only
	// the first byte read "Ambiguous" as A and "Both" as B — the two answers that
	// most clearly mean the model could not pick a side — and left the closed-set
	// property resting on the separator guard alone.
	i := 0
	for i < len(v) && isASCIILetter(v[i]) {
		i++
	}
	token := v[:i]
	switch token {
	case "A", "B", "a", "b":
		side = strings.ToUpper(token)
	default:
		return "", ""
	}
	rest := v[i:]
	// A real side token stands alone or is followed by a separator.
	if rest != "" {
		r, _ := utf8.DecodeRuneInString(rest)
		if !strings.ContainsRune(replacesSeparators, r) {
			return "", ""
		}
		// A LOWERCASE token separated from what follows by nothing but whitespace
		// is the English article far more often than a side label: "a bit unclear"
		// is not a selection of A. "b — replaced X" is a selection of B, so the
		// test is what the whitespace leads to, not the case on its own.
		if token == "a" || token == "b" {
			if after := strings.TrimLeft(rest, " \t"); len(after) != len(rest) {
				ar, _ := utf8.DecodeRuneInString(after)
				if after == "" || !strings.ContainsRune(replacesSeparators, ar) || ar == ' ' || ar == '\t' {
					return "", ""
				}
			}
		}
	}
	evidence = normalizeValue(strings.TrimLeft(rest, replacesSeparators))
	// The value's job is to SELECT one side. A remainder naming the other side,
	// opening with a coordinator, or retracting the finding means the model
	// enumerated or gave up rather than choosing — and the prompt's own response
	// template ("A or B, then the replacement language") parses as a bare side
	// selection unless this rejects it, so an echoed template would otherwise
	// write a guessed direction into a durable, agent-facing link.
	if namesOppositeSide(evidence, side) || enumeratesOrRetracts(evidence) {
		return "", ""
	}
	return side, evidence
}

// isASCIILetter reports whether b is an unaccented ASCII letter. Side tokens are
// a closed two-value set, so no wider alphabet is needed.
func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// NoopValidator marks candidates as unvalidated when no LLM is configured.
// Returns "unvalidated" relationship so downstream consumers can distinguish
// between LLM-confirmed conflicts and candidates that were never classified.
// Previously returned "contradiction" which caused 100% false positive rate
// for deployments without an LLM provider.
type NoopValidator struct{}

func (NoopValidator) Validate(_ context.Context, _ ValidateInput) (ValidationResult, error) {
	return ValidationResult{
		Relationship: "unvalidated",
		Explanation:  "no LLM validator configured — candidate requires manual review or LLM validation",
	}, nil
}

// perCallTimeout is the maximum time for a single LLM validation call to an
// external API (OpenAI). Separate from the scorer's overall context timeout
// so one slow call doesn't block the entire scoring pass.
const perCallTimeout = 15 * time.Second

// ollamaPerCallTimeout is higher than perCallTimeout to account for local model
// cold-start on the warmup call (model must be loaded from disk on first use)
// and slower CPU/GPU inference. A 3B model on CPU can take 20-60s to produce
// its first token; subsequent calls with keep_alive=-1 are much faster.
const ollamaPerCallTimeout = 90 * time.Second

// OllamaValidator validates conflict candidates using a local Ollama chat model.
// Reuses the existing OLLAMA_URL configuration. The model should be a text
// generation model (e.g., qwen3.5:9b), not an embedding model.
type OllamaValidator struct {
	baseURL    string
	model      string
	numThreads int // 0 = let Ollama decide; >0 = cap inference to this many CPU threads
	httpClient *http.Client
}

// NewOllamaValidator creates a validator that calls Ollama's chat API.
// numThreads caps the CPU threads Ollama uses per inference call (0 = Ollama default).
// Recommended: floor(runtime.NumCPU()/3) to leave headroom for the server and embeddings.
func NewOllamaValidator(baseURL, model string, numThreads int) *OllamaValidator {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &OllamaValidator{
		baseURL:    baseURL,
		model:      model,
		numThreads: numThreads,
		httpClient: &http.Client{
			// HTTP timeout must exceed ollamaPerCallTimeout to avoid a
			// transport-level close before the context deadline fires.
			Timeout: ollamaPerCallTimeout + 5*time.Second,
		},
	}
}

// ollamaOpts returns the options object for Ollama requests, or nil if no
// options need to be set (e.g. numThreads == 0 means use Ollama's default).
func (v *OllamaValidator) ollamaOpts() *ollamaOptions {
	if v.numThreads > 0 {
		return &ollamaOptions{NumThread: v.numThreads}
	}
	return nil
}

// Warmup loads the model into Ollama's memory before the first real validation
// call. Without this, the first backfill request pays the full cold-start
// penalty (model load from disk) which can exceed 60s on CPU. Warmup sends a
// minimal prompt; the response is discarded. It is non-fatal if it fails.
func (v *OllamaValidator) Warmup(ctx context.Context) error {
	warmCtx, cancel := context.WithTimeout(ctx, ollamaPerCallTimeout)
	defer cancel()

	body, _ := json.Marshal(ollamaChatRequest{
		Model:     v.model,
		Messages:  []ollamaChatMessage{{Role: "user", Content: "hi"}},
		Stream:    false,
		KeepAlive: "72h",
		Options:   v.ollamaOpts(),
	})
	req, err := http.NewRequestWithContext(warmCtx, http.MethodPost, v.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ollama warmup: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama warmup: request: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama warmup: status %d", resp.StatusCode)
	}
	return nil
}

type ollamaChatRequest struct {
	Model     string              `json:"model"`
	Messages  []ollamaChatMessage `json:"messages"`
	Stream    bool                `json:"stream"`
	KeepAlive string              `json:"keep_alive,omitempty"` // "72h" keeps model in RAM for 3 days (effectively permanent for dev sessions).
	Options   *ollamaOptions      `json:"options,omitempty"`
}

type ollamaOptions struct {
	NumThread int `json:"num_thread,omitempty"` // CPU threads to use for inference. 0 = Ollama default (all cores).
}

type ollamaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

func (v *OllamaValidator) Validate(ctx context.Context, input ValidateInput) (ValidationResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, ollamaPerCallTimeout)
	defer cancel()

	prompt := formatPrompt(input)

	body, err := json.Marshal(ollamaChatRequest{
		Model: v.model,
		Messages: []ollamaChatMessage{
			{Role: "user", Content: prompt},
		},
		Stream:    false,
		KeepAlive: "72h", // Keep model loaded in RAM between calls; avoids cold-start penalty.
		Options:   v.ollamaOpts(),
	})
	if err != nil {
		return ValidationResult{}, fmt.Errorf("ollama validator: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, v.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return ValidationResult{}, fmt.Errorf("ollama validator: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return ValidationResult{}, fmt.Errorf("ollama validator: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return ValidationResult{}, fmt.Errorf("ollama validator: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ValidationResult{}, fmt.Errorf("ollama validator: decode response: %w", err)
	}

	return ParseValidatorResponse(result.Message.Content)
}

// OpenAIValidator validates conflict candidates using the OpenAI chat API.
// Defaults to gpt-4o-mini for cost efficiency. Reuses the existing OPENAI_API_KEY.
type OpenAIValidator struct {
	apiKey     string
	model      string
	timeout    time.Duration
	httpClient *http.Client
}

// OpenAIOption customizes an OpenAIValidator.
type OpenAIOption func(*OpenAIValidator)

// WithRequestTimeout overrides the per-call deadline.
//
// The default suits chat-completion models, which answer in a few seconds.
// Reasoning models spend far longer before emitting a first token: a gold-set
// run against gpt-5 at the default deadline failed 159 of 200 calls with
// context deadline exceeded, and — because the scorer treats a validation
// error as fail-safe and skips the candidate — that presents as a quiet drop
// in detections rather than as an error anyone notices. Any deployment
// pointing AKASHI_CONFLICT_OPENAI_MODEL at a reasoning model must raise this.
func WithRequestTimeout(d time.Duration) OpenAIOption {
	return func(v *OpenAIValidator) {
		if d > 0 {
			v.timeout = d
			v.httpClient.Timeout = d + 5*time.Second
		}
	}
}

// NewOpenAIValidator creates a validator that calls the OpenAI chat completions API.
func NewOpenAIValidator(apiKey, model string, opts ...OpenAIOption) *OpenAIValidator {
	if model == "" {
		model = "gpt-4o-mini"
	}
	v := &OpenAIValidator{
		apiKey:  apiKey,
		model:   model,
		timeout: perCallTimeout,
		httpClient: &http.Client{
			Timeout: perCallTimeout + 5*time.Second,
		},
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

type openAIChatRequest struct {
	Model    string              `json:"model"`
	Messages []openAIChatMessage `json:"messages"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (v *OpenAIValidator) Validate(ctx context.Context, input ValidateInput) (ValidationResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	prompt := formatPrompt(input)

	body, err := json.Marshal(openAIChatRequest{
		Model: v.model,
		Messages: []openAIChatMessage{
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return ValidationResult{}, fmt.Errorf("openai validator: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ValidationResult{}, fmt.Errorf("openai validator: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.apiKey)

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return ValidationResult{}, fmt.Errorf("openai validator: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return ValidationResult{}, fmt.Errorf("openai validator: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ValidationResult{}, fmt.Errorf("openai validator: decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return ValidationResult{}, fmt.Errorf("openai validator: no choices in response")
	}

	return ParseValidatorResponse(result.Choices[0].Message.Content)
}
