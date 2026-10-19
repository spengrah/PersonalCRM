package decision

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
)

// tree is the question tree of one window of a thread context: the questions a
// method asks of it, and what maps their answers back to judgment records on
// the Pi.
type tree struct {
	kind      threadctx.ContextKind
	questions []threadctx.Question
	parties   []party
	// flags and clearings map a question ID to the entry it concerns. Question
	// IDs name a message by its position in the window, since a message key is
	// a source identifier that stays on the Pi.
	flags     map[string]flagTarget
	clearings map[string]clearingTarget
}

// party is a participant the tree asks about: a matched CRM contact other than
// the user. A participant who is not a contact is context only.
type party struct {
	role    threadctx.Role
	contact uuid.UUID
}

type flagTarget struct {
	entry int
	kind  judgment.FlagKind
}

type clearingTarget struct {
	entry      int
	commitment Commitment
}

// window is the part of a thread context one request reads: the entries from
// start on, the entries among them that are new at the cut-off, and the
// earlier commitments inside it that the request re-checks. Entry indexes
// are positions in the whole context.
type window struct {
	start     int
	new       []int
	rechecked []recheck
}

type recheck struct {
	entry      int
	commitment Commitment
}

// Question IDs name their participant by role label.
func ballInCourtID(r threadctx.Role) string {
	return string(judgment.QuestionBallInCourt) + "/" + string(r)
}

func flagID(kind judgment.FlagKind, ref string) string { return "flag/" + string(kind) + "/" + ref }

func clearingID(ref string) string { return "clearing/" + ref }

// messageRef names the i-th entry of a window, as the rendered state labels it.
func messageRef(i int) string { return "m" + strconv.Itoa(i+1) }

// buildTree builds the question tree of a window: per matched contact,
// ball-in-court (conversations only); per new message, one yes/no question per
// flag kind; per commitment re-checked, one yes/no clearing question.
func buildTree(c threadctx.Context, w Wording, win window) tree {
	t := tree{kind: c.Kind, flags: map[string]flagTarget{}, clearings: map[string]clearingTarget{}}
	for _, role := range slices.Sorted(maps.Keys(c.Participants)) {
		p := c.Participants[role]
		if p.IsUser || p.ContactID == nil {
			continue
		}
		t.parties = append(t.parties, party{role: role, contact: *p.ContactID})
	}
	if c.Kind == threadctx.KindConversation {
		for _, p := range t.parties {
			fill := strings.NewReplacer("{party}", string(p.role)).Replace
			t.questions = append(t.questions, question(ballInCourtID(p.role), threadctx.QuestionChoice, w.BallInCourt, fill))
		}
	}
	flagWording := map[judgment.FlagKind]QuestionWording{
		judgment.FlagKindCommitment: w.FlagCommitment,
		judgment.FlagKindBigMoment:  w.FlagBigMoment,
		judgment.FlagKindClaim:      w.FlagClaim,
	}
	for _, entry := range win.new {
		ref := messageRef(entry - win.start)
		fill := strings.NewReplacer("{message}", ref).Replace
		for _, kind := range judgment.FlagKinds {
			id := flagID(kind, ref)
			t.flags[id] = flagTarget{entry: entry, kind: kind}
			t.questions = append(t.questions, question(id, threadctx.QuestionNoul, flagWording[kind], fill))
		}
	}
	for _, r := range win.rechecked {
		ref := messageRef(r.entry - win.start)
		id := clearingID(ref)
		t.clearings[id] = clearingTarget(r)
		t.questions = append(t.questions, question(id, threadctx.QuestionNoul, w.Clearing, strings.NewReplacer("{message}", ref).Replace))
	}
	return t
}

// question builds one question from its wording.
func question(id string, kind threadctx.QuestionKind, w QuestionWording, fill func(string) string) threadctx.Question {
	q := threadctx.Question{ID: id, Kind: kind, Instructions: fill(w.Instructions)}
	for _, o := range w.Options {
		q.Options = append(q.Options, threadctx.Option{Key: o.Key, Label: fill(o.Label), Description: fill(o.Description)})
	}
	return q
}

// answers is what a tree's answers give: the ball-in-court questions as
// answered and their outcomes, and the flag and clearing decisions.
type answers struct {
	questions []judgment.AnsweredQuestion
	outcomes  judgment.Outcomes
	flags     []judgment.FlagDecision
	clearings []judgment.ClearingDecision
}

// answer maps a response onto the tree. Every question must be answered with
// every option; a response that leaves one unanswered is an error.
func (t tree) answer(c threadctx.Context, byQuestion map[string]Answer) (answers, error) {
	probs := make(map[string]map[string]float64, len(t.questions))
	chosen := make(map[string]string, len(t.questions))
	for _, q := range t.questions {
		a, ok := byQuestion[q.ID]
		if !ok {
			return answers{}, fmt.Errorf("%w: question %s", errIncompleteAnswers, q.ID)
		}
		p := make(map[string]float64, len(q.Options))
		for _, o := range q.Options {
			v, ok := a.Probabilities[o.Key]
			if !ok {
				return answers{}, fmt.Errorf("%w: option %s of %s", errIncompleteAnswers, o.Key, q.ID)
			}
			p[o.Key] = v
		}
		probs[q.ID] = p
		if _, ok := p[a.Chosen]; ok {
			chosen[q.ID] = a.Chosen
		}
	}

	var out answers
	if t.kind == threadctx.KindConversation {
		for _, p := range t.parties {
			id := ballInCourtID(p.role)
			out.questions = append(out.questions, judgment.AnsweredQuestion{ID: id, Probabilities: probs[id], OriginalAnswer: chosen[id]})
			value, confidence := decide(t.options(id), probs[id], chosen[id])
			out.outcomes.BallInCourt = append(out.outcomes.BallInCourt, judgment.ParticipantOutcome[judgment.BallInCourt]{
				QuestionID: id, ContactID: p.contact, Outcome: judgment.BallInCourt(value), Confidence: confidence,
			})
		}
	}
	for _, q := range t.questions {
		if f, ok := t.flags[q.ID]; ok {
			out.flags = append(out.flags, judgment.FlagDecision{MessageKey: c.Entries[f.entry].Key, Kind: f.kind, Probability: probability(probs[q.ID][optionYes])})
		}
	}
	for _, q := range t.questions {
		if r, ok := t.clearings[q.ID]; ok {
			p := probability(probs[q.ID][optionDone])
			done := chosen[q.ID] == optionDone || (chosen[q.ID] == "" && p > probs[q.ID][optionNotDone])
			out.clearings = append(out.clearings, judgment.ClearingDecision{
				CommitmentMessageKey: r.commitment.MessageKey, CommitmentJudgment: r.commitment.Judgment, Done: done, Probability: p,
			})
		}
	}
	return out, nil
}

func (t tree) options(id string) []threadctx.Option {
	i := slices.IndexFunc(t.questions, func(q threadctx.Question) bool { return q.ID == id })
	return t.questions[i].Options
}

// probability bounds a vendor's probability to [0, 1].
func probability(p float64) float64 { return min(max(p, 0), 1) }

// decide returns the outcome an answer gives and its probability. Options
// keyed "<outcome>.<reason>" are one outcome, their probabilities summed. The
// outcome is the option the model chose; where it chose none, or chose a
// reason, which splits an outcome rather than naming one, it is the most
// probable outcome, ties going to the earlier option.
func decide(options []threadctx.Option, p map[string]float64, chosen string) (string, float64) {
	var order []string
	sums := map[string]float64{}
	for _, o := range options {
		outcome := judgment.OptionValue(o.Key)
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
