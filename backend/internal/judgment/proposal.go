package judgment

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
)

// A proposal is one judgment as the bench shows it: the question tree
// compiled into the answers the user reviews. The bench applies no judgment
// rule of its own; every list, state, mark and act below is decided here.

// ItemAct is one review control: the review it sends.
type ItemAct struct {
	Act    ActKind
	Review ReviewRequest
}

// ItemChange is the change a judgment suggests to a live item. DueDate is the
// later date; Confidence is nil for a removal no method answered.
type ItemChange struct {
	Kind       ChangeKind
	DueDate    *time.Time
	Confidence *float64
}

// ThreadMessage is one message of a thread's content, each message once
// however many per-contact copies hold it: a message the bench shows, among
// them the one a commitment's judgment names as stating it. Key is the
// message's key in the thread's content (S1).
type ThreadMessage struct {
	Key      string
	SentAt   time.Time
	FromUser bool
	// Sender is the contact the message is from; nil for the user's own.
	Sender *uuid.UUID
	Text   string
}

// ProposalItem is one item of a proposal, standing for exactly one judged
// outcome; every act on it reviews that outcome. Contact is the participant
// of a reply item and the counterparty of a commitment; nil for the flag.
// State is empty for an item the user removed. Acts is empty once the item is
// reviewed.
type ProposalItem struct {
	OutcomeID  uuid.UUID
	List       ProposalList
	Contact    *uuid.UUID
	Debtor     Debtor
	State      ItemState
	Confidence *float64
	DueDate    *time.Time
	Change     *ItemChange
	Source     *ThreadMessage
	Mark       ItemMark
	Acts       []ItemAct
}

// ProposalAddition is one choice of a proposal's pickers: adding Contact to a
// reply list, adding a commitment between the user and Contact, or adding the
// flag, by the review it sends.
type ProposalAddition struct {
	List      ProposalList
	Contact   *uuid.UUID
	Debtor    Debtor
	OutcomeID uuid.UUID
	Review    ReviewRequest
}

// Proposal is one judgment of a thread as the bench shows it. Thread is the
// judged thread, whose content the bench reads as ThreadContent. CutOff is the
// judgment's first appearance, as of which its content is shown; ReusedAt is
// the cut-off of its appearance in the run a view is narrowed to, when that
// run reused it. ReplyLists is false for a meeting, which ball-in-court is not
// asked of. ToReview counts the items without a review.
type Proposal struct {
	JudgmentID uuid.UUID
	Thread     threadctx.VenueID
	Method     MethodRef
	Production bool
	Kind       threadctx.ContextKind
	CutOff     time.Time
	ReusedAt   *time.Time
	ReplyLists bool
	Items      []ProposalItem
	Additions  []ProposalAddition
	ToReview   int
	// Names holds the name of every contact the proposal names.
	Names map[uuid.UUID]string
}

// SourceMessageQuestionID is the ID of the choice question naming the message
// that states the commitment a commitment question asks about. Its options are
// keyed by the thread's message keys (S1), so the method's answer names a
// message and generates no text.
func SourceMessageQuestionID(commitmentQuestionID string) string {
	return commitmentQuestionID + "/source_message"
}

// ThreadProposals returns the proposals of a thread's judgments, or of those
// appearing in run when it is given, in no particular order.
func (s *Store) ThreadProposals(ctx context.Context, thread threadctx.VenueID, run *RunID) ([]Proposal, error) {
	rows, err := s.queries.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{
		Ids: []uuid.UUID{}, ThreadID: &thread, RunID: run, ExampleIds: []string{},
	})
	if err != nil {
		return nil, fmt.Errorf("list thread judgments: %w", err)
	}
	if len(rows) == 0 {
		return []Proposal{}, nil
	}
	ids := judgmentIDs(rows)
	outcomes, err := s.outcomesByJudgment(ctx, ids)
	if err != nil {
		return nil, err
	}
	reused := map[uuid.UUID]time.Time{}
	if run != nil {
		appearances, err := s.appearances(ctx, ids, run)
		if err != nil {
			return nil, err
		}
		for id, as := range appearances {
			for _, a := range as {
				reused[id] = a.CutOff
			}
		}
	}
	live, err := loadLiveState(ctx, s.queries, thread)
	if err != nil {
		return nil, err
	}
	pc := &proposalContext{thread: thread, live: live, messages: repository.NewThreadContentRepository(s.queries)}
	proposals := make([]Proposal, len(rows))
	for i, j := range rows {
		p, err := s.compile(ctx, pc, j, outcomes[j.ID])
		if err != nil {
			return nil, err
		}
		if at, ok := reused[j.ID]; ok && !at.Equal(j.CutOff) {
			p.ReusedAt = &at
		}
		proposals[i] = p
	}
	var named []uuid.UUID
	for _, p := range proposals {
		for _, item := range p.Items {
			named = appendContact(named, item.Contact, item.Source)
		}
		for _, a := range p.Additions {
			named = appendContact(named, a.Contact, nil)
		}
	}
	names, err := s.contacts.ListContactNamesByIDs(ctx, named)
	if err != nil {
		return nil, fmt.Errorf("list contact names: %w", err)
	}
	for i := range proposals {
		proposals[i].Names = names
		sortProposal(&proposals[i], names)
	}
	return proposals, nil
}

func appendContact(ids []uuid.UUID, contact *uuid.UUID, source *ThreadMessage) []uuid.UUID {
	if contact != nil {
		ids = append(ids, *contact)
	}
	if source != nil && source.Sender != nil {
		ids = append(ids, *source.Sender)
	}
	return ids
}

// proposalContext is what compiling a thread's proposals reads besides its
// judgments: the thread's live state and content.
type proposalContext struct {
	thread   uuid.UUID
	live     *liveState
	messages *repository.ThreadContentRepository
}

// compile turns one judgment's outcomes into its proposal.
func (s *Store) compile(ctx context.Context, pc *proposalContext, j *db.ListJudgmentRowsRow, outcomes []*outcomeRecord) (Proposal, error) {
	questions, err := decodeQuestions(j.Questions)
	if err != nil {
		return Proposal{}, err
	}
	kind := threadctx.ContextKind(j.ContextKind)
	method := MethodRef{j.MethodKey, j.MethodVersion, j.WordingVersion}
	production, designated := s.ProductionMethod()
	p := Proposal{
		JudgmentID: j.ID,
		Thread:     j.ThreadID,
		Method:     method,
		Production: designated && method.Key == production,
		Kind:       kind,
		CutOff:     j.CutOff,
		ReplyLists: kind == threadctx.KindConversation,
		Items:      []ProposalItem{},
		Additions:  []ProposalAddition{},
	}
	for _, o := range outcomes {
		item, ok, err := pc.itemOf(ctx, o)
		if err != nil {
			return Proposal{}, err
		}
		if !ok {
			continue
		}
		if item.List == ProposalListCommitments && item.Mark != ItemMarkAdded && item.Change.kind() != ChangeKindRemove {
			if item.Source, err = s.sourceMessage(ctx, pc, questions, o.QuestionID); err != nil {
				return Proposal{}, err
			}
		}
		p.Items = append(p.Items, item)
	}
	p.Items = absorbReplies(p.Items)
	p.Additions = additions(outcomes, p.Items)
	for _, item := range p.Items {
		if item.Mark == "" {
			p.ToReview++
		}
	}
	return p, nil
}

func (c *ItemChange) kind() ChangeKind {
	if c == nil {
		return ""
	}
	return c.Kind
}

// itemOf is the item an outcome stands for, and false when it stands for
// none: an outcome whose value has no effect, unless it would remove a live
// item, and unless the user's review gave it one. An item whose live effect
// no longer holds acts no more, so it is not acting.
func (pc *proposalContext) itemOf(ctx context.Context, o *outcomeRecord) (ProposalItem, bool, error) {
	change := changeOf(o)
	// before is the list the item sits in before any review: the judged
	// value's, or for a suggested change the list of the live value it would
	// take away.
	before := listOf(o.Question, o.value())
	if change != "" {
		before = listOf(o.Question, o.kept())
	}

	item := ProposalItem{OutcomeID: o.ID, Contact: o.ContactID, Debtor: debtorOf(o.Question)}
	if o.review == nil {
		if before == "" {
			return ProposalItem{}, false, nil
		}
		item.List = before
		item.Acts = actsOf(o, change)
	} else {
		right := *o.review.ValueText
		switch {
		case isNoEffectValue(right) && before == "":
			return ProposalItem{}, false, nil
		case isNoEffectValue(right):
			item.List, item.Mark = before, ItemMarkRemoved
		case o.adds(right):
			item.List, item.Mark = listOf(o.Question, right), ItemMarkAdded
		default:
			item.List, item.Mark = listOf(o.Question, right), ItemMarkConfirmed
		}
	}

	gone, err := pc.live.effectGone(ctx, o)
	if err != nil {
		return ProposalItem{}, false, err
	}
	switch {
	case item.Mark == ItemMarkRemoved:
	case o.state() == OutcomeStateEvidence, gone:
		item.State = ItemStateNotActing
	case o.review != nil:
		item.State = ItemStateLive
	case change != "":
		item.State = ItemStateLiveWithChange
	case o.state() == OutcomeStateClaim:
		item.State = ItemStateSuggested
	default:
		item.State = ItemStateLive
	}

	switch {
	case change != "" && o.review == nil:
		item.Change = &ItemChange{Kind: change, Confidence: o.Confidence}
		if change == ChangeKindLaterDate {
			item.Change.DueDate = o.DueDate
		}
	case change == "" && item.Mark != ItemMarkAdded:
		item.Confidence = o.Confidence
	}

	if item.List == ProposalListCommitments {
		switch {
		case item.Mark == ItemMarkAdded:
		case change == ChangeKindLaterDate && o.review != nil && !o.keepsCurrentDate():
			item.DueDate = o.DueDate
		case change != "":
			item.DueDate = pc.live.obligationDue(o)
		default:
			item.DueDate = o.DueDate
		}
	}
	if item.List == ProposalListBigMoment {
		item.Contact = nil
	}
	return item, true, nil
}

// changeOf is the change an outcome landed as a removal suggests to a live
// item: removing it, for a value with no effect, or moving an open commitment
// later.
func changeOf(o *outcomeRecord) ChangeKind {
	if o.class == nil || o.class.PredicateKey != predicateRemoval {
		return ""
	}
	switch {
	case isNoEffectValue(o.value()):
		return ChangeKindRemove
	case o.value() == string(CommitmentStateOpen):
		return ChangeKindLaterDate
	default:
		return ""
	}
}

// listOf is the list an outcome value puts an item in; empty for a value with
// no effect.
func listOf(question Question, value string) ProposalList {
	switch {
	case value == string(BallInCourtIOweReply):
		return ProposalListIOweReply
	case value == string(BallInCourtTheyOweReply):
		return ProposalListOwesMeReply
	case question == QuestionBigMoment && !isNoEffectValue(value):
		return ProposalListBigMoment
	case (question == QuestionCommitmentMine || question == QuestionCommitmentTheirs) && value == string(CommitmentStateOpen):
		return ProposalListCommitments
	default:
		return ""
	}
}

// replyValue is the ball-in-court value that puts a contact in a reply list.
func replyValue(list ProposalList) string {
	if list == ProposalListIOweReply {
		return string(BallInCourtIOweReply)
	}
	return string(BallInCourtTheyOweReply)
}

// noEffectValue is the value that removes a question's item.
func noEffectValue(question Question) string {
	if question == QuestionBallInCourt {
		return string(BallInCourtNobodyOwes)
	}
	return string(CommitmentStateNone)
}

func debtorOf(question Question) Debtor {
	switch question {
	case QuestionCommitmentMine:
		return DebtorMe
	case QuestionCommitmentTheirs:
		return DebtorContact
	default:
		return ""
	}
}

// actsOf are an unreviewed item's review controls. Keep states the live
// value the suggested removal would take away.
func actsOf(o *outcomeRecord, change ChangeKind) []ItemAct {
	judged := o.value()
	switch change {
	case ChangeKindRemove:
		return []ItemAct{
			{Act: ActKindAccept, Review: ReviewRequest{RightOutcome: judged}},
			{Act: ActKindKeep, Review: ReviewRequest{RightOutcome: o.kept()}},
		}
	case ChangeKindLaterDate:
		return []ItemAct{
			{Act: ActKindAccept, Review: ReviewRequest{RightOutcome: judged}},
			{Act: ActKindKeepOriginal, Review: ReviewRequest{RightOutcome: judged, KeepCurrentDate: true}},
		}
	default:
		return []ItemAct{
			{Act: ActKindConfirm, Review: ReviewRequest{RightOutcome: judged}},
			{Act: ActKindRemove, Review: ReviewRequest{RightOutcome: noEffectValue(o.Question)}},
		}
	}
}

// absorbReplies drops each unreviewed reply item whose party has a commitment
// in the same direction: the reply that states a commitment and the
// commitment are one obligation.
func absorbReplies(items []ProposalItem) []ProposalItem {
	committed := map[Debtor]map[uuid.UUID]bool{DebtorMe: {}, DebtorContact: {}}
	for _, item := range items {
		if item.List == ProposalListCommitments && item.Mark != ItemMarkRemoved && item.Contact != nil {
			committed[item.Debtor][*item.Contact] = true
		}
	}
	return slices.DeleteFunc(items, func(item ProposalItem) bool {
		if item.Mark != "" || item.Contact == nil {
			return false
		}
		return (item.List == ProposalListIOweReply && committed[DebtorMe][*item.Contact]) ||
			(item.List == ProposalListOwesMeReply && committed[DebtorContact][*item.Contact])
	})
}

// additions are the pickers' choices: each participant not already in a
// reply list whose ball-in-court outcome has no review, each commitment not
// already an item whose outcome has no review, and the flag while the thread
// has none and its big-moment outcome has no review.
func additions(outcomes []*outcomeRecord, items []ProposalItem) []ProposalAddition {
	has := func(list ProposalList, debtor Debtor, contact *uuid.UUID) bool {
		return slices.ContainsFunc(items, func(item ProposalItem) bool {
			return item.List == list && item.Debtor == debtor &&
				(contact == nil || (item.Contact != nil && *item.Contact == *contact))
		})
	}
	out := []ProposalAddition{}
	for _, o := range outcomes {
		if o.review != nil || o.QuestionID == "" {
			continue
		}
		switch o.Question {
		case QuestionBallInCourt:
			for _, list := range []ProposalList{ProposalListIOweReply, ProposalListOwesMeReply} {
				if !has(list, "", o.ContactID) {
					out = append(out, ProposalAddition{List: list, Contact: o.ContactID, OutcomeID: o.ID, Review: ReviewRequest{RightOutcome: replyValue(list)}})
				}
			}
		case QuestionCommitmentMine, QuestionCommitmentTheirs:
			debtor := debtorOf(o.Question)
			if !has(ProposalListCommitments, debtor, o.ContactID) {
				out = append(out, ProposalAddition{List: ProposalListCommitments, Contact: o.ContactID, Debtor: debtor, OutcomeID: o.ID, Review: ReviewRequest{RightOutcome: string(CommitmentStateOpen)}})
			}
		case QuestionBigMoment:
			if !has(ProposalListBigMoment, "", nil) {
				out = append(out, ProposalAddition{List: ProposalListBigMoment, OutcomeID: o.ID, Review: ReviewRequest{RightOutcome: string(BigMomentParticipant)}})
			}
		}
	}
	return out
}

// sourceMessage is the message the judgment names as stating the commitment
// its question asks about; nil when the judgment names none.
func (s *Store) sourceMessage(ctx context.Context, pc *proposalContext, questions []AnsweredQuestion, commitmentQuestionID string) (*ThreadMessage, error) {
	if commitmentQuestionID == "" {
		return nil, nil
	}
	i := slices.IndexFunc(questions, func(q AnsweredQuestion) bool { return q.ID == SourceMessageQuestionID(commitmentQuestionID) })
	if i < 0 {
		return nil, nil
	}
	key := chosenOption(questions[i])
	if key == "" {
		return nil, nil
	}
	m, err := pc.messages.GetThreadMessage(ctx, pc.thread, key)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	source := &ThreadMessage{Key: key, SentAt: m.SentAt, FromUser: m.FromUser, Text: m.Body}
	if !m.FromUser {
		source.Sender = m.ContactID
	}
	return source, nil
}

// chosenOption is a choice question's answer: the model's original answer,
// else its most probable option.
func chosenOption(q AnsweredQuestion) string {
	if q.OriginalAnswer != "" {
		return q.OriginalAnswer
	}
	best, bestP := "", -1.0
	keys := make([]string, 0, len(q.Probabilities))
	for k := range q.Probabilities {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if q.Probabilities[k] > bestP {
			best, bestP = k, q.Probabilities[k]
		}
	}
	return best
}

// sortProposal orders a proposal's items and additions by the participant's
// name, the user's own commitment before the participant's.
func sortProposal(p *Proposal, names map[uuid.UUID]string) {
	name := func(id *uuid.UUID) string {
		if id == nil {
			return ""
		}
		return strings.ToLower(names[*id])
	}
	id := func(c *uuid.UUID) []byte {
		if c == nil {
			return nil
		}
		return c[:]
	}
	listRank := map[ProposalList]int{ProposalListIOweReply: 0, ProposalListOwesMeReply: 1, ProposalListCommitments: 2, ProposalListBigMoment: 3}
	debtorRank := map[Debtor]int{"": 0, DebtorMe: 0, DebtorContact: 1}
	slices.SortStableFunc(p.Items, func(a, b ProposalItem) int {
		return cmp.Or(
			cmp.Compare(listRank[a.List], listRank[b.List]),
			strings.Compare(name(a.Contact), name(b.Contact)),
			bytes.Compare(id(a.Contact), id(b.Contact)),
			cmp.Compare(debtorRank[a.Debtor], debtorRank[b.Debtor]),
		)
	})
	slices.SortStableFunc(p.Additions, func(a, b ProposalAddition) int {
		return cmp.Or(
			cmp.Compare(listRank[a.List], listRank[b.List]),
			strings.Compare(name(a.Contact), name(b.Contact)),
			bytes.Compare(id(a.Contact), id(b.Contact)),
			cmp.Compare(debtorRank[a.Debtor], debtorRank[b.Debtor]),
		)
	})
}
