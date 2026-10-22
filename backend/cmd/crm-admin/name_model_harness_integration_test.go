//go:build integration_testdb

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/namemodel"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/require"
)

// spansRequest is a request the name model received.
type spansRequest struct {
	Model     string   `json:"model"`
	Label     string   `json:"label"`
	Threshold float64  `json:"threshold"`
	Texts     []string `json:"texts"`
}

// nameModel is a fake name model: an HTTP server on a Unix socket speaking
// the model container's endpoint (POST /v1/spans), recording every request
// and answering each by the policy a test sets. By default it finds no name.
type nameModel struct {
	socket string
	mu     sync.Mutex
	asked  []spansRequest
	srv    *httptest.Server
	// answer writes the response to a request; nil answers every text with
	// no span.
	answer func(w http.ResponseWriter, r *http.Request, req spansRequest)
}

func newNameModel(t *testing.T) *nameModel {
	t.Helper()
	// A Unix socket path is short; t.TempDir's can exceed the limit.
	dir, err := os.MkdirTemp("", "nm")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	m := &nameModel{socket: filepath.Join(dir, "s.sock")}
	m.start(t)
	t.Cleanup(m.stop)
	return m
}

// start listens on the socket.
func (m *nameModel) start(t *testing.T) {
	t.Helper()
	l, err := net.Listen("unix", m.socket)
	require.NoError(t, err)
	m.srv = httptest.NewUnstartedServer(http.HandlerFunc(m.serve))
	m.srv.Listener = l
	m.srv.Start()
}

// stop closes the socket, so no listener answers it.
func (m *nameModel) stop() {
	if m.srv != nil {
		m.srv.CloseClientConnections()
		m.srv.Close()
		m.srv = nil
	}
}

func (m *nameModel) serve(w http.ResponseWriter, r *http.Request) {
	var req spansRequest
	if r.Method != http.MethodPost || r.URL.Path != "/v1/spans" || json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusUnprocessableEntity)
		return
	}
	m.mu.Lock()
	m.asked = append(m.asked, req)
	answer := m.answer
	m.mu.Unlock()
	if answer != nil {
		answer(w, r, req)
		return
	}
	writeSpans(w, req, make([][]threadctx.NameSpan, len(req.Texts)))
}

// writeSpans answers the request with the spans, echoing its model version.
func writeSpans(w http.ResponseWriter, req spansRequest, spans [][]threadctx.NameSpan) {
	out := make([][]map[string]int, len(spans))
	for i, s := range spans {
		out[i] = []map[string]int{}
		for _, n := range s {
			out[i] = append(out[i], map[string]int{"start": n.Start, "end": n.End})
		}
	}
	writeTestJSON(w, map[string]any{"model": req.Model, "label": req.Label, "threshold": req.Threshold, "spans": out})
}

// setAnswer sets the policy the model answers by.
func (m *nameModel) setAnswer(answer func(w http.ResponseWriter, r *http.Request, req spansRequest)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answer = answer
}

// requests returns every request the model received.
func (m *nameModel) requests() []spansRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.asked)
}

// texts returns every text the model was asked, in order.
func (m *nameModel) texts() []string {
	var out []string
	for _, r := range m.requests() {
		out = append(out, r.Texts...)
	}
	return out
}

func (m *nameModel) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = nil
}

// testDeadline bounds a request to the fake name model.
var testDeadline = namemodel.Deadline{Start: 10 * time.Second, PerText: time.Second}

// maskerOf returns a masker over the user's own names that covers requests
// with the env's span store and the name model on the socket, each request
// bounded by the deadline.
func (e *flagsEnv) maskerOf(t *testing.T, socket string, deadline namemodel.Deadline) *threadctx.Masker {
	t.Helper()
	client, err := namemodel.New(socket, deadline)
	require.NoError(t, err)
	masker, err := threadctx.NewMasker(threadctx.Dictionary{Own: ownNames}, threadctx.NewNameSpans(repository.NewNameSpanRepository(e.database.Queries), client))
	require.NoError(t, err)
	return masker
}
