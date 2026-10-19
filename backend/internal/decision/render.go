package decision

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"personal-crm/backend/internal/threadctx"
)

// wireRequest is what a vendor reads: the state as text, and the questions
// with each option named by the label the model reads.
type wireRequest struct {
	State     string
	Questions []wireQuestion
}

type wireQuestion struct {
	ID           string
	Kind         threadctx.QuestionKind
	Instructions string
	Options      []wireOption
}

// wireOption is one option as sent: the name the model reads, unique within
// its question, and the option key it maps back to.
type wireOption struct {
	Key, Name, Description string
}

// render writes a context's entries and questions as a vendor reads them.
// Each entry is labeled with its message reference, its sender's role and its
// local time.
func render(kind threadctx.ContextKind, cutOff time.Time, entries []threadctx.Entry, questions []threadctx.Question) wireRequest {
	var b strings.Builder
	if kind == threadctx.KindMeeting {
		fmt.Fprintf(&b, "Meeting notes ME wrote, read up to %s.\n", stamp(cutOff))
	} else {
		fmt.Fprintf(&b, "Conversation messages, read up to %s. ME is the user.\n", stamp(cutOff))
	}
	for i, e := range entries {
		fmt.Fprintf(&b, "\n[%s] %s, %s\n", messageRef(i), e.Role, stamp(e.SentAt))
		for _, name := range slices.Sorted(maps.Keys(e.Headers)) {
			fmt.Fprintf(&b, "%s: %s\n", name, e.Headers[name])
		}
		b.WriteString(e.Text)
		b.WriteString("\n")
	}
	out := wireRequest{State: b.String(), Questions: make([]wireQuestion, len(questions))}
	for i, q := range questions {
		wq := wireQuestion{ID: q.ID, Kind: q.Kind, Instructions: q.Instructions, Options: make([]wireOption, len(q.Options))}
		used := map[string]bool{}
		for j, o := range q.Options {
			name := o.Label
			if name == "" {
				name = o.Key
			}
			base := name
			for n := 2; used[name]; n++ {
				name = fmt.Sprintf("%s (%d)", base, n)
			}
			used[name] = true
			wq.Options[j] = wireOption{Key: o.Key, Name: name, Description: o.Description}
		}
		out.Questions[i] = wq
	}
	return out
}

func renderMasked(r threadctx.MaskedRequest) wireRequest {
	return render(r.Kind(), r.CutOff(), r.Entries(), r.Questions())
}

func stamp(t time.Time) string { return t.In(time.Local).Format("Mon 2006-01-02 15:04") }

// keyOf returns the option key a vendor's option name maps back to.
func (q wireQuestion) keyOf(name string) (string, bool) {
	for _, o := range q.Options {
		if o.Name == name {
			return o.Key, true
		}
	}
	return "", false
}

// countTokens bounds the tokens a vendor counts for a request: one per byte
// of everything the model reads, which no byte-level tokenizer exceeds, plus
// a fixed allowance per question for the vendor's own framing.
func countTokens(w wireRequest) int {
	n := len(w.State)
	for _, q := range w.Questions {
		n += questionFraming + len(q.Instructions)
		for _, o := range q.Options {
			n += len(o.Name) + len(o.Description)
		}
	}
	return n
}

// countTokensPerQuestion is countTokens for a vendor that bills the state
// once for each question.
func countTokensPerQuestion(w wireRequest) int {
	return len(w.State)*(max(len(w.Questions), 1)-1) + countTokens(w)
}

// questionFraming is the allowance per question for a vendor's framing.
const questionFraming = 64
