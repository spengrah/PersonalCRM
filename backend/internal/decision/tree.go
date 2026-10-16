package decision

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
)

// Caps on the candidate options one question offers: a commitment's source
// message is one of its debtor's most recent messages, and its date one of the
// most recent distinct dates the thread's timing expressions resolve to.
const (
	maxMessageOptions = 48
	maxDateOptions    = 24
)

// tree is the question tree of one thread context: the questions sent in one
// request, and what maps their answers back to outcomes on the Pi.
type tree struct {
	kind      threadctx.ContextKind
	questions []threadctx.Question
	parties   []party
	// messages maps a source-message question ID to its options' message
	// keys. Option keys on the wire name messages by position only, since a
	// message key is a source identifier.
	messages map[string]map[string]string
}

// party is a participant the tree asks about: a matched CRM contact other than
// the user. A participant who is not a contact is context only.
type party struct {
	role    threadctx.Role
	contact uuid.UUID
}

// Question IDs name their participant by role label.
func ballInCourtID(r threadctx.Role) string {
	return string(judgment.QuestionBallInCourt) + "/" + string(r)
}

func commitmentID(d judgment.CommitmentDirection, r threadctx.Role) string {
	return string(commitmentQuestion(d)) + "/" + string(r)
}

func dateID(commitmentQuestionID string) string { return commitmentQuestionID + "/date" }

const bigMomentID = string(judgment.QuestionBigMoment)

func commitmentQuestion(d judgment.CommitmentDirection) judgment.Question {
	if d == judgment.CommitmentDirectionMine {
		return judgment.QuestionCommitmentMine
	}
	return judgment.QuestionCommitmentTheirs
}

var directions = []judgment.CommitmentDirection{judgment.CommitmentDirectionMine, judgment.CommitmentDirectionTheirs}

// messageRef names the i-th entry of a context, as the rendered state labels it.
func messageRef(i int) string { return "m" + strconv.Itoa(i+1) }

// buildTree builds the whole question tree of a context: per matched contact,
// ball-in-court (conversations only), and per direction a commitment question,
// its date and its source message; then one big-moment question.
func buildTree(c threadctx.Context, w Wording) tree {
	t := tree{kind: c.Kind, messages: map[string]map[string]string{}}
	for _, role := range slices.Sorted(maps.Keys(c.Participants)) {
		p := c.Participants[role]
		if p.IsUser || p.ContactID == nil {
			continue
		}
		t.parties = append(t.parties, party{role: role, contact: *p.ContactID})
	}
	dates := dateOptions(c.Entries, w.DateOption)
	for _, p := range t.parties {
		fill := strings.NewReplacer("{party}", string(p.role)).Replace
		if c.Kind == threadctx.KindConversation {
			t.questions = append(t.questions, question(ballInCourtID(p.role), threadctx.QuestionChoice, w.BallInCourt, fill, nil))
		}
		for _, d := range directions {
			debtor, creditor := threadctx.RoleMe, p.role
			commitment := w.CommitmentMine
			if d == judgment.CommitmentDirectionTheirs {
				debtor, creditor = p.role, threadctx.RoleMe
				commitment = w.CommitmentTheirs
			}
			id := commitmentID(d, p.role)
			fillCommitment := strings.NewReplacer("{party}", string(p.role), "{debtor}", string(debtor), "{creditor}", string(creditor)).Replace
			t.questions = append(t.questions, question(id, threadctx.QuestionNoul, commitment, fillCommitment, nil))
			t.questions = append(t.questions, question(dateID(id), threadctx.QuestionChoice, w.CommitmentDate, fillCommitment, dates))
			sourceID := judgment.SourceMessageQuestionID(id)
			messages, keys := messageOptions(c, debtor, w.MessageOption)
			t.messages[sourceID] = keys
			t.questions = append(t.questions, question(sourceID, threadctx.QuestionChoice, w.SourceMessage, fillCommitment, messages))
		}
	}
	t.questions = append(t.questions, question(bigMomentID, threadctx.QuestionChoice, w.BigMoment, strings.NewReplacer().Replace, nil))
	return t
}

// question builds one question: the candidate options first, then the
// wording's own.
func question(id string, kind threadctx.QuestionKind, w QuestionWording, fill func(string) string, candidates []threadctx.Option) threadctx.Question {
	q := threadctx.Question{ID: id, Kind: kind, Instructions: fill(w.Instructions), Options: slices.Clone(candidates)}
	for _, o := range w.Options {
		q.Options = append(q.Options, threadctx.Option{Key: o.Key, Label: fill(o.Label), Description: fill(o.Description)})
	}
	return q
}

// messageOptions offers the debtor's most recent messages as the message
// stating a commitment; in a meeting every note is the user's, so any note
// may state anyone's commitment. It returns the options and each option key's
// message key.
func messageOptions(c threadctx.Context, debtor threadctx.Role, label string) ([]threadctx.Option, map[string]string) {
	var picked []int
	for i := len(c.Entries) - 1; i >= 0 && len(picked) < maxMessageOptions; i-- {
		if c.Kind == threadctx.KindMeeting || c.Entries[i].Role == debtor {
			picked = append(picked, i)
		}
	}
	slices.Reverse(picked)
	options := make([]threadctx.Option, len(picked))
	keys := make(map[string]string, len(picked))
	for n, i := range picked {
		ref := messageRef(i)
		options[n] = threadctx.Option{Key: ref, Label: strings.ReplaceAll(label, "{message}", ref)}
		keys[ref] = c.Entries[i].Key
	}
	return options, keys
}

// dateOptions offers each distinct date the thread's timing expressions
// resolve to, from the most recent messages back, keyed by the date itself
// (YYYY-MM-DD) and ordered by date.
func dateOptions(entries []threadctx.Entry, label string) []threadctx.Option {
	seen := map[string]bool{}
	var options []threadctx.Option
	for i := len(entries) - 1; i >= 0 && len(options) < maxDateOptions; i-- {
		for _, f := range findDates(entries[i].Text, entries[i].SentAt) {
			key := f.date.Format(time.DateOnly)
			if seen[key] || len(options) == maxDateOptions {
				continue
			}
			seen[key] = true
			options = append(options, threadctx.Option{Key: key, Label: strings.NewReplacer(
				"{expression}", f.expression,
				"{message}", messageRef(i),
				"{date}", f.date.Format("Mon 2006-01-02"),
			).Replace(label)})
		}
	}
	slices.SortStableFunc(options, func(a, b threadctx.Option) int { return strings.Compare(a.Key, b.Key) })
	return options
}

// answer maps a response onto the tree: every question as answered, each
// option keyed as the Pi knows it, and the outcomes. An answer made
// irrelevant by one higher in the tree is kept as answered and yields no
// outcome: a participant's commitments when ball-in-court finds the thread is
// not a conversation with them, the big moment when it finds that for every
// participant, and a commitment's date when there is no commitment.
func (t tree) answer(byQuestion map[string]Answer) ([]judgment.AnsweredQuestion, judgment.Outcomes, error) {
	answered := make([]judgment.AnsweredQuestion, len(t.questions))
	probs := make(map[string]map[string]float64, len(t.questions))
	choices := make(map[string]string, len(t.questions))
	for i, q := range t.questions {
		a, ok := byQuestion[q.ID]
		if !ok {
			return nil, judgment.Outcomes{}, fmt.Errorf("%w: question %s", errIncompleteAnswers, q.ID)
		}
		p := make(map[string]float64, len(q.Options))
		for _, o := range q.Options {
			v, ok := a.Probabilities[o.Key]
			if !ok {
				return nil, judgment.Outcomes{}, fmt.Errorf("%w: option %s of %s", errIncompleteAnswers, o.Key, q.ID)
			}
			p[o.Key] = v
		}
		probs[q.ID] = p
		chosen := a.Chosen
		if _, ok := p[chosen]; !ok {
			chosen = ""
		}
		choices[q.ID] = chosen
		answered[i] = judgment.AnsweredQuestion{ID: q.ID, Probabilities: p, OriginalAnswer: chosen}
		if keys, ok := t.messages[q.ID]; ok {
			answered[i] = rekey(answered[i], keys)
		}
	}

	outcome := func(id string) (string, float64) { return decide(t.options(id), probs[id], choices[id]) }
	var out judgment.Outcomes
	notConversation := 0
	for _, p := range t.parties {
		if t.kind == threadctx.KindConversation {
			id := ballInCourtID(p.role)
			value, confidence := outcome(id)
			out.BallInCourt = append(out.BallInCourt, judgment.ParticipantOutcome[judgment.BallInCourt]{
				QuestionID: id, ContactID: p.contact, Outcome: judgment.BallInCourt(value), Confidence: confidence,
			})
			if judgment.BallInCourt(value) == judgment.BallInCourtNotApplicable {
				notConversation++
				continue
			}
		}
		for _, d := range directions {
			id := commitmentID(d, p.role)
			value, confidence := outcome(id)
			c := judgment.CommitmentOutcome{QuestionID: id, ContactID: p.contact, Direction: d, Outcome: judgment.CommitmentState(value), Confidence: confidence}
			if c.Outcome == judgment.CommitmentStateOpen {
				c.DueDate = dueDate(outcome(dateID(id)))
			}
			out.Commitments = append(out.Commitments, c)
		}
	}
	if len(t.parties) == 0 || notConversation < len(t.parties) {
		value, confidence := outcome(bigMomentID)
		out.BigMoment = judgment.Scored[judgment.BigMoment]{QuestionID: bigMomentID, Outcome: judgment.BigMoment(value), Confidence: confidence}
	}
	return answered, out, nil
}

func (t tree) options(id string) []threadctx.Option {
	i := slices.IndexFunc(t.questions, func(q threadctx.Question) bool { return q.ID == id })
	return t.questions[i].Options
}

// decide returns the outcome an answer gives and its probability. Options
// keyed "<outcome>.<reason>" are one outcome, their probabilities summed. The
// outcome is the option the model chose; where it chose none, or chose a
// reason, which splits an outcome rather than naming one, it is the most
// probable outcome, ties going to the earlier option.
func decide(options []threadctx.Option, p map[string]float64, chosen string) (string, float64) {
	var order []string
	sums := map[string]float64{}
	for _, o := range options {
		outcome, _, _ := strings.Cut(o.Key, ".")
		if _, ok := sums[outcome]; !ok {
			order = append(order, outcome)
		}
		sums[outcome] += p[o.Key]
	}
	if _, _, reasoned := strings.Cut(chosen, "."); chosen != "" && !reasoned {
		return chosen, min(sums[chosen], 1)
	}
	winner := slices.MaxFunc(order, func(a, b string) int { return cmp.Compare(sums[a], sums[b]) })
	return winner, min(sums[winner], 1)
}

// dueDate is the date a date option names; nil for the options that name
// none.
func dueDate(key string, _ float64) *time.Time {
	d, err := time.Parse(time.DateOnly, key)
	// best-effort: the fixed options are keys that are not dates and name none.
	if err != nil {
		return nil
	}
	return &d
}

// rekey keys a source-message answer by message key instead of position.
func rekey(q judgment.AnsweredQuestion, keys map[string]string) judgment.AnsweredQuestion {
	out := judgment.AnsweredQuestion{ID: q.ID, Probabilities: make(map[string]float64, len(q.Probabilities))}
	for k, v := range q.Probabilities {
		out.Probabilities[cmp.Or(keys[k], k)] = v
	}
	if q.OriginalAnswer != "" {
		out.OriginalAnswer = cmp.Or(keys[q.OriginalAnswer], q.OriginalAnswer)
	}
	return out
}
