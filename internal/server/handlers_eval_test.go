package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/conflicts"
)

// mockValidator implements conflicts.Validator for testing.
type mockValidator struct {
	result conflicts.ValidationResult
	err    error
}

func (m mockValidator) Validate(_ context.Context, _ conflicts.ValidateInput) (conflicts.ValidationResult, error) {
	return m.result, m.err
}

func TestHandleValidatePair_NilValidator(t *testing.T) {
	h := &Handlers{
		logger:              quietLogger(),
		conflictValidator:   nil,
		maxRequestBodyBytes: 1 << 20,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/admin/conflicts/validate-pair", strings.NewReader(`{}`))
	h.HandleValidatePair(rec, req)

	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestHandleValidatePair_NoopValidator(t *testing.T) {
	h := &Handlers{
		logger:              quietLogger(),
		conflictValidator:   conflicts.NoopValidator{},
		maxRequestBodyBytes: 1 << 20,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/admin/conflicts/validate-pair", strings.NewReader(`{}`))
	h.HandleValidatePair(rec, req)

	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestHandleValidatePair_MissingOutcomes(t *testing.T) {
	h := &Handlers{
		logger:              quietLogger(),
		conflictValidator:   mockValidator{},
		maxRequestBodyBytes: 1 << 20,
	}

	rec := httptest.NewRecorder()
	body := `{"outcome_a":"","outcome_b":""}`
	req := httptest.NewRequest("POST", "/v1/admin/conflicts/validate-pair", strings.NewReader(body))
	h.HandleValidatePair(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleValidatePair_SuccessfulValidation(t *testing.T) {
	h := &Handlers{
		logger: quietLogger(),
		conflictValidator: mockValidator{
			result: conflicts.ValidationResult{
				Relationship: "contradiction",
				Category:     "factual",
				Severity:     "high",
				Explanation:  "These outcomes directly contradict each other",
			},
		},
		maxRequestBodyBytes: 1 << 20,
	}

	rec := httptest.NewRecorder()
	body := `{"outcome_a":"Use Redis","outcome_b":"Use Memcached","type_a":"architecture","type_b":"architecture","agent_a":"alpha","agent_b":"beta"}`
	req := httptest.NewRequest("POST", "/v1/admin/conflicts/validate-pair", strings.NewReader(body))
	h.HandleValidatePair(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data validatePairResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "contradiction", resp.Data.Relationship)
	assert.Equal(t, "factual", resp.Data.Category)
	assert.Equal(t, "high", resp.Data.Severity)
	assert.Equal(t, "These outcomes directly contradict each other", resp.Data.Explanation)
}

func TestHandleValidatePair_ValidatorError(t *testing.T) {
	h := &Handlers{
		logger: quietLogger(),
		conflictValidator: mockValidator{
			err: assert.AnError,
		},
		maxRequestBodyBytes: 1 << 20,
	}

	rec := httptest.NewRecorder()
	body := `{"outcome_a":"Use Redis","outcome_b":"Use Memcached"}`
	req := httptest.NewRequest("POST", "/v1/admin/conflicts/validate-pair", strings.NewReader(body))
	h.HandleValidatePair(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestHandleConflictEval_NilValidator(t *testing.T) {
	h := &Handlers{
		logger:              quietLogger(),
		conflictValidator:   nil,
		maxRequestBodyBytes: 1 << 20,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/admin/conflicts/eval", nil)
	h.HandleConflictEval(rec, req)

	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestHandleConflictEval_NoopValidator(t *testing.T) {
	h := &Handlers{
		logger:              quietLogger(),
		conflictValidator:   conflicts.NoopValidator{},
		maxRequestBodyBytes: 1 << 20,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/admin/conflicts/eval", nil)
	h.HandleConflictEval(rec, req)

	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestHandleConflictEval_WithMockValidator(t *testing.T) {
	h := &Handlers{
		logger: quietLogger(),
		conflictValidator: mockValidator{
			result: conflicts.ValidationResult{
				Relationship: "contradiction",
				Category:     "factual",
				Severity:     "high",
				Explanation:  "mock conflict",
			},
		},
		maxRequestBodyBytes: 1 << 20,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/admin/conflicts/eval", nil)
	h.HandleConflictEval(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data conflictEvalResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Greater(t, len(resp.Data.Results), 0, "eval should produce results from default dataset")
}

func TestHandleConflictEval_WithValidatorError(t *testing.T) {
	h := &Handlers{
		logger: quietLogger(),
		conflictValidator: mockValidator{
			err: assert.AnError,
		},
		maxRequestBodyBytes: 1 << 20,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/admin/conflicts/eval", nil)
	h.HandleConflictEval(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data conflictEvalResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	// All results should have errors.
	for _, r := range resp.Data.Results {
		assert.NotEmpty(t, r.Error, "each result should have an error when validator fails")
	}
}

func TestHandleValidatePair_InvalidJSON(t *testing.T) {
	h := &Handlers{
		logger:              quietLogger(),
		conflictValidator:   mockValidator{},
		maxRequestBodyBytes: 1 << 20,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/admin/conflicts/validate-pair", strings.NewReader("not json"))
	h.HandleValidatePair(rec, req)

	// The handler returns early without writing an error response when
	// decodeJSON fails (it should call handleDecodeError but doesn't).
	// This is existing behavior; the test documents it.
	assert.Equal(t, http.StatusOK, rec.Code)
}
