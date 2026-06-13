package todoist

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingRoundTripper records how many HTTP requests reach it and returns a
// canned response. It lets the guard tests detect whether a real outbound
// request was issued without hitting the global endpoint vars or the network,
// so they stay parallel-safe.
type recordingRoundTripper struct {
	calls atomic.Int32
	body  string
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls.Add(1)
	body := rt.body
	if body == "" {
		body = "{}"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// newRecordingClient builds a SyncClient for the given env whose transport
// records calls instead of hitting the network.
func newRecordingClient(env, respBody string) (*SyncClient, *recordingRoundTripper) {
	rt := &recordingRoundTripper{body: respBody}
	c := NewSyncClientForEnv("test-token", env)
	c.SetHTTPClient(&http.Client{Transport: rt})
	return c, rt
}

// nonProdEnvs are the CRM_ENV values that must refuse outbound writes.
var nonProdEnvs = []string{"staging", "test", "testing", "accelerated"}

// Test #1: QuickAdd under non-prod envs refuses and never issues HTTP.
func TestQuickAdd_NonProd_Refuses(t *testing.T) {
	t.Parallel()
	for _, env := range nonProdEnvs {
		env := env
		t.Run(env, func(t *testing.T) {
			t.Parallel()
			c, rt := newRecordingClient(env, "")
			task, err := c.QuickAdd(context.Background(), "do a thing", "")
			require.ErrorIs(t, err, ErrNonProdWriteRefused)
			assert.Nil(t, task)
			assert.Equal(t, int32(0), rt.calls.Load(), "no HTTP request should be issued")
		})
	}
}

// Test #2: Sync with commands under non-prod envs refuses and never issues HTTP.
func TestSync_WithCommands_NonProd_Refuses(t *testing.T) {
	t.Parallel()
	for _, env := range nonProdEnvs {
		env := env
		t.Run(env, func(t *testing.T) {
			t.Parallel()
			c, rt := newRecordingClient(env, "")
			cmds := []SyncCommand{NewItemCloseCommand("task-123")}
			resp, err := c.Sync(context.Background(), "*", []string{"items"}, cmds)
			require.ErrorIs(t, err, ErrNonProdWriteRefused)
			assert.Nil(t, resp)
			assert.Equal(t, int32(0), rt.calls.Load(), "no HTTP request should be issued")
		})
	}
}

// Test #3: Sync with empty commands (read-only) under non-prod is allowed and
// does issue HTTP — guards against over-blocking reads.
func TestSync_ReadOnly_NonProd_Allowed(t *testing.T) {
	t.Parallel()
	c, rt := newRecordingClient("staging", `{"sync_token":"abc","full_sync":true}`)
	resp, err := c.Sync(context.Background(), "*", []string{"items"}, nil)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "abc", resp.SyncToken)
	assert.Equal(t, int32(1), rt.calls.Load(), "read-only sync should issue one request")
}

// Test #4: QuickAdd under production issues the request and decodes the response.
func TestQuickAdd_Production_Writes(t *testing.T) {
	t.Parallel()
	c, rt := newRecordingClient("production", `{"id":"6abc","content":"do a thing"}`)
	task, err := c.QuickAdd(context.Background(), "do a thing", "")
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.Equal(t, "6abc", task.ID)
	assert.Equal(t, int32(1), rt.calls.Load())
}

// Test #5/#6/#7: Sync with commands under prod aliases (production, prod, "")
// issues the request and decodes the response.
func TestSync_WithCommands_ProdAliases_Write(t *testing.T) {
	t.Parallel()
	for _, env := range []string{"production", "prod", ""} {
		env := env
		t.Run("env="+env, func(t *testing.T) {
			t.Parallel()
			c, rt := newRecordingClient(env, `{"sync_token":"xyz"}`)
			cmds := []SyncCommand{NewItemCloseCommand("task-123")}
			resp, err := c.Sync(context.Background(), "*", []string{"items"}, cmds)
			require.NoError(t, err)
			require.NotNil(t, resp)
			assert.Equal(t, "xyz", resp.SyncToken)
			assert.Equal(t, int32(1), rt.calls.Load())
		})
	}
}

// Test #8: NewSyncClient produces a production-env client (back-compat): a
// Sync-with-commands call issues the request.
func TestNewSyncClient_BackCompat_Writes(t *testing.T) {
	t.Parallel()
	rt := &recordingRoundTripper{body: `{"sync_token":"ok"}`}
	c := NewSyncClient("test-token")
	c.SetHTTPClient(&http.Client{Transport: rt})
	cmds := []SyncCommand{NewItemCloseCommand("task-123")}
	resp, err := c.Sync(context.Background(), "*", []string{"items"}, cmds)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, int32(1), rt.calls.Load())
}

// Test #9: NewClientFactory wires the env through — staging refuses, production writes.
func TestNewClientFactory_EnvWiring(t *testing.T) {
	t.Parallel()

	t.Run("staging refuses", func(t *testing.T) {
		t.Parallel()
		client := NewClientFactory("staging")("test-token")
		task, err := client.QuickAdd(context.Background(), "x", "")
		require.ErrorIs(t, err, ErrNonProdWriteRefused)
		assert.Nil(t, task)
	})

	t.Run("production writes", func(t *testing.T) {
		t.Parallel()
		rt := &recordingRoundTripper{body: `{"id":"6abc"}`}
		client := NewClientFactory("production")("test-token")
		// The factory returns a Client; the concrete type is *SyncClient.
		sc, ok := client.(*SyncClient)
		require.True(t, ok, "factory should return *SyncClient")
		sc.SetHTTPClient(&http.Client{Transport: rt})
		task, err := sc.QuickAdd(context.Background(), "x", "")
		require.NoError(t, err)
		require.NotNil(t, task)
		assert.Equal(t, int32(1), rt.calls.Load())
	})
}

// Test #10: the refused error satisfies errors.Is and returns a nil result.
func TestRefusedError_Surface(t *testing.T) {
	t.Parallel()
	c, _ := newRecordingClient("staging", "")

	task, err := c.QuickAdd(context.Background(), "x", "")
	assert.True(t, errors.Is(err, ErrNonProdWriteRefused))
	assert.Nil(t, task)

	resp, err := c.Sync(context.Background(), "*", []string{"items"}, []SyncCommand{NewItemCloseCommand("t")})
	assert.True(t, errors.Is(err, ErrNonProdWriteRefused))
	assert.Nil(t, resp)
}
