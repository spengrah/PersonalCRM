package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"personal-crm/backend/internal/threadctx"
)

// sage speaks Levanto's native batch API: one group holding the state and
// every question. Settings are sent explicitly, so a change of the
// deployment's defaults cannot change a method silently.
type sage struct {
	url, apiKey string
	http        *http.Client
}

const (
	sageLatencyMode = "quality"
	sageReasoning   = "off"
)

// NewSageAdapter is an adapter for Levanto's batch API at url.
func NewSageAdapter(url, apiKey string) Adapter { return newSage(url, apiKey) }

func newSage(url, apiKey string) *sage {
	return &sage{url: url, apiKey: apiKey, http: &http.Client{Timeout: callTimeout}}
}

func (s *sage) Route() Route {
	return Route{Protocol: "sage-batch", Endpoint: s.url, Settings: map[string]string{"latency_mode": sageLatencyMode, "reasoning": sageReasoning}}
}

func (s *sage) Send(ctx context.Context, req threadctx.MaskedRequest) (Answers, error) {
	return s.post(ctx, renderMasked(req))
}

type sageOption struct {
	Option      string  `json:"option"`
	Description *string `json:"description,omitempty"`
}

type sageQuestion struct {
	Kind         string       `json:"kind"`
	ID           string       `json:"id"`
	Instructions string       `json:"instructions"`
	Options      []sageOption `json:"options,omitempty"`
}

type sageResponse struct {
	Results []struct {
		Answers []struct {
			OK     bool `json:"ok"`
			Result *struct {
				ID     string `json:"id"`
				Kind   string `json:"kind"`
				Result struct {
					// yesno: answer is yes, no or null (not sure); probability is yes.
					Answer      *string  `json:"answer"`
					Probability *float64 `json:"probability"`
					// choice
					Chosen        *string `json:"chosen"`
					Probabilities []struct {
						Option      string  `json:"option"`
						Probability float64 `json:"probability"`
					} `json:"probabilities"`
				} `json:"result"`
			} `json:"result"`
		} `json:"answers"`
	} `json:"results"`
	Meta struct {
		Usage *struct {
			BilledInputTokens *int `json:"billed_input_tokens"`
		} `json:"usage"`
	} `json:"meta"`
}

func (s *sage) post(ctx context.Context, w wireRequest) (Answers, error) {
	questions := make([]sageQuestion, len(w.Questions))
	for i, q := range w.Questions {
		if q.Kind == threadctx.QuestionNoul {
			// A yes/no question takes no criteria, so its meanings join the
			// instructions.
			questions[i] = sageQuestion{Kind: "yesno", ID: q.ID, Instructions: strings.Join([]string{
				q.Instructions, "Yes means: " + q.Options[0].Description, "No means: " + q.Options[1].Description,
			}, "\n")}
			continue
		}
		options := make([]sageOption, len(q.Options))
		for j, o := range q.Options {
			options[j] = sageOption{Option: o.Name}
			if o.Description != "" {
				options[j].Description = &o.Description
			}
		}
		questions[i] = sageQuestion{Kind: "choice", ID: q.ID, Instructions: q.Instructions, Options: options}
	}
	type content struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	type group struct {
		Content   content        `json:"content"`
		Questions []sageQuestion `json:"questions"`
	}
	body, err := json.Marshal(struct {
		Requests    []group `json:"requests"`
		LatencyMode string  `json:"latency_mode"`
		Reasoning   string  `json:"reasoning"`
	}{[]group{{content{"text", w.State}, questions}}, sageLatencyMode, sageReasoning})
	if err != nil {
		return Answers{}, fmt.Errorf("encode request: %w", err)
	}
	var resp sageResponse
	if err := postJSON(ctx, s.http, s.url, s.apiKey, body, &resp); err != nil {
		return Answers{}, err
	}
	byID := make(map[string]wireQuestion, len(w.Questions))
	for _, q := range w.Questions {
		byID[q.ID] = q
	}
	out := Answers{ByQuestion: make(map[string]Answer, len(w.Questions))}
	for _, group := range resp.Results {
		for _, a := range group.Answers {
			if !a.OK || a.Result == nil {
				continue
			}
			q, ok := byID[a.Result.ID]
			if !ok {
				continue
			}
			r := a.Result.Result
			if q.Kind == threadctx.QuestionNoul {
				if r.Probability == nil {
					continue
				}
				chosen := ""
				switch {
				case r.Answer == nil: // not sure: no original answer
				case *r.Answer == "yes":
					chosen = q.Options[0].Key
				case *r.Answer == "no":
					chosen = q.Options[1].Key
				}
				out.ByQuestion[q.ID] = nounAnswer(q, *r.Probability, chosen)
				continue
			}
			answer := Answer{Probabilities: make(map[string]float64, len(r.Probabilities))}
			for _, p := range r.Probabilities {
				if key, ok := q.keyOf(p.Option); ok {
					answer.Probabilities[key] = p.Probability
				}
			}
			if r.Chosen != nil {
				answer.Chosen, _ = q.keyOf(*r.Chosen)
			}
			out.ByQuestion[q.ID] = answer
		}
	}
	if u := resp.Meta.Usage; u != nil {
		out.BilledTokens = u.BilledInputTokens
	}
	return out, nil
}
