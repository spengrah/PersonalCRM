package decision

import (
	"errors"
	"slices"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/threadctx"
)

// Endpoints of the hosted routes.
const (
	openRouterDecisionsURL = "https://openrouter.ai/api/alpha/decisions"
	veniceDecisionsURL     = "https://api.venice.ai/api/v1/decisions"
	sageURL                = "https://sage.levanto.ai/decide/batch"
)

// readOn is when the terms below were read at their primary sources.
var readOn = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)

func sources(urls ...string) []TermsSource {
	out := make([]TermsSource, len(urls))
	for i, u := range urls {
		out[i] = TermsSource{URL: u, ReadOn: readOn}
	}
	return out
}

// Terms each route's vendors show, read at the URLs listed. OpenRouter's own
// terms promise no training and, under its zero-retention routing, no
// retention by OpenRouter or the serving provider.
var (
	openRouterSources = []string{
		"https://openrouter.ai/privacy",
		"https://openrouter.ai/terms",
		"https://openrouter.ai/docs/features/zdr",
		"https://openrouter.ai/api/v1/endpoints/zdr",
	}
	// Clef and Clef-Flash pinned to DeepInfra under zero-retention routing:
	// DeepInfra's terms promise no training on and no retention of customer
	// data beyond processing the request.
	deepInfraTerms = TermsStatus{Training: TermsShown, Retention: TermsShown, Sources: sources(slices.Concat(openRouterSources, []string{
		"https://deepinfra.com/terms", "https://deepinfra.com/privacy", "https://deepinfra.com/docs/data"})...)}
	// Venice relays Jev to TypeSafe without the user's Venice identity and
	// stores no prompt or response content; TypeSafe still sees the masked
	// content, promises no training, but keeps customer data "as long as
	// necessary" and offers zero retention to enterprise customers only.
	veniceJevTerms = TermsStatus{Training: TermsShown, Retention: TermsContradicted, Sources: append(
		[]TermsSource{{URL: "https://docs.venice.ai/overview/privacy", ReadOn: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}},
		sources("https://typesafe.ai/legal/privacy-policy", "https://typesafe.ai/legal/data-processing", "https://typesafe.ai/legal/mca", "https://docs.typesafe.ai/legal.md")...)}
	// Levanto promises no training and keeps no content after responding,
	// except diagnostic payloads of failed or slow requests, deleted after 30
	// days: temporary, and masked.
	sageTerms = TermsStatus{Training: TermsShown, Retention: TermsShown, Sources: sources(
		"https://levanto.ai/privacy", "https://levanto.ai/terms")}
)

// Limits, each bounding the whole request so that every vendor limit holds.
var (
	// Clef on DeepInfra: a 65,536-token context; 64 questions, as on Workers AI.
	clefLimits = Limits{Tokens: 65536, Questions: 64}
	// Clef-Flash on DeepInfra: a 24,576-token context; 64 questions.
	clefFlashLimits = Limits{Tokens: 24576, Questions: 64}
	// Jev on Venice: 32,000 tokens for the state with its longest question,
	// the tighter of its two limits (64,000 for the state with every
	// question); no question cap.
	jevLimits = Limits{Tokens: 32000, Questions: 32000}
	// Sage: 32,768 tokens on plans below Growth, which alone extends the
	// context to 131,072; no documented question cap.
	sageLimits = Limits{Tokens: 32768, Questions: 32768}
)

// Sizing of each method's window, chosen from the ratio of the input tokens
// its vendor billed to the size estimate (one token per byte of everything the
// model reads, an upper bound on any tokenizer's count) over the January 2026
// runs. The window is sized by the estimate times Ratio times (1 + Margin).
//
//   - Clef and Clef-Flash: ratio 0.33 to 0.59, median 0.43, stable across
//     context sizes. Ratio is the median and Margin takes the factor to 0.60,
//     the largest ratio billed.
//   - Jev: ratio 0.33 to 0.68, median 0.40; the highest ratios are contexts
//     under 3,000 tokens, far from any limit. Margin takes the factor to 0.64,
//     above the 99th percentile.
//   - Sage: ratio 0.30 to 2.80, median 1.05, and growing with the context and
//     with the number of questions, since Sage bills each question's content
//     separately. Its ratio is unstable, so no ratio sizes its window: it is
//     sized by the unscaled estimate, which bounds the content each question
//     carries. What Sage bills grows with the window and the questions, which
//     the sizing does not bound; that cost is the user's to decide.
var (
	clefSizing      = Sizing{Ratio: 0.43, Margin: 0.40}
	clefFlashSizing = Sizing{Ratio: 0.43, Margin: 0.40}
	jevSizing       = Sizing{Ratio: 0.40, Margin: 0.60}
	sageSizing      = Sizing{Ratio: 1, Margin: 0}
)

// zeroRetention routes an OpenRouter request only to zero-retention
// endpoints, pinned to providers, with no fallback.
func zeroRetention(providers ...string) *openRouterRouting {
	return &openRouterRouting{ZDR: true, Only: providers, AllowFallbacks: false}
}

// registered is one method in the registry: a definition, the setting
// that must be configured, and the build.
type registered struct {
	def     Definition
	setting string
	value   string
	build   func(Definition) (Method, error)
}

// NewRegistry returns the registry of every compared method, each keyed by
// its definition's key. A method whose route is not configured stays in the
// registry and fails on Get.
func NewRegistry(cfg config.DecisionConfig, masker *threadctx.Masker) *Registry {
	openRouter := func(model string, routing *openRouterRouting) func(Definition) (Method, error) {
		return func(def Definition) (Method, error) {
			return NewMaskedMethod(def, masker, newSystemOne(openRouterDecisionsURL, cfg.OpenRouterAPIKey, model, routing)), nil
		}
	}
	routes := []registered{
		{Definition{Key: "clef-masked", Wording: DefaultWording, Limits: clefLimits, Sizing: clefSizing, Terms: deepInfraTerms},
			"OPENROUTER_API_KEY", cfg.OpenRouterAPIKey, openRouter("cloudflare/clef", zeroRetention("deepinfra"))},
		{Definition{Key: "clef-masked-reasoned", Wording: ReasonedWording, Limits: clefLimits, Sizing: clefSizing, Terms: deepInfraTerms},
			"OPENROUTER_API_KEY", cfg.OpenRouterAPIKey, openRouter("cloudflare/clef", zeroRetention("deepinfra"))},
		{Definition{Key: "clef-flash-masked", Wording: DefaultWording, Limits: clefFlashLimits, Sizing: clefFlashSizing, Terms: deepInfraTerms},
			"OPENROUTER_API_KEY", cfg.OpenRouterAPIKey, openRouter("cloudflare/clef-flash", zeroRetention("deepinfra"))},
		// Venice relays Jev without the user's identity, but its route offers
		// no zero retention at TypeSafe, so Jev runs on masked content only.
		// Venice serves only jev-latest and reports no release, so a new Jev
		// release does not change this method's version.
		{Definition{Key: "jev-masked", Wording: DefaultWording, Limits: jevLimits, Sizing: jevSizing, Terms: veniceJevTerms},
			"VENICE_API_KEY", cfg.VeniceAPIKey, func(def Definition) (Method, error) {
				return NewMaskedMethod(def, masker, newSystemOne(veniceDecisionsURL, cfg.VeniceAPIKey, "jev-latest", nil)), nil
			}},
		// Levanto's route offers no zero retention either.
		{Definition{Key: "sage-masked", Wording: DefaultWording, Limits: sageLimits, Sizing: sageSizing, Terms: sageTerms},
			"SAGE_API_KEY", cfg.SageAPIKey, func(def Definition) (Method, error) {
				return NewMaskedMethod(def, masker, newSage(sageURL, cfg.SageAPIKey)), nil
			}},
	}
	r := &Registry{methods: make(map[string]func() (Method, error), len(routes))}
	for _, rt := range routes {
		if _, dup := r.methods[rt.def.Key]; dup {
			panic("decision: method key registered twice: " + rt.def.Key)
		}
		r.methods[rt.def.Key] = func() (Method, error) {
			if rt.value == "" {
				return nil, errors.New(rt.setting + " is not set")
			}
			return rt.build(rt.def)
		}
	}
	return r
}
