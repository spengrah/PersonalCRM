package langfuseexport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// batchSize bounds the records in one request.
const batchSize = 100

// client sends to one Langfuse project. Langfuse in events-only mode takes
// traces only over OTLP and scores only as ingestion score events.
type client struct {
	host                 string
	publicKey, secretKey string
	http                 *http.Client
}

// sendSpans sends each span as a trace of its own over OTLP/JSON.
func (c *client) sendSpans(ctx context.Context, spans []span) error {
	for start := 0; start < len(spans); start += batchSize {
		batch := spans[start:min(start+batchSize, len(spans))]
		encoded := make([]otlpSpan, len(batch))
		for i, s := range batch {
			e, err := encodeSpan(s)
			if err != nil {
				return err
			}
			encoded[i] = e
		}
		body := otlpRequest{ResourceSpans: []otlpResourceSpans{{
			Resource:   otlpResource{Attributes: []otlpAttribute{stringAttr("service.name", "crm-langfuse-export")}},
			ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: "langfuseexport"}, Spans: encoded}},
		}}}
		if _, err := c.post(ctx, "/api/public/otel/v1/traces", body); err != nil {
			return err
		}
	}
	return nil
}

// sendScores sends each score as an ingestion score event.
func (c *client) sendScores(ctx context.Context, scores []score) error {
	for start := 0; start < len(scores); start += batchSize {
		batch := scores[start:min(start+batchSize, len(scores))]
		events := make([]ingestionEvent, len(batch))
		for i, s := range batch {
			events[i] = ingestionEvent{
				ID: s.id, Type: "score-create", Timestamp: s.at.UTC().Format(time.RFC3339Nano),
				Body: scoreBody{ID: s.id, TraceID: s.traceID, Name: scoreVerdict, Value: s.value, DataType: "CATEGORICAL", Metadata: s.metadata},
			}
		}
		raw, err := c.post(ctx, "/api/public/ingestion", ingestionRequest{Batch: events})
		if err != nil {
			return err
		}
		var result struct {
			Errors []struct {
				ID      string `json:"id"`
				Status  int    `json:"status"`
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("decode ingestion response: %w", err)
		}
		if n := len(result.Errors); n > 0 {
			e := result.Errors[0]
			return fmt.Errorf("langfuse refused %d of %d scores; first %s: status %d: %s", n, len(batch), e.ID, e.Status, e.Message)
		}
	}
	return nil
}

// traceExists reports whether the project holds any observation of a trace.
func (c *client) traceExists(ctx context.Context, traceID string) (bool, error) {
	q := url.Values{"traceId": {traceID}, "fields": {"core"}, "limit": {"1"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/api/public/v2/observations?"+q.Encode()), nil)
	if err != nil {
		return false, fmt.Errorf("build observations request: %w", err)
	}
	raw, err := c.do(req)
	if err != nil {
		return false, err
	}
	var result struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return false, fmt.Errorf("decode observations response: %w", err)
	}
	return len(result.Data) > 0, nil
}

func (c *client) post(ctx context.Context, path string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-langfuse-ingestion-version", "4")
	return c.do(req)
}

func (c *client) do(req *http.Request) ([]byte, error) {
	req.SetBasicAuth(c.publicKey, c.secretKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", req.URL.Path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: status %d: %.200s", req.Method, req.URL.Path, resp.StatusCode, raw)
	}
	return raw, nil
}

func (c *client) url(path string) string {
	return strings.TrimRight(c.host, "/") + path
}

// encodeSpan maps a span onto the attributes Langfuse reads from OTLP.
func encodeSpan(s span) (otlpSpan, error) {
	attrs := []otlpAttribute{stringAttr("langfuse.trace.name", s.name)}
	jsonAttr := func(key string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("encode %s: %w", key, err)
		}
		attrs = append(attrs, stringAttr(key, string(raw)))
		return nil
	}
	if s.generation {
		attrs = append(attrs, stringAttr("langfuse.observation.type", "generation"), stringAttr("langfuse.observation.model.name", s.model))
	}
	if s.usage != nil {
		if err := jsonAttr("langfuse.observation.usage_details", s.usage); err != nil {
			return otlpSpan{}, err
		}
	}
	if s.cost != nil {
		if err := jsonAttr("langfuse.observation.cost_details", s.cost); err != nil {
			return otlpSpan{}, err
		}
	}
	if err := jsonAttr("langfuse.trace.metadata", s.metadata); err != nil {
		return otlpSpan{}, err
	}
	if s.output != nil {
		if err := jsonAttr("langfuse.observation.output", s.output); err != nil {
			return otlpSpan{}, err
		}
	}
	return otlpSpan{
		TraceID: s.traceID, SpanID: s.spanID, Name: s.name, Kind: 1,
		StartTimeUnixNano: strconv.FormatInt(s.start.UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(s.end.UnixNano(), 10),
		Attributes:        attrs,
	}, nil
}

func stringAttr(key, value string) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpValue{StringValue: value}}
}

// OTLP/JSON trace request, the subset the export sends.
type otlpRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	Name              string          `json:"name"`
	Kind              int             `json:"kind"`
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	EndTimeUnixNano   string          `json:"endTimeUnixNano"`
	Attributes        []otlpAttribute `json:"attributes"`
}

type otlpAttribute struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue string `json:"stringValue"`
}

// Langfuse ingestion request, carrying score events only.
type ingestionRequest struct {
	Batch []ingestionEvent `json:"batch"`
}

type ingestionEvent struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Timestamp string    `json:"timestamp"`
	Body      scoreBody `json:"body"`
}

type scoreBody struct {
	ID       string         `json:"id"`
	TraceID  string         `json:"traceId"`
	Name     string         `json:"name"`
	Value    string         `json:"value"`
	DataType string         `json:"dataType"`
	Metadata map[string]any `json:"metadata"`
}
