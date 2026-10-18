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
// Mark is how the decision's label, from whichever proposal it was made in,
// left the item; empty while the decision has no label. State is the item's
// live state, beside its mark where the two disagree; empty only for a
// removal that took effect. Acts are its review controls; on an item with a
// mark they are its re-review controls, which record the label only.
// LiveDueDate is the obligation's live date, set only for a live commitment
// with a label whose obligation holds a date other than the one the item
// shows, whatever change its outcome suggested.
type ProposalItem struct {
	OutcomeID   uuid.UUID
	List        ProposalList
	Contact     *uuid.UUID
	Debtor      Debtor
	State       ItemState
	Confidence  *float64
	DueDate     *time.Time
	Change      *ItemChange
	Source      *ThreadMessage
	Mark        ItemMark
	Acts        []ItemAct
	LiveDueDate *time.Time
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
// asked of. ToReview counts the items without a label. ReviewStatus is
// what remains to review; ConfirmNones is true exactly when the proposal has
// no items and holds a negative decision without a label that no proposal on
// the same content holds as an item without a label, so Confirm Nones would
// label it. Lists holds one entry per list the proposal shows, in display
// order.
type Proposal struct {
	JudgmentID   uuid.UUID
	Thread       threadctx.VenueID
	Method       MethodRef
	Production   bool
	Kind         threadctx.ContextKind
	CutOff       time.Time
	ReusedAt     *time.Time
	ReplyLists   bool
	Items        []ProposalItem
	Additions    []ProposalAddition
	ToReview     int
	ReviewStatus ReviewStatus
	ConfirmNones bool
	Lists        []ProposalListView
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
	return s.threadProposals(ctx, thread, rows, run)
}

// ThreadProposalsOf returns the proposals of the thread the judgment judged,
// as ThreadProposals does: what a save on the judgment's proposal can change
// on the bench. It returns ErrNotFound for an unknown judgment.
func (s *Store) ThreadProposalsOf(ctx context.Context, judgment uuid.UUID, run *RunID) ([]Proposal, error) {
	rows, err := s.queries.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: []uuid.UUID{judgment}, ExampleIds: []string{}})
	if err != nil {
		return nil, fmt.Errorf("load judgment %s: %w", judgment, err)
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return s.ThreadProposals(ctx, rows[0].ThreadID, run)
}

// Proposals returns the proposals of the given judgments, as the bench shows
// them in a view narrowed to run when it is given, in no particular order. It
// returns ErrNotFound when a judgment is unknown.
func (s *Store) Proposals(ctx context.Context, judgments []uuid.UUID, run *RunID) ([]Proposal, error) {
	if len(judgments) == 0 {
		return []Proposal{}, nil
	}
	rows, err := s.queries.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: judgments, ExampleIds: []string{}})
	if err != nil {
		return nil, fmt.Errorf("list judgments: %w", err)
	}
	for _, id := range judgments {
		if !slices.ContainsFunc(rows, func(j *db.ListJudgmentRowsRow) bool { return j.ID == id }) {
			return nil, ErrNotFound
		}
	}
	byThread := map[uuid.UUID][]*db.ListJudgmentRowsRow{}
	var threads []uuid.UUID
	for _, j := range rows {
		if _, seen := byThread[j.ThreadID]; !seen {
			threads = append(threads, j.ThreadID)
		}
		byThread[j.ThreadID] = append(byThread[j.ThreadID], j)
	}
	proposals := []Proposal{}
	for _, thread := range threads {
		ps, err := s.threadProposals(ctx, thread, byThread[thread], run)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, ps...)
	}
	return proposals, nil
}

// threadProposals compiles the proposals of judgments of one thread.
func (s *Store) threadProposals(ctx context.Context, thread threadctx.VenueID, rows []*db.ListJudgmentRowsRow, run *RunID) ([]Proposal, error) {
	if len(rows) == 0 {
		return []Proposal{}, nil
	}
	ids := judgmentIDs(rows)
	outcomes, lb, err := loadContent(ctx, s.queries, rows)
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
	pc := &proposalContext{thread: thread, live: live, labels: lb, unreviewed: unreviewedItems(outcomes, lb), messages: repository.NewThreadContentRepository(s.queries)}
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
		for _, l := range p.Lists {
			if l.Closest != nil {
				named = appendContact(named, l.Closest.Contact, nil)
			}
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
// judgments: the thread's live state, its decisions' labels, the decisions
// awaiting review as an item in a proposal on the same content, and its
// content.
type proposalContext struct {
	thread     uuid.UUID
	live       *liveState
	labels     labels
	unreviewed map[decisionKey]bool
	messages   *repository.ThreadContentRepository
}

// loadContent reads the proposals on the content the given judgments judged:
// the outcomes of every judgment on that content, whichever run or row it
// appears in, by judgment, and the labels of its decisions.
func loadContent(ctx context.Context, q db.Querier, rows []*db.ListJudgmentRowsRow) (map[uuid.UUID][]*outcomeRecord, labels, error) {
	examples := make([]ExampleID, len(rows))
	ids := make([]string, len(rows))
	for i, j := range rows {
		examples[i], ids[i] = ExampleID(j.ExampleID), j.ExampleID
	}
	judgments, err := q.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: []uuid.UUID{}, ExampleIds: ids})
	if err != nil {
		return nil, nil, fmt.Errorf("list content judgments: %w", err)
	}
	records, err := loadOutcomes(ctx, q, judgmentIDs(judgments), nil)
	if err != nil {
		return nil, nil, err
	}
	byJudgment := map[uuid.UUID][]*outcomeRecord{}
	for _, o := range records {
		byJudgment[o.JudgmentID] = append(byJudgment[o.JudgmentID], o)
	}
	lb, err := loadLabels(ctx, q, examples)
	if err != nil {
		return nil, nil, err
	}
	return byJudgment, lb, nil
}

// unreviewedItems are the decisions without a label that some proposal on the
// content holds as an item: each awaits the user's review of that item, in
// every proposal on the content.
func unreviewedItems(byJudgment map[uuid.UUID][]*outcomeRecord, lb labels) map[decisionKey]bool {
	unreviewed := map[decisionKey]bool{}
	for _, outcomes := range byJudgment {
		for _, pl := range sortOutcomes(outcomes, lb).items {
			if pl.label == nil {
				unreviewed[pl.o.decision()] = true
			}
		}
	}
	return unreviewed
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
	sorted := sortOutcomes(outcomes, pc.labels)
	for _, pl := range sorted.items {
		item, err := pc.itemOf(ctx, pl)
		if err != nil {
			return Proposal{}, err
		}
		if item.List == ProposalListCommitments && !pl.added && item.Change.kind() != ChangeKindRemove {
			if item.Source, err = s.sourceMessage(ctx, pc, questions, pl.o.QuestionID); err != nil {
				return Proposal{}, err
			}
		}
		if item.Mark == "" {
			p.ToReview++
		}
		p.Items = append(p.Items, item)
	}
	p.Additions = additions(outcomes, sorted, pc.labels)
	nones := len(sorted.unlabeledNegatives(pc.labels, nil)) > 0
	switch {
	case p.ToReview > 0:
		p.ReviewStatus = ReviewStatusToReview
	case nones:
		p.ReviewStatus = ReviewStatusNonesToReview
	default:
		p.ReviewStatus = ReviewStatusAllReviewed
	}
	p.ConfirmNones = len(sorted.confirmNones(pc.labels, pc.unreviewed)) > 0
	p.Lists = listViews(p.ReplyLists, questions, outcomes, sorted, pc.labels)
	return p, nil
}

// placement is an outcome standing for an item of its proposal: the value
// the item stands for and the list that value puts it in, whether the user
// added that value, the decision's label and the mark it gives the item.
type placement struct {
	o     *outcomeRecord
	value string
	list  ProposalList
	added bool
	label *label
	mark  ItemMark
}

// place is where an outcome sits under its decision's label: an item in a
// list, or none for an outcome whose value has no effect, unless it would
// remove a live item, and unless the label gives it one. A label with no
// effect removes the item the decision's last review with an effect stood
// for, so the item keeps the list that review put it in.
func place(o *outcomeRecord, l *label) (placement, bool) {
	// The item stands, before any label, for the judged value, or for a
	// suggested change the live value it would take away.
	pl := placement{o: o, label: l, value: o.value()}
	if changeOf(o) != "" {
		pl.value = o.kept()
	}
	switch {
	case l == nil:
	case isNoEffectValue(l.right):
		pl.mark = ItemMarkRemoved
		if o.LastEffect != "" && o.adds(o.LastEffect) {
			pl.value, pl.added = o.LastEffect, true
		}
	case o.adds(l.right):
		pl.value, pl.added, pl.mark = l.right, true, ItemMarkAdded
	default:
		pl.value, pl.mark = l.right, ItemMarkConfirmed
	}
	pl.list = listOf(o.Question, pl.value)
	return pl, pl.list != ""
}

// sorting is a judgment's outcomes sorted by what they compile into under
// their decisions' labels: the outcomes standing for its items, and its
// negative decisions, the answered outcomes standing for no item. A reply
// its party's commitment in the same direction absorbs is neither: it is a
// positive decision the commitment stands for.
type sorting struct {
	items     []placement
	negatives []*outcomeRecord
}

// sortOutcomes sorts a judgment's outcomes into its items and its negative
// decisions. The bench's compile and the labeling rule both read a proposal
// through it.
func sortOutcomes(outcomes []*outcomeRecord, lb labels) sorting {
	var out sorting
	committed := map[Debtor]map[uuid.UUID]bool{DebtorMe: {}, DebtorContact: {}}
	for _, o := range outcomes {
		pl, ok := place(o, lb.of(o))
		switch {
		case ok:
			out.items = append(out.items, pl)
			if pl.list == ProposalListCommitments && pl.mark != ItemMarkRemoved && o.ContactID != nil {
				committed[debtorOf(o.Question)][*o.ContactID] = true
			}
		case o.QuestionID != "":
			out.negatives = append(out.negatives, o)
		}
	}
	out.items = slices.DeleteFunc(out.items, func(pl placement) bool {
		if pl.label != nil || pl.o.ContactID == nil {
			return false
		}
		return (pl.list == ProposalListIOweReply && committed[DebtorMe][*pl.o.ContactID]) ||
			(pl.list == ProposalListOwesMeReply && committed[DebtorContact][*pl.o.ContactID])
	})
	return out
}

// unlabeledNegatives are the negative decisions without a label, but for
// those among except. The labeling rule labels those it returns with except
// the decisions awaiting review as an item (G4); a proposal's status counts
// every one.
func (st sorting) unlabeledNegatives(lb labels, except map[decisionKey]bool) []*outcomeRecord {
	var out []*outcomeRecord
	for _, o := range st.negatives {
		if lb.of(o) == nil && !except[o.decision()] {
			out = append(out, o)
		}
	}
	return out
}

// confirmNones are the negative decisions Confirm Nones labels: none for a
// proposal with an item, else its negative decisions without a label but for
// those among unreviewed. The offer of Confirm Nones and its refusal both
// read it.
func (st sorting) confirmNones(lb labels, unreviewed map[decisionKey]bool) []*outcomeRecord {
	if len(st.items) > 0 {
		return nil
	}
	return st.unlabeledNegatives(lb, unreviewed)
}

// holdsLabel reports whether any decision of the proposal, an item's or a
// negative one, has a label, from whichever proposal it was made in.
func (st sorting) holdsLabel(lb labels) bool {
	return slices.ContainsFunc(st.items, func(pl placement) bool { return pl.label != nil }) ||
		slices.ContainsFunc(st.negatives, func(o *outcomeRecord) bool { return lb.of(o) != nil })
}

func (c *ItemChange) kind() ChangeKind {
	if c == nil {
		return ""
	}
	return c.Kind
}

// itemOf is the item a placement stands for. An item without a label shows
// how its outcome landed; an item whose live effect no longer holds acts no
// more, so it is not acting. An item with a label shows its live state beside
// its mark: live while the effect of the value the item stands for holds,
// else not acting, or for a removal no state at all, since the removal took
// effect.
func (pc *proposalContext) itemOf(ctx context.Context, pl placement) (ProposalItem, error) {
	o, l, change := pl.o, pl.label, changeOf(pl.o)
	item := ProposalItem{
		OutcomeID: o.ID, List: pl.list, Contact: o.ContactID, Debtor: debtorOf(o.Question),
		Mark: pl.mark, Acts: actsOf(pl, change),
	}
	if l == nil {
		gone, err := pc.live.effectGone(ctx, o)
		if err != nil {
			return ProposalItem{}, err
		}
		switch {
		case o.state() == OutcomeStateEvidence, gone:
			item.State = ItemStateNotActing
		case change != "":
			item.State = ItemStateLiveWithChange
		case o.state() == OutcomeStateClaim:
			item.State = ItemStateSuggested
		default:
			item.State = ItemStateLive
		}
	} else {
		holds, err := pc.live.holds(ctx, o, pl.value)
		if err != nil {
			return ProposalItem{}, err
		}
		switch {
		case holds:
			item.State = ItemStateLive
		case pl.mark != ItemMarkRemoved:
			item.State = ItemStateNotActing
		}
	}

	switch {
	case change != "" && l == nil:
		item.Change = &ItemChange{Kind: change, Confidence: o.Confidence}
		if change == ChangeKindLaterDate {
			item.Change.DueDate = o.DueDate
		}
	case change == "" && !pl.added:
		item.Confidence = o.Confidence
	}

	if item.List == ProposalListCommitments {
		live := pc.live.obligationDue(o)
		switch {
		// A label that keeps the date keeps the decision's kept date in every
		// proposal, however its outcome landed.
		case l != nil && l.keepsDate:
			item.DueDate = cmp.Or(o.KeptDueDate, live)
		case pl.added:
		case change == ChangeKindLaterDate && l != nil:
			item.DueDate = o.DueDate
		case change != "":
			item.DueDate = live
		default:
			item.DueDate = o.DueDate
		}
		// A label states the date its item shows; where the live obligation
		// holds another, the item carries the live date beside it.
		if l != nil && item.State == ItemStateLive && item.DueDate != nil && live != nil && !live.Equal(*item.DueDate) {
			item.LiveDueDate = live
		}
	}
	item.Contact = specOf(item.List).party(o)
	return item, nil
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

// actsOf are an item's review controls, the same before and after a label:
// confirm states the value the item stands for (for an item the user added,
// the value the user added), and remove the question's no-effect value; for
// a suggested change, accept states the judged outcome and keep the live
// value the suggested removal would take away, or keep original the judged
// outcome with the current date kept.
func actsOf(pl placement, change ChangeKind) []ItemAct {
	o, judged := pl.o, pl.o.value()
	switch {
	case pl.added:
		return []ItemAct{
			{Act: ActKindConfirm, Review: ReviewRequest{RightOutcome: pl.value}},
			{Act: ActKindRemove, Review: ReviewRequest{RightOutcome: noEffectValue(o.Question)}},
		}
	case change == ChangeKindRemove:
		return []ItemAct{
			{Act: ActKindAccept, Review: ReviewRequest{RightOutcome: judged}},
			{Act: ActKindKeep, Review: ReviewRequest{RightOutcome: o.kept()}},
		}
	case change == ChangeKindLaterDate:
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

// additions are the pickers' choices: for each answered outcome whose
// decision has no label or is a negative decision, labeled or not, each list
// of its question that does not already hold an item of the same debtor and
// the party the list names it by (any item, for a list that names none). A
// decision that is positive and has a label is changed only through its
// item's controls.
func additions(outcomes []*outcomeRecord, sorted sorting, lb labels) []ProposalAddition {
	has := func(list ProposalList, debtor Debtor, contact *uuid.UUID) bool {
		return slices.ContainsFunc(sorted.items, func(pl placement) bool {
			return pl.list == list && debtorOf(pl.o.Question) == debtor &&
				(contact == nil || (pl.o.ContactID != nil && *pl.o.ContactID == *contact))
		})
	}
	out := []ProposalAddition{}
	for _, o := range outcomes {
		if o.QuestionID == "" || (lb.of(o) != nil && !slices.Contains(sorted.negatives, o)) {
			continue
		}
		for _, spec := range listSpecs {
			if !slices.Contains(spec.questions, o.Question) {
				continue
			}
			contact, debtor := spec.party(o), debtorOf(o.Question)
			if !has(spec.list, debtor, contact) {
				out = append(out, ProposalAddition{List: spec.list, Contact: contact, Debtor: debtor, OutcomeID: o.ID, Review: ReviewRequest{RightOutcome: spec.values[0]}})
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
	key := questions[i].Chosen()
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
	listRank := func(list ProposalList) int {
		return slices.IndexFunc(listSpecs, func(spec listSpec) bool { return spec.list == list })
	}
	debtorRank := map[Debtor]int{"": 0, DebtorMe: 0, DebtorContact: 1}
	slices.SortStableFunc(p.Items, func(a, b ProposalItem) int {
		return cmp.Or(
			cmp.Compare(listRank(a.List), listRank(b.List)),
			strings.Compare(name(a.Contact), name(b.Contact)),
			bytes.Compare(id(a.Contact), id(b.Contact)),
			cmp.Compare(debtorRank[a.Debtor], debtorRank[b.Debtor]),
		)
	})
	slices.SortStableFunc(p.Additions, func(a, b ProposalAddition) int {
		return cmp.Or(
			cmp.Compare(listRank(a.List), listRank(b.List)),
			strings.Compare(name(a.Contact), name(b.Contact)),
			bytes.Compare(id(a.Contact), id(b.Contact)),
			cmp.Compare(debtorRank[a.Debtor], debtorRank[b.Debtor]),
		)
	})
}
