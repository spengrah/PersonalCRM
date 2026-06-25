package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestContext returns a gin context backed by a fresh recorder.
func newTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return c, w
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) APIResponse {
	t.Helper()
	var resp APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func TestRespondError_BareNotFound(t *testing.T) {
	c, w := newTestContext()

	RespondError(c, db.ErrNotFound, "Contact")

	require.Equal(t, http.StatusNotFound, w.Code)
	resp := decodeError(t, w)
	require.NotNil(t, resp.Error)
	assert.Equal(t, ErrCodeNotFound, resp.Error.Code)
	assert.Equal(t, "Contact not found", resp.Error.Message)
	// A not-found is expected, not a server fault: it must NOT populate c.Errors.
	assert.Empty(t, c.Errors)
}

func TestRespondError_WrappedNotFound(t *testing.T) {
	c, w := newTestContext()

	// Proves errors.Is matching, not == comparison.
	wrapped := fmt.Errorf("get contact: %w", db.ErrNotFound)
	RespondError(c, wrapped, "Contact")

	require.Equal(t, http.StatusNotFound, w.Code)
	resp := decodeError(t, w)
	require.NotNil(t, resp.Error)
	assert.Equal(t, ErrCodeNotFound, resp.Error.Code)
	assert.Empty(t, c.Errors)
}

func TestRespondError_GenericDelegatesToInternal(t *testing.T) {
	c, w := newTestContext()

	RespondError(c, errors.New("db exploded"), "Contact")

	require.Equal(t, http.StatusInternalServerError, w.Code)
	resp := decodeError(t, w)
	require.NotNil(t, resp.Error)
	assert.Equal(t, ErrCodeInternal, resp.Error.Code)
	assert.Equal(t, "Internal server error", resp.Error.Message)
	// No raw cause leaks into the body.
	assert.Empty(t, resp.Error.Details)
	assert.NotContains(t, w.Body.String(), "db exploded")
	// The cause is captured for the access log.
	require.Len(t, c.Errors, 1)
}

func TestRespondError_NilErr(t *testing.T) {
	c, w := newTestContext()

	require.NotPanics(t, func() { RespondError(c, nil, "Contact") })

	require.Equal(t, http.StatusInternalServerError, w.Code)
	// nil err must not populate c.Errors (gin panics on c.Error(nil)).
	assert.Empty(t, c.Errors)
}

func TestRespondInternal_GenericError(t *testing.T) {
	c, w := newTestContext()

	RespondInternal(c, errors.New("connection refused"))

	require.Equal(t, http.StatusInternalServerError, w.Code)
	resp := decodeError(t, w)
	require.NotNil(t, resp.Error)
	assert.Equal(t, ErrCodeInternal, resp.Error.Code)
	assert.Equal(t, "Internal server error", resp.Error.Message)
	assert.Empty(t, resp.Error.Details)
	assert.NotContains(t, w.Body.String(), "connection refused")
	require.Len(t, c.Errors, 1)
	assert.Contains(t, c.Errors[0].Error(), "connection refused")
}

func TestRespondInternal_NilErr(t *testing.T) {
	c, w := newTestContext()

	require.NotPanics(t, func() { RespondInternal(c, nil) })

	require.Equal(t, http.StatusInternalServerError, w.Code)
	resp := decodeError(t, w)
	require.NotNil(t, resp.Error)
	assert.Equal(t, ErrCodeInternal, resp.Error.Code)
	assert.Empty(t, resp.Error.Details)
	// nil err: no c.Error, no log line (guarded).
	assert.Empty(t, c.Errors)
}

// TestRespondInternal_LogsCause is SERIAL (no t.Parallel) because it mutates the
// process-global logger via logger.SetOutput. It proves the acceptance criterion that a
// 500 logs its cause with request_id at error level.
func TestRespondInternal_LogsCause(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.SetOutput(&buf)
	defer restore()

	c, _ := newTestContext()
	c.Set("request_id", "req-abc-123")

	RespondInternal(c, errors.New("upstream timeout"))

	logLine := buf.String()
	assert.Contains(t, logLine, `"level":"error"`)
	assert.Contains(t, logLine, "req-abc-123")
	assert.Contains(t, logLine, "upstream timeout")
}

// TestRespondInternal_ConstructedSentinel covers the no-underlying-err sites (e.g.
// mac_host's "missing mac_host context" invariant) that pass a constructed error so they
// still get c.Error + a logged cause.
func TestRespondInternal_ConstructedSentinel(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.SetOutput(&buf)
	defer restore()

	c, w := newTestContext()

	RespondInternal(c, errors.New("missing mac_host context"))

	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Len(t, c.Errors, 1)
	assert.Contains(t, buf.String(), "missing mac_host context")
}

func TestLogServerError_NilIsNoop(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.SetOutput(&buf)
	defer restore()

	c, _ := newTestContext()

	require.NotPanics(t, func() { LogServerError(c, nil) })
	assert.Empty(t, c.Errors)
	assert.Empty(t, buf.String())
}

func TestLogServerError_UnknownRequestID(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.SetOutput(&buf)
	defer restore()

	c, _ := newTestContext() // no request_id set

	LogServerError(c, errors.New("boom"))

	require.Len(t, c.Errors, 1)
	assert.Contains(t, buf.String(), "unknown")
	assert.Contains(t, buf.String(), "boom")
}
