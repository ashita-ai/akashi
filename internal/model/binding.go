package model

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Binding is the named thing a decision sets, and the value it sets it to.
//
// It exists because contradiction detection over prose is a screening problem
// at a ~3% base rate, while two decisions binding the same parameter to
// different values is a join. Classifying the corpus's 93 gold contradictions
// by shape found 27% take exactly this form; the rest have no key to join on
// and still require the judge.
type Binding struct {
	ID         uuid.UUID `json:"id,omitempty"`
	DecisionID uuid.UUID `json:"decision_id,omitempty"`
	OrgID      uuid.UUID `json:"org_id,omitempty"`

	// Parameter as the agent wrote it, e.g. "qdrant.create_field_index_on_startup"
	// or "cache TTL". Preserved for display.
	Parameter string `json:"parameter"`
	// Value as the agent wrote it, e.g. "true" or "5 minutes".
	Value string `json:"value"`

	// ParameterKey and ValueKey are the canonical forms the join runs on.
	// Populated by Canonicalize; callers do not supply them.
	ParameterKey string `json:"parameter_key,omitempty"`
	ValueKey     string `json:"value_key,omitempty"`
}

const (
	maxBindingParameterLen = 200
	maxBindingValueLen     = 500
	// MaxBindingsPerDecision bounds fan-out. A decision that claims to set
	// dozens of parameters is describing a changelog rather than a decision,
	// and each binding multiplies the join's candidate set.
	MaxBindingsPerDecision = 16
)

// separatorRun matches any run of characters that different agents use
// interchangeably between words of the same parameter name.
var separatorRun = regexp.MustCompile(`[\s._\-/:]+`)

// camelBoundary matches the seam between words in camelCase and PascalCase, so
// "CreateFieldIndex" and "create_field_index" reach the same key. The two
// alternations handle the ordinary seam (lower or digit, then upper) and the
// end of an acronym run ("HTTPServer" -> "http.server").
var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])|([A-Z]+)([A-Z][a-z])`)

// Canonicalize fills ParameterKey and ValueKey, and validates the binding.
//
// Canonicalization is deliberately shallow: case, surrounding whitespace, and
// separator style only. It is tempting to go further — unit-normalize "5m" and
// "300s", coerce "yes" to "true" — and that temptation must be resisted,
// because deciding two values mean the same thing requires knowing the
// parameter's type, which is not recorded. Over-normalizing merges genuinely
// different parameters and manufactures conflicts out of nothing, which is a
// worse failure than missing one: this feature's entire value is that a
// detection it produces is certain.
//
// The same reasoning caps the other direction. Under-normalizing only loses
// matches, and the judge still sees those pairs.
func (b *Binding) Canonicalize() error {
	b.Parameter = strings.TrimSpace(b.Parameter)
	b.Value = strings.TrimSpace(b.Value)

	if b.Parameter == "" {
		return fmt.Errorf("binding: parameter is required")
	}
	if b.Value == "" {
		return fmt.Errorf("binding: value is required for parameter %q", b.Parameter)
	}
	if len(b.Parameter) > maxBindingParameterLen {
		return fmt.Errorf("binding: parameter exceeds %d characters", maxBindingParameterLen)
	}
	if len(b.Value) > maxBindingValueLen {
		return fmt.Errorf("binding: value for %q exceeds %d characters", b.Parameter, maxBindingValueLen)
	}

	b.ParameterKey = canonicalParameterKey(b.Parameter)
	b.ValueKey = canonicalValueKey(b.Value)

	if b.ParameterKey == "" {
		return fmt.Errorf("binding: parameter %q normalizes to empty", b.Parameter)
	}
	if b.ValueKey == "" {
		return fmt.Errorf("binding: value %q normalizes to empty", b.Value)
	}
	return nil
}

// canonicalParameterKey lowercases, splits camel seams, and collapses
// separator runs to a single ".".
//
// "Qdrant.CreateFieldIndex_on_startup", "qdrant create field index on startup"
// and "qdrant-create-field-index-on-startup" all reduce to the same key, which
// is the realistic amount of spelling variation between two agents naming the
// same knob. Anything beyond that is guessing.
//
// Splitting camel seams is safe in a way that value normalization is not: the
// mapping is deterministic and structural, so it can merge two spellings of one
// name but cannot merge two different names.
func canonicalParameterKey(s string) string {
	// Split camel seams before lowercasing, since lowercasing destroys them.
	s = camelBoundary.ReplaceAllString(strings.TrimSpace(s), "${1}${3}.${2}${4}")
	return collapse(s)
}

// canonicalValueKey normalizes case and separators but deliberately does NOT
// split camel seams.
//
// Parameter names are identifier-like, so splitting them is safe and useful.
// Values are prose: a product name written "PostgreSQL Only" would split into
// "postgre.sql.only" while the equivalent "postgresql-only" would not, and the
// two would then read as a disagreement they are not. Values only need case and
// separator agreement.
func canonicalValueKey(s string) string {
	return collapse(strings.TrimSpace(s))
}

func collapse(s string) string {
	s = strings.ToLower(s)
	s = separatorRun.ReplaceAllString(s, ".")
	return strings.Trim(s, ".")
}

// CanonicalizeBindings validates a decision's bindings and rejects duplicates.
//
// A decision binding the same parameter twice is contradicting itself within a
// single trace, which is a caller bug rather than a conflict to detect, so it
// is refused at the door instead of being stored and re-discovered later.
func CanonicalizeBindings(bindings []Binding) ([]Binding, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	if len(bindings) > MaxBindingsPerDecision {
		return nil, fmt.Errorf("binding: %d bindings exceeds the limit of %d for one decision",
			len(bindings), MaxBindingsPerDecision)
	}
	seen := make(map[string]string, len(bindings))
	out := make([]Binding, 0, len(bindings))
	for i := range bindings {
		b := bindings[i]
		if err := b.Canonicalize(); err != nil {
			return nil, err
		}
		if prev, dup := seen[b.ParameterKey]; dup {
			return nil, fmt.Errorf("binding: parameter %q bound twice in one decision (as %q and %q)",
				b.ParameterKey, prev, b.Parameter)
		}
		seen[b.ParameterKey] = b.Parameter
		out = append(out, b)
	}
	return out, nil
}
