// Package namemodel is the backend's client of the name model container: it
// sends texts over the model's Unix socket and returns the spans the model
// labels a person (seam S2 of the masking-fixes arc: POST /v1/spans). It
// checks every answer against the texts it sent, and an answer it cannot
// trust is a failure, never spans.
package namemodel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
	"unicode/utf8"

	"personal-crm/backend/internal/threadctx"
)

// Deadline bounds one request: Start for the model to start and load, which
// a request after the model went idle waits for, PerText for each text it
// answers, and PerCharacter for each character of those texts.
type Deadline struct {
	Start, PerText, PerCharacter time.Duration
}

// DefaultDeadline allows twice what the Pi needs, measured 2026-10-23: a cold
// start of about 30 s, about 1.5 s for a short text, and about 16 ms per word
// once a text is long. A word holds at least one character, so 32 ms per
// character covers any text however it divides into words.
var DefaultDeadline = Deadline{Start: 90 * time.Second, PerText: 5 * time.Second, PerCharacter: 32 * time.Millisecond}

// of returns the time a request for the texts may take.
func (d Deadline) of(texts []string) time.Duration {
	total := d.Start
	for _, text := range texts {
		total += d.PerText + time.Duration(utf8.RuneCountInString(text))*d.PerCharacter
	}
	return total
}

// Client asks the name model for spans.
type Client struct {
	http     *http.Client
	deadline Deadline
}

// New returns a client of the model listening on the Unix socket. It refuses
// an empty path: with no model, no request could be masked.
func New(socket string, deadline Deadline) (*Client, error) {
	if socket == "" {
		return nil, errors.New("the name model's socket is unknown: set NAME_MODEL_SOCKET to its path")
	}
	var dialer net.Dialer
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		// The model exits once idle, so a kept connection would be dead.
		DisableKeepAlives: true,
	}
	return &Client{http: &http.Client{Transport: transport}, deadline: deadline}, nil
}

type request struct {
	Model     string   `json:"model"`
	Label     string   `json:"label"`
	Threshold float64  `json:"threshold"`
	Texts     []string `json:"texts"`
}

// response decodes the endpoint's answer. A text's result, a span and a
// span's offsets are pointers, so a null or missing one is told apart from an
// empty result or a zero offset and fails the whole response.
type response struct {
	Model     string             `json:"model"`
	Label     string             `json:"label"`
	Threshold float64            `json:"threshold"`
	Spans     []*[]*responseSpan `json:"spans"`
}

type responseSpan struct {
	Start *int `json:"start"`
	End   *int `json:"end"`
}

// Spans returns, for each text, the spans the model labels the version's
// label at its cutoff or more, as UTF-8 byte offsets into the text. It fails
// when the model does not answer within the deadline, answers with a
// non-success status, or answers for another model version, for other texts,
// or with a span that is not a range of whole characters of its text. Its
// errors carry no text.
func (c *Client) Spans(ctx context.Context, model threadctx.ModelVersion, texts []string) ([][]threadctx.NameSpan, error) {
	for _, text := range texts {
		if !utf8.ValidString(text) {
			return nil, errors.New("name model: a text is not valid UTF-8")
		}
	}
	body, err := json.Marshal(request{Model: model.Model(), Label: model.Label(), Threshold: model.Threshold(), Texts: texts})
	if err != nil {
		return nil, fmt.Errorf("name model: encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.deadline.of(texts))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://name-model/v1/spans", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("name model: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("name model: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("name model: status %d", resp.StatusCode)
	}
	var got response
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return nil, errors.New("name model: the response is not the endpoint's JSON")
	}
	if got.Model != model.Model() || got.Label != model.Label() || got.Threshold != model.Threshold() {
		return nil, errors.New("name model: the response is for another model version")
	}
	if len(got.Spans) != len(texts) {
		return nil, fmt.Errorf("name model: %d results for %d texts", len(got.Spans), len(texts))
	}
	out := make([][]threadctx.NameSpan, len(texts))
	for i, spans := range got.Spans {
		if spans == nil {
			return nil, fmt.Errorf("name model: text %d has no result", i)
		}
		text := texts[i]
		out[i] = make([]threadctx.NameSpan, len(*spans))
		for j, s := range *spans {
			if s == nil || s.Start == nil || s.End == nil {
				return nil, fmt.Errorf("name model: span %d of text %d lacks its offsets", j, i)
			}
			start, end := *s.Start, *s.End
			if start < 0 || start >= end || end > len(text) || !boundary(text, start) || !boundary(text, end) {
				return nil, fmt.Errorf("name model: span %d of text %d is not a range of whole characters of it", j, i)
			}
			out[i][j] = threadctx.NameSpan{Start: start, End: end}
		}
	}
	return out, nil
}

// boundary reports whether a byte offset of a valid UTF-8 text starts a
// character or ends the text.
func boundary(text string, at int) bool {
	return at == len(text) || utf8.RuneStart(text[at])
}
