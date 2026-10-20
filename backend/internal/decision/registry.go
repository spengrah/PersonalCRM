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
	// Clef pinned to DeepInfra under zero-retention routing:
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
)

// Limits, each bounding the whole request so that every vendor limit holds.
var (
	// Clef on DeepInfra: a 65,536-token context; 64 questions, as on Workers AI.
	clefLimits = Limits{Tokens: 65536, Questions: 64}
	// Jev on Venice: 32,000 tokens for the state with its longest question,
	// the tighter of its two limits (64,000 for the state with every
	// question); no question cap.
	jevLimits = Limits{Tokens: 32000, Questions: 32000}
)

// Sizing of each method's window. The window is sized by the size estimate
// (one token per byte of everything the model reads, an upper bound on any
// tokenizer's count) times Ratio times (1 + Margin). Ratio and Margin come
// from the ratio of the input tokens a vendor billed to that estimate over the
// January 2026 runs, and are checked against the tokens billed for synthetic
// threads at sampled sizes.
var (
	clefSizing = Sizing{Ratio: 0.43, Margin: 0.40}
	jevSizing  = Sizing{Ratio: 0.40, Margin: 0.60}
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

// NewRegistry returns the registry of the compared methods, each keyed by its
// definition's key: the one list of methods a run, the production designation
// and the scorecard accept. A removed method (sage-masked, clef-flash-masked)
// is not listed, so naming it is refused as an unknown method is; its stored
// judgments and runs read without the registry. A method whose route is not
// configured stays in the registry and fails on Get.
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
		// Venice relays Jev without the user's identity, but its route offers
		// no zero retention at TypeSafe, so Jev runs on masked content only.
		// Venice serves only jev-latest and reports no release, so a new Jev
		// release does not change this method's version.
		{Definition{Key: "jev-masked", Wording: DefaultWording, Limits: jevLimits, Sizing: jevSizing, Terms: veniceJevTerms},
			"VENICE_API_KEY", cfg.VeniceAPIKey, func(def Definition) (Method, error) {
				return NewMaskedMethod(def, masker, newSystemOne(veniceDecisionsURL, cfg.VeniceAPIKey, "jev-latest", nil)), nil
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
