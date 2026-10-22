package threadctx

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"personal-crm/backend/internal/repository"
)

// NameSpan is a range of a text the name model labels a person: UTF-8 byte
// offsets into the text, half-open.
type NameSpan struct{ Start, End int }

// The name model the masker asks, and the label it asks for.
const (
	nameModelID    = "urchade/gliner_multi_pii-v1"
	nameModelLabel = "person"
)

// ModelVersion identifies the name model's spans: the model and label, which
// the masker owns, and the score cutoff (NAME_MODEL_THRESHOLD), which the
// configuration parses and validates. Equal cutoffs written differently are
// one version.
type ModelVersion struct{ threshold float64 }

// NewModelVersion is the name model at the score cutoff.
func NewModelVersion(threshold float64) ModelVersion { return ModelVersion{threshold: threshold} }

// Model returns the name model's identity.
func (v ModelVersion) Model() string { return nameModelID }

// Label returns the label the model is asked for.
func (v ModelVersion) Label() string { return nameModelLabel }

// Threshold returns the score cutoff.
func (v ModelVersion) Threshold() float64 { return v.threshold }

// String identifies the version: model, label and cutoff.
func (v ModelVersion) String() string {
	return nameModelID + " " + nameModelLabel + " " + strconv.FormatFloat(v.threshold, 'g', -1, 64)
}

// NameModel finds names: for each text, the spans it labels the version's
// label at the version's cutoff or more. It answers every text or fails.
type NameModel interface {
	Spans(ctx context.Context, model ModelVersion, texts []string) ([][]NameSpan, error)
}

// ErrNoNameSpans is a request with a text the name model has not covered: it
// is not masked, so it is not sent.
var ErrNoNameSpans = errors.New("a text has no name spans")

// NameSpans is the one path to name spans: the span store, and the name model
// asked for the texts the store lacks.
type NameSpans struct {
	store *repository.NameSpanRepository
	model NameModel
}

// NewNameSpans returns the name spans of the store, filled by the model.
func NewNameSpans(store *repository.NameSpanRepository, model NameModel) *NameSpans {
	return &NameSpans{store: store, model: model}
}

// spansPerRequest bounds the texts of one request to the name model, so a
// request's deadline stays bounded and a failed request loses few answers.
const spansPerRequest = 16

// textKey identifies a text the store keys spans by: its message and a digest
// of the text, never the text.
type textKey struct {
	message string
	digest  [sha256.Size]byte
}

func keyOf(message, text string) textKey {
	return textKey{message: message, digest: sha256.Sum256([]byte(text))}
}

// ensure returns the spans of every text of the entries under the version
// that the store holds or the model gives, asking the model for those the
// store lacks and storing what it answers. A failure of the model ends the
// asking and is returned as missed; any other error is returned as err.
func (n *NameSpans) ensure(ctx context.Context, model ModelVersion, entries []Entry) (found map[textKey][]NameSpan, missed, err error) {
	texts := map[textKey]string{}
	var order []textKey
	for _, e := range entries {
		for _, text := range e.modelTexts() {
			k := keyOf(e.Key, text)
			if _, seen := texts[k]; !seen {
				texts[k] = text
				order = append(order, k)
			}
		}
	}
	keys := make([]repository.NameSpanKey, len(order))
	for i, k := range order {
		keys[i] = repository.NameSpanKey{MessageKey: k.message, Digest: k.digest[:]}
	}
	stored, err := n.store.ListNameSpans(ctx, model.String(), keys)
	if err != nil {
		return nil, nil, fmt.Errorf("list name spans: %w", err)
	}
	found = map[textKey][]NameSpan{}
	for _, s := range stored {
		k := textKey{message: s.MessageKey}
		copy(k.digest[:], s.Digest)
		spans := make([]NameSpan, len(s.Starts))
		for i := range s.Starts {
			spans[i] = NameSpan{Start: int(s.Starts[i]), End: int(s.Ends[i])}
		}
		found[k] = spans
	}
	var lacking []textKey
	for _, k := range order {
		if _, ok := found[k]; !ok {
			lacking = append(lacking, k)
		}
	}
	for batch := range slices.Chunk(lacking, spansPerRequest) {
		asked := make([]string, len(batch))
		for i, k := range batch {
			asked[i] = texts[k]
		}
		answered, err := n.model.Spans(ctx, model, asked)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		if err != nil {
			return found, err, nil
		}
		save := make([]repository.StoredNameSpans, len(batch))
		for i, k := range batch {
			found[k] = answered[i]
			save[i] = repository.StoredNameSpans{
				NameSpanKey: repository.NameSpanKey{MessageKey: k.message, Digest: k.digest[:]},
				Starts:      make([]int32, len(answered[i])),
				Ends:        make([]int32, len(answered[i])),
			}
			for j, s := range answered[i] {
				save[i].Starts[j], save[i].Ends[j] = int32(s.Start), int32(s.End)
			}
		}
		if err := n.store.SaveNameSpans(ctx, model.String(), save); err != nil {
			return nil, nil, fmt.Errorf("save name spans: %w", err)
		}
	}
	return found, nil, nil
}

// modelTexts are the texts of the entry the name model covers: its text and
// every header that is not an address header, whose words are all masked.
func (e Entry) modelTexts() []string {
	texts := []string{e.Text}
	for _, name := range slices.Sorted(maps.Keys(e.Headers)) {
		if !addressHeaders[strings.ToLower(name)] {
			texts = append(texts, e.Headers[name])
		}
	}
	return texts
}

// covered returns c with each entry's texts covered by the spans spansOf
// finds for them, by message key and text; a text it finds none for is left
// uncovered, so Mask refuses its entry.
func covered(c Context, spansOf func(message, text string) ([]NameSpan, bool)) Context {
	c.Entries = slices.Clone(c.Entries)
	for i, e := range c.Entries {
		names := map[string][]NameSpan{}
		for _, text := range e.modelTexts() {
			if spans, ok := spansOf(e.Key, text); ok {
				names[text] = spans
			}
		}
		c.Entries[i].names = names
	}
	return c
}

// namesOf returns the name spans the entry was covered with for one of its
// texts, and ErrNoNameSpans when it has none: an entry the masker did not
// cover, or a text changed after it was covered.
func (e Entry) namesOf(text string) ([]NameSpan, error) {
	spans, ok := e.names[text]
	if !ok {
		return nil, ErrNoNameSpans
	}
	return spans, nil
}
