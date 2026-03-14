package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHandleListConflicts_InvalidConflictKind(t *testing.T) {
	h := &Handlers{
		logger: quietLogger(),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/conflicts?conflict_kind=invalid", nil)
	h.HandleListConflicts(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid conflict_kind")
}

func TestHandleListConflicts_ValidConflictKind(t *testing.T) {
	h := &Handlers{
		logger: quietLogger(),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/conflicts?conflict_kind=cross_agent", nil)
	h.HandleListConflicts(rec, req)

	assert.NotEqual(t, http.StatusBadRequest, rec.Code)
}
