package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"personal-crm/backend/internal/threadctx"
)

// callTimeout bounds one call; a local model on CPU reads a long context slowly.
const callTimeout = 10 * time.Minute

// openRouterRouting is OpenRouter's provider routing for one request.
type openRouterRouting struct {
	ZDR            bool     `json:"zdr"`
	Only           []string `json:"only,omitempty"`
	AllowFallbacks bool     `json:"allow_fallbacks"`
}

// systemOne speaks the System One wire shape, which TypeSafe's native API,
// OpenRouter's decisions endpoint and a Kev server share: a state, and
// questions keyed by ID with their criteria; answers keyed by the same IDs.
type systemOne struct {
	url, apiKey, model string
	routing            *openRouterRouting
	http               *http.Client
}

// NewSystemOneAdapter is an adapter for a System One endpoint at url, such as
// TypeSafe's native API or a Kev server.
func NewSystemOneAdapter(url, apiKey, model string) Adapter {
	return newSystemOne(url, apiKey, model, nil)
}

func newSystemOne(url, apiKey, model string, routing *openRouterRouting) *systemOne {
	return &systemOne{url: url, apiKey: apiKey, model: model, routing: routing, http: &http.Client{Timeout: callTimeout}}
}

// Route reports the endpoint, model and routing; the API key is no part of it.
func (s *systemOne) Route() Route {
	r := Route{Protocol: "systemone", Endpoint: s.url, Model: s.model}
	if s.routing != nil {
		r.Settings = map[string]string{
			"zdr":             strconv.FormatBool(s.routing.ZDR),
			"only":            strings.Join(s.routing.Only, ","),
			"allow_fallbacks": strconv.FormatBool(s.routing.AllowFallbacks),
		}
	}
	return r
}

func (s *systemOne) Send(ctx context.Context, req threadctx.MaskedRequest) (Answers, error) {
	return s.post(ctx, renderMasked(req))
}

type systemOneQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type systemOneAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type systemOneResponse struct {
	Answers map[string]systemOneAnswer `json:"answers"`
	Usage   *struct {
		InputTokens *int     `json:"input_tokens"`
		Cost        *float64 `json:"cost"`
	} `json:"usage"`
}

func (s *systemOne) post(ctx context.Context, w wireRequest) (Answers, error) {
	questions := make(map[string]systemOneQuestion, len(w.Questions))
	for _, q := range w.Questions {
		sq := systemOneQuestion{Type: string(q.Kind), Instructions: q.Instructions}
		if q.Kind == threadctx.QuestionNoul {
			sq.Criteria = map[string]string{"true": q.Options[0].Description, "false": q.Options[1].Description}
		} else {
			criteria := make(map[string]*string, len(q.Options))
			for _, o := range q.Options {
				criteria[o.Name] = nil
				if o.Description != "" {
					criteria[o.Name] = &o.Description
				}
			}
			sq.Criteria = criteria
		}
		questions[q.ID] = sq
	}
	body, err := json.Marshal(struct {
		Model     string                       `json:"model"`
		State     string                       `json:"state"`
		Questions map[string]systemOneQuestion `json:"questions"`
		Provider  *openRouterRouting           `json:"provider,omitempty"`
	}{s.model, w.State, questions, s.routing})
	if err != nil {
		return Answers{}, fmt.Errorf("encode request: %w", err)
	}
	var resp systemOneResponse
	if err := postJSON(ctx, s.http, s.url, s.apiKey, body, &resp); err != nil {
		return Answers{}, err
	}
	out := Answers{ByQuestion: make(map[string]Answer, len(w.Questions))}
	for _, q := range w.Questions {
		a, ok := resp.Answers[q.ID]
		if !ok {
			continue
		}
		if q.Kind == threadctx.QuestionNoul {
			if a.Noul != nil {
				out.ByQuestion[q.ID] = nounAnswer(q, *a.Noul, "")
			}
			continue
		}
		answer := Answer{Probabilities: make(map[string]float64, len(a.Probabilities))}
		for name, p := range a.Probabilities {
			if key, ok := q.keyOf(name); ok {
				answer.Probabilities[key] = p
			}
		}
		answer.Chosen, _ = q.keyOf(a.Choice)
		out.ByQuestion[q.ID] = answer
	}
	if resp.Usage != nil {
		out.BilledTokens, out.CostUSD = resp.Usage.InputTokens, resp.Usage.Cost
	}
	return out, nil
}

// nounAnswer is a yes/no answer as option probabilities: the first option is
// yes, the second no.
func nounAnswer(q wireQuestion, yes float64, chosen string) Answer {
	return Answer{Probabilities: map[string]float64{q.Options[0].Key: yes, q.Options[1].Key: 1 - yes}, Chosen: chosen}
}

// statusError is a call a vendor refused, by HTTP status. Its message carries
// no response body, which may echo request content.
type statusError struct{ status int }

func (e statusError) Error() string { return "decision call refused: HTTP " + strconv.Itoa(e.status) }

func postJSON(ctx context.Context, client *http.Client, url, apiKey string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("decision call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return statusError{resp.StatusCode}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
