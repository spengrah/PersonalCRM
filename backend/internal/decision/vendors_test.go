package decision_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// systemOneServer stands in for a System One vendor: it answers every noul
// question with yes at 0.2 and every choice with its first option at 0.7,
// reporting the given usage object.
func systemOneServer(t *testing.T, usage map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]struct {
				Type     string          `json:"type"`
				Criteria json.RawMessage `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			if q.Type == "noul" {
				answers[id] = map[string]any{"type": "noul", "noul": 0.2}
				continue
			}
			var criteria map[string]*string
			if err := json.Unmarshal(q.Criteria, &criteria); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			probs, first := map[string]float64{}, ""
			for name := range criteria {
				if first == "" || name < first {
					first = name
				}
			}
			for name := range criteria {
				probs[name] = 0.3 / float64(max(len(criteria)-1, 1))
			}
			probs[first] = 0.7
			answers[id] = map[string]any{"type": "choice", "choice": first, "probabilities": probs, "confidence": 0.5}
		}
		writeJSON(t, w, map[string]any{"model": "vendor-1", "answers": answers, "usage": usage})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sageServer stands in for Levanto's batch API: yes/no questions get the
// given answer ("yes", "no" or nil for not sure) at yes probability 0.35, and
// choices their first option at 0.6; usage is the given meta usage, or null.
// A non-zero status refuses the call.
func sageServer(t *testing.T, yesno *string, usage map[string]any, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			http.Error(w, `{"detail":"refused"}`, status)
			return
		}
		var req struct {
			Requests []struct {
				Questions []struct {
					Kind    string `json:"kind"`
					ID      string `json:"id"`
					Options []struct {
						Option string `json:"option"`
					} `json:"options"`
				} `json:"questions"`
			} `json:"requests"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var answers []any
		for _, q := range req.Requests[0].Questions {
			if q.Kind == "yesno" {
				answers = append(answers, map[string]any{"ok": true, "result": map[string]any{"id": q.ID, "kind": "yesno", "result": map[string]any{"answer": yesno, "probability": 0.35}, "meta": map[string]any{}}})
				continue
			}
			var probs []any
			for i, o := range q.Options {
				p := 0.4 / float64(len(q.Options)-1)
				if i == 0 {
					p = 0.6
				}
				probs = append(probs, map[string]any{"option": o.Option, "probability": p})
			}
			answers = append(answers, map[string]any{"ok": true, "result": map[string]any{"id": q.ID, "kind": "choice", "result": map[string]any{"chosen": q.Options[0].Option, "probability": 0.6, "probabilities": probs}, "meta": map[string]any{}}})
		}
		meta := map[string]any{"request_count": 1, "question_count": len(answers), "usage": nil}
		if usage != nil {
			meta["usage"] = usage
		}
		writeJSON(t, w, map[string]any{"results": []any{map[string]any{"answers": answers}}, "meta": meta})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Error(err)
	}
}

// An unmasked call follows no redirect: a local server, or a proxy in front
// of it, redirecting the request elsewhere fails the call, and nothing reaches
// the redirect's target.
func TestUnmasked_RedirectIsNotFollowed(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	t.Cleanup(target.Close)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/decide", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(local.Close)

	endpoint, err := decision.NewLocalEndpoint(local.URL)
	require.NoError(t, err)
	res, err := decision.NewUnmaskedMethod(definition(roomy), endpoint, "", "kev-latest").Judge(t.Context(), dm("I'll send the file on Friday"))
	require.NoError(t, err)
	assert.Equal(t, decision.Incomplete{Reason: judgment.IncompleteReasonCallFailed}, res)
	assert.False(t, reached, "the request did not follow the redirect")
}
