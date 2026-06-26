package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseUUIDParam_Valid(t *testing.T) {
	want := uuid.New()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: want.String()}}

	got, ok := ParseUUIDParam(c, "id", "contact")

	require.True(t, ok)
	assert.Equal(t, want, got)
	// On success nothing is written to the response.
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Body.String())
}

func TestParseUUIDParam_Invalid(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}

	got, ok := ParseUUIDParam(c, "id", "contact")

	require.False(t, ok)
	assert.Equal(t, uuid.Nil, got)
	require.Equal(t, http.StatusBadRequest, w.Code)

	var resp APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ErrCodeValidation, resp.Error.Code)
	assert.Equal(t, "Invalid contact ID", resp.Error.Message)
	assert.Equal(t, "ID must be a valid UUID", resp.Error.Details)
	// The normalized 400 must not leak the raw parse error.
	assert.NotContains(t, w.Body.String(), "invalid UUID")
}
