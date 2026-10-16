package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"personal-crm/backend/internal/judgment"
)

// Definition is what a method is apart from its route: its key, question
// wording, limits and terms status.
type Definition struct {
	Key     string
	Wording Wording
	Limits  Limits
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

// ref names the method built from the definition on a route. Its version is
// derived from everything that produces a judgment, the route, the masking
// variant and the question wording, so changing any part changes it. Limits
// and terms decide whether and where a method may run, not what it answers.
func (d Definition) ref(r Route, masked bool) judgment.MethodRef {
	return judgment.MethodRef{
		Key: d.Key,
		Version: digest(struct {
			Route   Route
			Masked  bool
			Wording Wording
		}{r, masked, d.Wording}),
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

// TermsStatus is a method's terms status for production (HC3): whether its
// vendors' terms show no training on request content, and no retention
// beyond masked content held temporarily. A local method is not hosted and
// has no vendor findings.
type TermsStatus struct {
	Hosted              bool
	Training, Retention TermsFinding
	Sources             []TermsSource
}

// Passes reports whether the method may serve production under HC3: it is
// not hosted, or its terms show both promises.
func (t TermsStatus) Passes() bool {
	return !t.Hosted || (t.Training == TermsShown && t.Retention == TermsShown)
}
