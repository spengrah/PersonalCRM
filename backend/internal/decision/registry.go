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
	typeSafeURL            = "https://api.typesafe.ai/v1/systemone"
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
	// TypeSafe promises no training, but keeps customer data "as long as
	// necessary" and offers zero retention to enterprise customers only.
	typeSafeTerms = TermsStatus{Training: TermsShown, Retention: TermsContradicted, Sources: sources(
		"https://typesafe.ai/legal/privacy-policy", "https://typesafe.ai/legal/data-processing", "https://typesafe.ai/legal/mca", "https://docs.typesafe.ai/legal.md")}
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
	// Jev: 32,768 tokens for the state with any one question, the tighter of
	// its two limits; no question cap.
	jevLimits = Limits{Tokens: 32768, Questions: 32768}
	// Sage: 32,768 tokens on plans below Growth, which alone extends the
	// context to 131,072; no documented question cap.
	sageLimits = Limits{Tokens: 32768, Questions: 32768}
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
		{Definition{Key: "clef-masked", Wording: DefaultWording, Limits: clefLimits, Terms: deepInfraTerms},
			"OPENROUTER_API_KEY", cfg.OpenRouterAPIKey, openRouter("cloudflare/clef", zeroRetention("deepinfra"))},
		{Definition{Key: "clef-masked-reasoned", Wording: ReasonedWording, Limits: clefLimits, Terms: deepInfraTerms},
			"OPENROUTER_API_KEY", cfg.OpenRouterAPIKey, openRouter("cloudflare/clef", zeroRetention("deepinfra"))},
		{Definition{Key: "clef-flash-masked", Wording: DefaultWording, Limits: clefFlashLimits, Terms: deepInfraTerms},
			"OPENROUTER_API_KEY", cfg.OpenRouterAPIKey, openRouter("cloudflare/clef-flash", zeroRetention("deepinfra"))},
		// TypeSafe's native route offers a self-serve account no zero
		// retention, so Jev runs without it, on masked content only.
		{Definition{Key: "jev-masked", Wording: DefaultWording, Limits: jevLimits, Terms: typeSafeTerms},
			"TYPESAFE_API_KEY", cfg.TypeSafeAPIKey, func(def Definition) (Method, error) {
				return NewMaskedMethod(def, masker, newSystemOne(typeSafeURL, cfg.TypeSafeAPIKey, "jev-1.14.0", nil)), nil
			}},
		// Levanto's route offers no zero retention either.
		{Definition{Key: "sage-masked", Wording: DefaultWording, Limits: sageLimits, Terms: sageTerms},
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
