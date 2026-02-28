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
			{SourceType: "document", SourceURI: ptr("file:///etc/passwd"), Content: "bad"},
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
	assert.Contains(t, err.Error(), "http or https")
}

func TestValidateSourceURI_FileSchemeRejected(t *testing.T) {
	err := model.ValidateSourceURI("file:///etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http or https")
}

func TestValidateSourceURI_NoSchemeRejected(t *testing.T) {
	err := model.ValidateSourceURI("example.com/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http or https")
}

func TestValidateSourceURI_FTPSchemeRejected(t *testing.T) {
	err := model.ValidateSourceURI("ftp://files.example.com/file.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http or https")
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
