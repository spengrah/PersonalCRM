package namemodel

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/threadctx"
)

// The production deadline allows at least twice what the Pi needs for every
// text a request carries, however the text divides into words or texts: a
// cold start of 30 s plus 1.5 s per window the server's model reads. Short
// texts keep at least 90 s plus 5 s per text.
// spec: KNW-090.deadline-covers-length
func TestDefaultDeadlineCoversLength(t *testing.T) {
	t.Parallel()
	const coldStart, perWindow = 30 * time.Second, 1500 * time.Millisecond
	long := func(unit string) string { return strings.Repeat(unit, 204000/len(unit)) }
	cases := []struct {
		name  string
		texts []string
		floor time.Duration
	}{
		{"one long prose text", []string{long("word ")}, 0},
		{"one long text with no spaces", []string{long("a.")}, 0},
		{"a long length split across texts", []string{long("word ")[:68000], long("word ")[:68000], long("word ")[:68000]}, 0},
		{"one short text", []string{"Testperson wrote."}, 90*time.Second + 5*time.Second},
		{"ten short texts", slices.Repeat([]string{"Testperson wrote."}, 10), 90*time.Second + 10*5*time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, err := New("unused.sock", DefaultDeadline)
			if err != nil {
				t.Fatal(err)
			}
			var deadline time.Time
			client.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
				var ok bool
				if deadline, ok = req.Context().Deadline(); !ok {
					t.Fatal("request has no deadline")
				}
				return emptyAnswer(len(tc.texts)), nil
			})
			before := accelerated.GetCurrentTime()
			if _, err := client.Spans(t.Context(), threadctx.NewModelVersion(0.5), tc.texts); err != nil {
				t.Fatal(err)
			}
			need := coldStart + time.Duration(serverWindows(t, tc.texts))*perWindow
			want := max(2*need, tc.floor)
			if got := deadline.Sub(before); got < want {
				t.Fatalf("deadline for %d texts: got %s, want at least %s (twice the need of %s, floor %s)", len(tc.texts), got, want, need, tc.floor)
			}
		})
	}
}

// serverWindows is the number of windows the server's model reads for the
// texts, counted by the server's own window function, so the window size and
// stride keep their one writer.
func serverWindows(t *testing.T, texts []string) int {
	t.Helper()
	input, err := json.Marshal(texts)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-B", "-c", `import json, sys
from name_model.gliner_model import GlinerModel
from name_model.server import windows
texts = json.load(sys.stdin)
print(sum(len(windows(t, GlinerModel.window_words, GlinerModel.overlap_words)) for t in texts))`)
	cmd.Dir = "../../../name-model"
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("count the server's windows: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("count the server's windows: %q is not a count", out)
	}
	return n
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// emptyAnswer is the endpoint's answer of no spans for each of n texts.
func emptyAnswer(n int) *http.Response {
	model := threadctx.NewModelVersion(0.5)
	spans := make([][]any, n)
	for i := range spans {
		spans[i] = []any{}
	}
	body, _ := json.Marshal(map[string]any{"model": model.Model(), "label": model.Label(), "threshold": model.Threshold(), "spans": spans})
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}
}
