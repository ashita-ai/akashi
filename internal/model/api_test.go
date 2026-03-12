package model_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

// ptr is a convenience helper for pointer literals in test cases.
func ptr[T any](v T) *T { return &v }

// ---- ValidateTraceDecision -----------------------------------------------

func TestValidateTraceDecision_HappyPath(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "architecture",
		Outcome:      "chose PostgreSQL",
		Confidence:   0.9,
		Reasoning:    ptr("fits our workload"),
		Evidence: []model.TraceEvidence{
			{SourceType: "document", SourceURI: ptr("https://example.com/doc"), Content: "referenced doc"},
		},
	}
	assert.NoError(t, model.ValidateTraceDecision(d))
}

func TestValidateTraceDecision_DecisionTypeAtExactMax(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: strings.Repeat("x", model.MaxDecisionTypeLen),
		Outcome:      "ok",
	}
	assert.NoError(t, model.ValidateTraceDecision(d), "at the limit should pass")
}

func TestValidateTraceDecision_DecisionTypeOverMax(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: strings.Repeat("x", model.MaxDecisionTypeLen+1),
		Outcome:      "ok",
	}
	err := model.ValidateTraceDecision(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decision_type")
}

func TestValidateTraceDecision_OutcomeOverMax(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      strings.Repeat("x", model.MaxOutcomeLen+1),
	}
	err := model.ValidateTraceDecision(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outcome")
}

func TestValidateTraceDecision_ReasoningOverMax(t *testing.T) {
	bigReasoning := strings.Repeat("x", model.MaxReasoningLen+1)
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Reasoning:    &bigReasoning,
	}
	err := model.ValidateTraceDecision(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reasoning")
}

func TestValidateTraceDecision_NilReasoningIsValid(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Reasoning:    nil,
	}
	assert.NoError(t, model.ValidateTraceDecision(d))
}

func TestValidateTraceDecision_EvidenceWithBadSourceURI(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Evidence: []model.TraceEvidence{
			{SourceType: "document", SourceURI: ptr("javascript:alert(1)"), Content: "xss attempt"},
		},
	}
	err := model.ValidateTraceDecision(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evidence[0].source_uri")
}

func TestValidateTraceDecision_EvidenceNilSourceURIIsValid(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Evidence: []model.TraceEvidence{
			{SourceType: "tool_output", SourceURI: nil, Content: "no URI is fine"},
		},
	}
	assert.NoError(t, model.ValidateTraceDecision(d))
}

func TestValidateTraceDecision_SecondEvidenceItemFails(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Evidence: []model.TraceEvidence{
			{SourceType: "document", SourceURI: ptr("https://ok.example.com"), Content: "good"},
			{SourceType: "document", SourceURI: ptr("javascript:alert(1)"), Content: "bad"},
		},
	}
	err := model.ValidateTraceDecision(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evidence[1].source_uri")
}

// ---- ValidateSourceURI ---------------------------------------------------

func TestValidateSourceURI_ValidHTTP(t *testing.T) {
	assert.NoError(t, model.ValidateSourceURI("http://example.com/path"))
}

func TestValidateSourceURI_ValidHTTPS(t *testing.T) {
	assert.NoError(t, model.ValidateSourceURI("https://docs.example.com/api#section"))
}

func TestValidateSourceURI_ValidHTTPSWithQuery(t *testing.T) {
	assert.NoError(t, model.ValidateSourceURI("https://example.com/search?q=foo&bar=baz"))
}

func TestValidateSourceURI_ValidPublicIP(t *testing.T) {
	// 8.8.8.8 is a public IP — should pass.
	assert.NoError(t, model.ValidateSourceURI("https://8.8.8.8/resource"))
}

func TestValidateSourceURI_JavascriptSchemeRejected(t *testing.T) {
	err := model.ValidateSourceURI("javascript:alert(1)")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not allowed")
}

func TestValidateSourceURI_DataSchemeRejected(t *testing.T) {
	err := model.ValidateSourceURI("data:text/html,<script>alert(1)</script>")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not allowed")
}

func TestValidateSourceURI_VBScriptSchemeRejected(t *testing.T) {
	err := model.ValidateSourceURI("vbscript:msgbox(1)")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not allowed")
}

func TestValidateSourceURI_FileSchemeAllowed(t *testing.T) {
	// file: URIs are stored metadata; browsers block file: navigation from https pages.
	assert.NoError(t, model.ValidateSourceURI("file:///home/user/docs/adr.md"))
}

func TestValidateSourceURI_RelativePathAllowed(t *testing.T) {
	// Relative paths are the documented example in the MCP tool description.
	assert.NoError(t, model.ValidateSourceURI("adrs/007.md"))
}

func TestValidateSourceURI_BareFilenameAllowed(t *testing.T) {
	assert.NoError(t, model.ValidateSourceURI("example.com/path"))
}

func TestValidateSourceURI_FTPSchemeAllowed(t *testing.T) {
	// Non-http schemes other than the dangerous set are permitted; source_uri is metadata only.
	assert.NoError(t, model.ValidateSourceURI("ftp://files.example.com/file.txt"))
}

func TestValidateSourceURI_CredentialsRejected(t *testing.T) {
	err := model.ValidateSourceURI("https://user:pass@example.com/resource")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credentials")
}

func TestValidateSourceURI_NoHostRejected(t *testing.T) {
	// A URL with scheme but no host.
	err := model.ValidateSourceURI("https:///path/only")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host")
}

func TestValidateSourceURI_LocalhostRejected(t *testing.T) {
	err := model.ValidateSourceURI("http://localhost/service")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "localhost")
}

func TestValidateSourceURI_LocalhostWithPortRejected(t *testing.T) {
	err := model.ValidateSourceURI("http://localhost:8080/api")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "localhost")
}

func TestValidateSourceURI_LoopbackIPRejected(t *testing.T) {
	err := model.ValidateSourceURI("http://127.0.0.1/admin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private or loopback")
}

func TestValidateSourceURI_LoopbackIPAltRejected(t *testing.T) {
	err := model.ValidateSourceURI("http://127.255.255.255/admin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private or loopback")
}

func TestValidateSourceURI_RFC1918_10Rejected(t *testing.T) {
	err := model.ValidateSourceURI("http://10.0.0.1/internal")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private or loopback")
}

func TestValidateSourceURI_RFC1918_172Rejected(t *testing.T) {
	err := model.ValidateSourceURI("http://172.16.0.1/internal")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private or loopback")
}

func TestValidateSourceURI_RFC1918_172UpperBoundRejected(t *testing.T) {
	err := model.ValidateSourceURI("http://172.31.255.255/internal")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private or loopback")
}

func TestValidateSourceURI_RFC1918_192168Rejected(t *testing.T) {
	err := model.ValidateSourceURI("http://192.168.1.100/internal")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private or loopback")
}

func TestValidateSourceURI_LinkLocalRejected(t *testing.T) {
	err := model.ValidateSourceURI("http://169.254.1.1/metadata")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private or loopback")
}

func TestValidateSourceURI_IPv6LoopbackRejected(t *testing.T) {
	err := model.ValidateSourceURI("http://[::1]/service")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private or loopback")
}

func TestValidateSourceURI_IPv6UniqueLocalRejected(t *testing.T) {
	// fc00::/7 — unique-local IPv6
	err := model.ValidateSourceURI("http://[fc00::1]/internal")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private or loopback")
}

func TestValidateSourceURI_IPv6LinkLocalRejected(t *testing.T) {
	// fe80::/10 — link-local IPv6
	err := model.ValidateSourceURI("http://[fe80::1]/internal")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private or loopback")
}

// ---- Payload size caps (evidence, alternatives, metadata) -----------------

func TestValidateTraceDecision_EvidenceContentAtMax(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Evidence: []model.TraceEvidence{
			{SourceType: "document", Content: strings.Repeat("x", model.MaxEvidenceContentLen)},
		},
	}
	assert.NoError(t, model.ValidateTraceDecision(d), "at the limit should pass")
}

func TestValidateTraceDecision_EvidenceContentOverMax(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Evidence: []model.TraceEvidence{
			{SourceType: "document", Content: strings.Repeat("x", model.MaxEvidenceContentLen+1)},
		},
	}
	err := model.ValidateTraceDecision(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evidence[0].content")
}

func TestValidateTraceDecision_AlternativeLabelOverMax(t *testing.T) {
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Alternatives: []model.TraceAlternative{
			{Label: strings.Repeat("x", model.MaxAlternativeLabelLen+1)},
		},
	}
	err := model.ValidateTraceDecision(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alternatives[0].label")
}

func TestValidateTraceDecision_RejectionReasonOverMax(t *testing.T) {
	bigReason := strings.Repeat("x", model.MaxRejectionReasonLen+1)
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Alternatives: []model.TraceAlternative{
			{Label: "option-a", RejectionReason: &bigReason},
		},
	}
	err := model.ValidateTraceDecision(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alternatives[0].rejection_reason")
}

func TestValidateTraceDecision_TooManyAlternatives(t *testing.T) {
	alts := make([]model.TraceAlternative, model.MaxAlternativeCount+1)
	for i := range alts {
		alts[i] = model.TraceAlternative{Label: "opt"}
	}
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Alternatives: alts,
	}
	err := model.ValidateTraceDecision(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alternatives count")
}

func TestValidateTraceDecision_TooManyEvidence(t *testing.T) {
	evs := make([]model.TraceEvidence, model.MaxEvidenceCount+1)
	for i := range evs {
		evs[i] = model.TraceEvidence{SourceType: "document", Content: "data"}
	}
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Evidence:     evs,
	}
	err := model.ValidateTraceDecision(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evidence count")
}

func TestValidateTraceDecision_MaxAlternativesAndEvidencePass(t *testing.T) {
	alts := make([]model.TraceAlternative, model.MaxAlternativeCount)
	for i := range alts {
		alts[i] = model.TraceAlternative{Label: "opt"}
	}
	evs := make([]model.TraceEvidence, model.MaxEvidenceCount)
	for i := range evs {
		evs[i] = model.TraceEvidence{SourceType: "document", Content: "data"}
	}
	d := model.TraceDecision{
		DecisionType: "arch",
		Outcome:      "ok",
		Alternatives: alts,
		Evidence:     evs,
	}
	assert.NoError(t, model.ValidateTraceDecision(d), "exactly at max should pass")
}

// ---- ValidateMetadataSize -------------------------------------------------

func TestValidateMetadataSize_NilMap(t *testing.T) {
	assert.NoError(t, model.ValidateMetadataSize("metadata", nil))
}

func TestValidateMetadataSize_EmptyMap(t *testing.T) {
	assert.NoError(t, model.ValidateMetadataSize("metadata", map[string]any{}))
}

func TestValidateMetadataSize_SmallMap(t *testing.T) {
	m := map[string]any{"key": "value", "nested": map[string]any{"a": 1}}
	assert.NoError(t, model.ValidateMetadataSize("metadata", m))
}

func TestValidateMetadataSize_OverMax(t *testing.T) {
	// Create a map that serializes to > 16 KB.
	m := map[string]any{"big": strings.Repeat("x", model.MaxMetadataBytes)}
	err := model.ValidateMetadataSize("metadata", m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata")
	assert.Contains(t, err.Error(), "exceeds maximum size")
}
