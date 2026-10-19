package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"time"

	"personal-crm/backend/internal/judgment"
)

// Definition is what a method is apart from its route: its key, question
// wording, limits, sizing and terms status.
type Definition struct {
	Key     string
	Wording Wording
	Limits  Limits
	Sizing  Sizing
	Terms   TermsStatus
}

// Route is how a method reaches its model: the wire protocol, the endpoint,
// the model and the route's settings (such as zero-retention routing). An
// adapter reports its own route. Masking is no setting: the constructor a
// method is built with decides it, so no route setting can turn it off.
type Route struct {
	Protocol string
	Endpoint string
	Model    string
	Settings map[string]string
}

// Limits are a method's request limits. Tokens bounds the whole request
// (state and every question) so that every limit the vendor sets holds.
// Questions bounds the question count; a vendor that caps no question count
// sets it to Tokens, since every question costs at least one token.
type Limits struct {
	Tokens    int
	Questions int
}

// Sizing scales the size estimate of a request (one token per byte of
// everything the model reads, which no tokenizer exceeds) to the input tokens
// the vendor bills for it. A method sizes its window by the scaled estimate:
// Ratio is the billed tokens per estimated token, Margin the fraction added
// above it so that a request billed above its typical ratio still fits.
type Sizing struct {
	Ratio, Margin float64
}

// scale returns the tokens a request's window is sized by.
func (s Sizing) scale(w wireRequest) int {
	return int(math.Ceil(float64(countTokens(w)) * s.Ratio * (1 + s.Margin)))
}

// ref names the method built from the definition on a route. Its version is
// derived from everything that produces a judgment, the route and the
// question wording, so changing any part changes it. Limits
// and terms decide whether and where a method may run, not what it answers.
func (d Definition) ref(r Route) judgment.MethodRef {
	return judgment.MethodRef{
		Key: d.Key,
		Version: digest(struct {
			Route   Route
			Wording Wording
		}{r, d.Wording}),
		WordingVersion: d.Wording.Version(),
	}
}

// Version derives the question-wording version from the whole wording.
func (w Wording) Version() string { return digest(w) }

func digest(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		panic(err) // plain structs of strings always encode
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:6])
}

// TermsFinding is what a vendor's terms show about one promise.
type TermsFinding string

const (
	TermsShown        TermsFinding = "shown"
	TermsContradicted TermsFinding = "contradicted"
	TermsUnverified   TermsFinding = "unverified"
)

// TermsSource is a primary source a terms finding was read from, and when.
type TermsSource struct {
	URL    string
	ReadOn time.Time
}

// TermsStatus is what a method's vendors' terms, read at the primary source,
// show about training on and retaining request content (HC3). It informs the
// user's choice of production method and gates nothing.
type TermsStatus struct {
	Training, Retention TermsFinding
	Sources             []TermsSource
}
