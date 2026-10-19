package judgment

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrThreadNotFound is returned for a thread that holds no content or lives
	// in no venue sourceVenues names.
	ErrThreadNotFound = errors.New("thread not found")
	// ErrRunNotFound is returned for an unknown run.
	ErrRunNotFound = errors.New("run not found")
	// ErrInvalidFlagKind is returned for a kind outside FlagKinds.
	ErrInvalidFlagKind = errors.New("unknown flag kind")
	// ErrInvalidMessage is returned for a message key the thread does not hold.
	ErrInvalidMessage = errors.New("the thread holds no such message")
	// ErrInvalidClearing is returned for a clearing decision no judgment of the
	// thread made.
	ErrInvalidClearing = errors.New("no judgment of the thread made that clearing decision")
)

// MethodKeys is the method registry's surface the Threads tab reads: the keys
// of the compared methods. *decision.Registry satisfies it.
type MethodKeys interface {
	Keys() []string
}

// sourceVenues names the app of each source whose threads the Threads tab
// serves: the one list of those sources, and the chip each row shows. A
// thread's type is its source's interaction type (service.InteractionTypeOf),
// so the type filter offers the types these sources have.
var sourceVenues = map[string]ThreadVenue{
	repository.InteractionSourceWhatsApp:        ThreadVenueWhatsApp,
	repository.InteractionSourceTelegram:        ThreadVenueTelegram,
	repository.InteractionSourceMessages:        ThreadVenueIMessage,
	repository.InteractionSourceGChat:           ThreadVenueGoogleChat,
	repository.InteractionSourceEmail:           ThreadVenueEmail,
	repository.InteractionSourceGCal:            ThreadVenueCalendar,
	repository.InteractionSourceAnarlogSessions: ThreadVenueAnarlog,
}

// threadSources lists the sources of the type, or every source sourceVenues
// names when typ is nil. A thread of any other source has no venue, so no list
// or read holds it.
func threadSources(typ *service.InteractionType) []string {
	var sources []string
	for source := range sourceVenues {
		if typ == nil || service.InteractionTypeOf(source) == *typ {
			sources = append(sources, source)
		}
	}
	slices.Sort(sources)
	return sources
}

// ThreadTypes are the interaction types the Threads tab's type filter offers,
// in the order the Interactions tab offers them: the types of the sources
// sourceVenues names.
func ThreadTypes() []service.InteractionTypeOption {
	var out []service.InteractionTypeOption
	for _, t := range service.InteractionTypes {
		if len(threadSources(&t.Key)) > 0 {
			out = append(out, t)
		}
	}
	return out
}

// ParseThreadType returns the interaction type a filter value names, if the
// Threads tab's type filter offers it.
func ParseThreadType(raw string) (service.InteractionType, bool) {
	typ := service.InteractionType(raw)
	return typ, slices.ContainsFunc(ThreadTypes(), func(t service.InteractionTypeOption) bool { return t.Key == typ })
}

// Threads serves the Threads tab: the threads a method can judge, the runs the
// user reviews, a thread's review state in a run, and the labels the user
// records there. Every label it records goes through the store's one review
// writer.
type Threads struct {
	store   *Store
	methods MethodKeys
	threads *repository.ThreadRepository
}

// NewThreads builds the Threads tab's service over the judgment store and the
// method registry's list of compared methods.
func NewThreads(store *Store, methods MethodKeys) *Threads {
	return &Threads{
		store: store, methods: methods,
		threads: repository.NewThreadRepository(store.queries),
	}
}

// ThreadQuery selects the threads of the list. Type keeps one interaction
// type's threads, ContactID the threads that contact takes part in, and Run the threads the run
// judged. Limit and Offset page the list; a position in the list ignores them.
type ThreadQuery struct {
	Type      *service.InteractionType
	ContactID *uuid.UUID
	Run       *RunID
	Limit     int32
	Offset    int32
}

// ThreadRow is one thread of the list: Venue names its app and Type its
// interaction type. Review is set when the list is narrowed to a run.
type ThreadRow struct {
	ID           threadctx.VenueID
	Venue        ThreadVenue
	Type         service.InteractionType
	IsGroup      bool
	Title        *string
	Participants []repository.ThreadParticipant
	LatestAt     time.Time
	MessageCount int
	Review       *ThreadRowReview
}

// ThreadRowReview is what a run shows of a thread: how many new messages its
// judgments of the thread answered, and the thread's status.
type ThreadRowReview struct {
	JudgedCount int
	Status      ThreadStatus
	ToReview    int
}

// RunSummary counts the threads a run judged by status.
type ThreadRunSummary struct {
	Judged         int
	ToReview       int
	NothingFlagged int
	Reviewed       int
}

// ThreadPage is one page of the list, the number of threads the filters keep,
// and, for a list narrowed to a run, the run's summary.
type ThreadPage struct {
	Rows    []ThreadRow
	Total   int
	Summary *ThreadRunSummary
}

// ThreadPosition is a thread's place in the list as the filters keep it: its
// 1-based index, the list's total and the neighbouring threads.
type ThreadPosition struct {
	Index    int
	Total    int
	Previous *threadctx.VenueID
	Next     *threadctx.VenueID
}

// ThreadRead is a thread: its header, its messages once in thread order, its
// place in the list and, given a run, the run's review of it. Names holds the
// name of each contact the messages name.
type ThreadRead struct {
	Row      ThreadRow
	Messages []ThreadMessage
	Names    map[uuid.UUID]string
	Position *ThreadPosition
	Review   *ThreadReview
}

// ReviewRuns returns the historical and scheduled runs of the compared methods,
// newest first. A scorecard's rerun, and a run of a method the registry does
// not list, is never one.
func (t *Threads) ReviewRuns(ctx context.Context) ([]ReviewRun, error) {
	rows, err := t.store.queries.ListReviewRuns(ctx, t.methods.Keys())
	if err != nil {
		return nil, fmt.Errorf("list review runs: %w", err)
	}
	runs := make([]ReviewRun, len(rows))
	for i, r := range rows {
		runs[i] = reviewRunOf(r)
	}
	return runs, nil
}

// kept lists the threads the query keeps, newest latest message first: every
// thread holding content, narrowed by the type, the contact and the run's
// threads.
func (t *Threads) kept(ctx context.Context, q ThreadQuery, judged []uuid.UUID) ([]repository.ThreadSummary, error) {
	f := repository.ThreadFilter{ContactID: q.ContactID, Sources: threadSources(q.Type)}
	if q.Run != nil {
		f.Restrict, f.Only = true, judged
	}
	return t.threads.ListThreads(ctx, f)
}

func (t *Threads) runThreads(ctx context.Context, run RunID) ([]uuid.UUID, error) {
	ids, err := t.store.queries.ListRunThreads(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("list run threads: %w", err)
	}
	return ids, nil
}

// rows names the threads' participants.
func (t *Threads) rows(ctx context.Context, summaries []repository.ThreadSummary) ([]ThreadRow, error) {
	ids := make([]uuid.UUID, len(summaries))
	for i, s := range summaries {
		ids[i] = s.ID
	}
	participants, err := t.threads.ListThreadParticipants(ctx, ids)
	if err != nil {
		return nil, err
	}
	rows := make([]ThreadRow, len(summaries))
	for i, s := range summaries {
		rows[i] = ThreadRow{
			ID: s.ID, Venue: sourceVenues[s.Source], Type: service.InteractionTypeOf(s.Source), IsGroup: s.VenueKind == repository.VenueKindGroupChat, Title: s.Title,
			Participants: emptyIfNil(participants[s.ID]), LatestAt: s.LatestAt, MessageCount: s.MessageCount,
		}
	}
	return rows, nil
}

// List returns one page of the threads the query keeps. Narrowed to a run, each
// row adds the run's judged count and the thread's status, and the page adds
// the run's summary over every thread the run judged. It returns
// ErrRunNotFound for an unknown run.
func (t *Threads) List(ctx context.Context, q ThreadQuery) (*ThreadPage, error) {
	var (
		run    *db.JudgmentRun
		judged []uuid.UUID
		err    error
	)
	if q.Run != nil {
		if run, err = t.store.runRow(ctx, *q.Run); err != nil {
			return nil, err
		}
		if judged, err = t.runThreads(ctx, run.ID); err != nil {
			return nil, err
		}
	}
	kept, err := t.kept(ctx, q, judged)
	if err != nil {
		return nil, err
	}
	start := min(int(q.Offset), len(kept))
	rows, err := t.rows(ctx, kept[start:min(start+int(q.Limit), len(kept))])
	if err != nil {
		return nil, err
	}
	page := &ThreadPage{Rows: rows, Total: len(kept)}
	if run == nil {
		return page, nil
	}
	every, err := t.threads.ListThreads(ctx, repository.ThreadFilter{Sources: threadSources(nil), Restrict: true, Only: judged})
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(every))
	for i, s := range every {
		ids[i] = s.ID
	}
	data, err := t.store.loadReviewData(ctx, run, ids)
	if err != nil {
		return nil, err
	}
	reviews := make(map[uuid.UUID]*ThreadRowReview, len(ids))
	page.Summary = &ThreadRunSummary{Judged: len(ids)}
	for _, id := range ids {
		built, err := data.build(ctx, id)
		if err != nil {
			return nil, err
		}
		reviews[id] = &ThreadRowReview{JudgedCount: built.JudgedCount, Status: built.Status, ToReview: built.ToReview}
		switch built.Status {
		case ThreadStatusToReview:
			page.Summary.ToReview++
		case ThreadStatusNothingFlagged:
			page.Summary.NothingFlagged++
		case ThreadStatusReviewed:
			page.Summary.Reviewed++
		}
	}
	for i := range page.Rows {
		page.Rows[i].Review = reviews[page.Rows[i].ID]
	}
	return page, nil
}

// Read returns the thread, its place in the list as the query's filters keep
// it and, when the query names a run, the run's review of it. It returns
// ErrThreadNotFound for a thread holding no content and ErrRunNotFound for an
// unknown run.
func (t *Threads) Read(ctx context.Context, thread uuid.UUID, q ThreadQuery) (*ThreadRead, error) {
	summary, err := t.summary(ctx, thread)
	if err != nil {
		return nil, err
	}
	var (
		run    *db.JudgmentRun
		judged []uuid.UUID
	)
	if q.Run != nil {
		if run, err = t.store.runRow(ctx, *q.Run); err != nil {
			return nil, err
		}
		if judged, err = t.runThreads(ctx, run.ID); err != nil {
			return nil, err
		}
	}
	kept, err := t.kept(ctx, q, judged)
	if err != nil {
		return nil, err
	}
	rows, err := t.rows(ctx, []repository.ThreadSummary{*summary})
	if err != nil {
		return nil, err
	}
	messages, err := t.store.threadMessages(ctx, thread)
	if err != nil {
		return nil, err
	}
	read := &ThreadRead{Row: rows[0], Messages: messages.list, Names: messages.names}
	if at := slices.IndexFunc(kept, func(s repository.ThreadSummary) bool { return s.ID == thread }); at >= 0 {
		read.Position = &ThreadPosition{Index: at + 1, Total: len(kept)}
		if at > 0 {
			read.Position.Previous = &kept[at-1].ID
		}
		if at+1 < len(kept) {
			read.Position.Next = &kept[at+1].ID
		}
	}
	if run != nil {
		if read.Review, err = t.store.reviewThread(ctx, run, thread, messages); err != nil {
			return nil, err
		}
	}
	return read, nil
}

func (t *Threads) summary(ctx context.Context, thread uuid.UUID) (*repository.ThreadSummary, error) {
	summary, err := t.threads.GetThread(ctx, thread)
	if errors.Is(err, db.ErrNotFound) || (err == nil && sourceVenues[summary.Source] == "") {
		return nil, ErrThreadNotFound
	}
	return summary, err
}

// LabelFlag records whether a message of the thread holds a flag kind, as the
// label of the flag decision, replacing any earlier label, and returns the
// thread's review state in the run. It records nothing and returns
// ErrRunNotFound, ErrThreadNotFound, ErrInvalidFlagKind or ErrInvalidMessage
// for an unknown run, an unknown thread, a kind outside FlagKinds and a
// message the thread does not hold.
func (t *Threads) LabelFlag(ctx context.Context, run RunID, thread uuid.UUID, messageKey string, kind FlagKind, holds bool) (*ThreadReview, error) {
	runRow, err := t.store.runRow(ctx, run)
	if err != nil {
		return nil, err
	}
	if _, err := t.summary(ctx, thread); err != nil {
		return nil, err
	}
	if !slices.Contains(FlagKinds, kind) {
		return nil, ErrInvalidFlagKind
	}
	held, err := t.threads.HoldsMessage(ctx, thread, messageKey)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, ErrInvalidMessage
	}
	if err := t.store.inTx(ctx, func(tx pgx.Tx) error {
		return t.store.recordReviewTx(ctx, tx, flagReview{thread: thread, messageKey: messageKey, kind: kind, holds: holds})
	}); err != nil {
		return nil, err
	}
	return t.store.reviewStateOf(ctx, runRow, thread)
}

// LabelClearing records whether a commitment's message of the thread is done as
// of the cut-off, as the label of the clearing decision, replacing any earlier
// label, and returns the thread's review state in the run. It records nothing
// and returns ErrInvalidClearing for a clearing decision no judgment of the
// thread made, and ErrRunNotFound or ErrThreadNotFound for an unknown run or
// thread.
func (t *Threads) LabelClearing(ctx context.Context, run RunID, thread uuid.UUID, commitmentKey string, cutOff time.Time, done bool) (*ThreadReview, error) {
	runRow, err := t.store.runRow(ctx, run)
	if err != nil {
		return nil, err
	}
	if _, err := t.summary(ctx, thread); err != nil {
		return nil, err
	}
	made, err := t.store.queries.HasJudgmentClearing(ctx, db.HasJudgmentClearingParams{ThreadID: thread, CutOff: cutOff, CommitmentMessageKey: commitmentKey})
	if err != nil {
		return nil, fmt.Errorf("check clearing decision: %w", err)
	}
	if !made {
		return nil, ErrInvalidClearing
	}
	if err := t.store.inTx(ctx, func(tx pgx.Tx) error {
		return t.store.recordReviewTx(ctx, tx, clearingReview{thread: thread, commitmentKey: commitmentKey, cutOff: cutOff, done: done})
	}); err != nil {
		return nil, err
	}
	return t.store.reviewStateOf(ctx, runRow, thread)
}

// Close records the user finishing the thread's review in the run. For each
// night that holds a label, it labels as not holding every flag decision
// without a label of the night's judged messages, for all three kinds, but for
// one that a judgment of a compared method shows as a flag whichever run holds
// it, and labels the night's negative reply decisions without a label with
// their judged values. A night holding no label gains nothing, and a label that
// exists is never replaced. It records all of its labels or none, and returns
// the thread's review state in the run.
func (t *Threads) Close(ctx context.Context, run RunID, thread uuid.UUID) (*ThreadReview, error) {
	runRow, err := t.store.runRow(ctx, run)
	if err != nil {
		return nil, err
	}
	if _, err := t.summary(ctx, thread); err != nil {
		return nil, err
	}
	data, err := t.store.loadReviewData(ctx, runRow, []uuid.UUID{thread})
	if err != nil {
		return nil, err
	}
	built, err := data.build(ctx, thread)
	if err != nil {
		return nil, err
	}
	var nights []Night
	for _, n := range built.Nights {
		if n.HoldsLabel {
			nights = append(nights, n)
		}
	}
	if err := t.labelNights(ctx, thread, data, nights); err != nil {
		return nil, err
	}
	return t.store.reviewStateOf(ctx, runRow, thread)
}

// ConfirmNight labels a night that shows nothing and holds no label, named by
// its judgment: it labels as not holding every flag decision without a label of
// the night's judged messages, but for one a judgment of a compared method shows
// as a flag, and the night's negative reply decisions with their judged values.
// It records all of its labels or none, and returns the thread's review state in
// the run. It returns ErrNotFound, recording nothing, for a judgment that is no
// night of the run, ErrConfirmNonesRefused for a night that shows a chip, a
// clearing decision or a reply item or holds a label, and ErrRunNotFound for an
// unknown run.
func (t *Threads) ConfirmNight(ctx context.Context, run RunID, judgment uuid.UUID) (*ThreadReview, error) {
	runRow, err := t.store.runRow(ctx, run)
	if err != nil {
		return nil, err
	}
	rows, err := t.store.queries.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: []uuid.UUID{judgment}, ExampleIds: []string{}})
	if err != nil {
		return nil, fmt.Errorf("list judgments: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	thread := rows[0].ThreadID
	data, err := t.store.loadReviewData(ctx, runRow, []uuid.UUID{thread})
	if err != nil {
		return nil, err
	}
	built, err := data.build(ctx, thread)
	if err != nil {
		return nil, err
	}
	at := slices.IndexFunc(built.Nights, func(n Night) bool { return n.JudgmentID == judgment })
	if at < 0 {
		return nil, ErrNotFound
	}
	night := built.Nights[at]
	if night.shows || night.HoldsLabel {
		return nil, ErrConfirmNonesRefused
	}
	if err := t.labelNights(ctx, thread, data, []Night{night}); err != nil {
		return nil, err
	}
	return t.store.reviewStateOf(ctx, runRow, thread)
}

// labelNights records, in one transaction, the labels a close or Confirm Nones
// gives the nights: the flag decisions without a label of their judged messages
// as not holding, but for those a compared method's judgment shows as a flag,
// then their negative reply decisions with their judged values.
func (t *Threads) labelNights(ctx context.Context, thread uuid.UUID, data *reviewData, nights []Night) error {
	if len(nights) == 0 {
		return nil
	}
	shown, err := t.store.queries.ListShownFlagDecisions(ctx, db.ListShownFlagDecisionsParams{ThreadID: thread, MethodKeys: t.methods.Keys()})
	if err != nil {
		return fmt.Errorf("list shown flag decisions: %w", err)
	}
	skip := map[flagKey]bool{}
	for _, s := range shown {
		skip[flagKey{s.MessageKey, FlagKind(s.Kind)}] = true
	}
	for _, l := range data.decisionLb[thread].Flags {
		skip[flagKey{l.MessageKey, l.Kind}] = true
	}
	judgments := make([]uuid.UUID, 0, len(nights))
	return t.store.inTx(ctx, func(tx pgx.Tx) error {
		for _, n := range nights {
			judgments = append(judgments, n.JudgmentID)
			for _, key := range n.newKeys {
				for _, kind := range FlagKinds {
					if skip[flagKey{key, kind}] {
						continue
					}
					if err := t.store.recordReviewTx(ctx, tx, flagReview{thread: thread, messageKey: key, kind: kind, holds: false}); err != nil {
						return err
					}
					skip[flagKey{key, kind}] = true
				}
			}
		}
		_, err := t.store.labelNegativesTx(ctx, tx, judgments, false)
		return err
	})
}
