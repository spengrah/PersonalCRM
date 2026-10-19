package judgment

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
)

// closestFloor is the probability a message must exceed for a night to name it
// as the unflagged message its method came closest to flagging.
const closestFloor = 0.05

// ReviewRun is a run as the Threads tab names it.
type ReviewRun struct {
	ID        RunID
	Kind      RunKind
	Method    MethodRef
	From      *time.Time
	To        *time.Time
	StartedAt time.Time
}

func reviewRunOf(r *db.JudgmentRun) ReviewRun {
	return ReviewRun{ID: r.ID, Kind: RunKind(r.Kind), Method: runMethod(r), From: r.WindowFrom, To: r.WindowTo, StartedAt: r.StartedAt}
}

// FlagChip is a flag a message shows under a night's method. Probability is
// nil for an added chip, which the method did not show. ClearedAt is the
// cut-off of the first later night of the run whose clearing decision of this
// commitment answered done; nil for any other chip and for a commitment no
// later night judged done.
type FlagChip struct {
	Kind        FlagKind
	State       ChipState
	Probability *float64
	ClearedAt   *time.Time
}

// NightMessage is a message placed in a night. Judged is true when the night's
// judgment answered it; an unjudged message carries the reason the run
// recorded for it, if any.
type NightMessage struct {
	Key    string
	Judged bool
	Reason UnjudgedReason
	Chips  []FlagChip
}

// NightClearing is a clearing decision of a night: the method's answer to
// whether an earlier commitment is done as of the cut-off of the judgment that
// re-checked it (CutOff, the decision's identity), with the state of the
// user's label against that answer.
type NightClearing struct {
	CommitmentMessageKey string
	CommitmentSentAt     time.Time
	CutOff               time.Time
	Done                 bool
	Probability          float64
	Label                ClearingLabelState
}

// ClosestUnflagged is, for a flag kind, the judged message of a night showing
// no chip of the kind whose probability for it is highest and above
// closestFloor. It is display only. Message is nil when there is none.
type ClosestUnflagged struct {
	Kind        FlagKind
	Message     *ThreadMessage
	Probability float64
}

// Night is one judgment the run recorded or reused for a thread, at a cut-off,
// with the messages that cut-off placed, in thread order.
//
// NewMessageCount counts the messages new at the cut-off as T2 defines them,
// judged or unjudged. ToReview counts the chips, clearing decisions and reply
// items it shows without a label. ConfirmNones is true when it shows no chip,
// clearing decision or reply item and holds no label. HoldsLabel is true when a
// flag decision of one of its messages, one of its clearing decisions or a
// decision of its reply proposal has a label. LabeledNone counts the judged
// messages it shows no chip for that hold a label (each such label says the
// message holds nothing). Proposal is its judgment's reply proposal; nil when
// the read leaves it out.
type Night struct {
	JudgmentID      uuid.UUID
	Method          MethodRef
	CutOff          time.Time
	WindowStart     string
	NewMessageCount int
	Messages        []NightMessage
	Clearings       []NightClearing
	Closest         []ClosestUnflagged
	Proposal        *Proposal
	ToReview        int
	ConfirmNones    bool
	HoldsLabel      bool
	LabeledNone     int

	row       *db.ListJudgmentRowsRow
	newKeys   []string
	placed    []int
	whole     bool
	sorted    sorting
	shows     bool
	flagProbs map[string]map[FlagKind]float64
}

// ThreadReview is a thread's review state in a run: its nights in cut-off
// order, the messages read before the first night and after the last, and the
// status the server computes. ContextMessages are the messages the first
// night's window read before its new messages; they sit in no night. Later
// messages follow the run's last cut-off. Names holds the name of each contact
// the proposals and closest messages name.
type ThreadReview struct {
	Run             ReviewRun
	Thread          threadctx.VenueID
	Status          ThreadStatus
	ToReview        int
	JudgedCount     int
	Nights          []Night
	ContextMessages []string
	LaterMessages   []string
	Names           map[uuid.UUID]string
}

// reviewData is everything a run's reviews of some threads read, loaded for
// all of them at once.
type reviewData struct {
	run         *db.JudgmentRun
	appearances map[uuid.UUID][]*db.ListRunAppearancesForThreadsRow
	rows        map[uuid.UUID]*db.ListJudgmentRowsRow
	reads       map[uuid.UUID]recordedRead
	outcomes    map[uuid.UUID][]*outcomeRecord
	outcomeLb   labels
	unjudged    map[uuid.UUID][]*db.JudgmentUnjudged
	refs        map[uuid.UUID][]refMessage
	decisionLb  map[uuid.UUID]ThreadLabels
	thresholds  *thresholdCache
}

// refMessage is a message of a thread in thread order.
type refMessage struct {
	key    string
	sentAt time.Time
}

// thresholdCache reads each method's display threshold once.
type thresholdCache struct {
	s     *Store
	cache map[thresholdKey]float64
}

type thresholdKey struct {
	method string
	kind   FlagKind
}

func (c *thresholdCache) get(ctx context.Context, method string, kind FlagKind) (float64, error) {
	k := thresholdKey{method, kind}
	if v, ok := c.cache[k]; ok {
		return v, nil
	}
	v, err := c.s.DisplayThreshold(ctx, method, kind)
	if err != nil {
		return 0, err
	}
	c.cache[k] = v
	return v, nil
}

// loadReviewData reads what the run's reviews of the threads need.
func (s *Store) loadReviewData(ctx context.Context, run *db.JudgmentRun, threads []uuid.UUID) (*reviewData, error) {
	q := s.queries
	d := &reviewData{
		run: run, appearances: map[uuid.UUID][]*db.ListRunAppearancesForThreadsRow{}, rows: map[uuid.UUID]*db.ListJudgmentRowsRow{},
		unjudged: map[uuid.UUID][]*db.JudgmentUnjudged{}, thresholds: &thresholdCache{s: s, cache: map[thresholdKey]float64{}},
	}
	appearances, err := q.ListRunAppearancesForThreads(ctx, db.ListRunAppearancesForThreadsParams{RunID: run.ID, ThreadIds: threads})
	if err != nil {
		return nil, fmt.Errorf("list run appearances: %w", err)
	}
	var judgments []uuid.UUID
	for _, a := range appearances {
		d.appearances[a.ThreadID] = append(d.appearances[a.ThreadID], a)
		judgments = append(judgments, a.JudgmentID)
	}
	if len(judgments) > 0 {
		rows, err := q.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: judgments, ExampleIds: []string{}})
		if err != nil {
			return nil, fmt.Errorf("list run judgments: %w", err)
		}
		for _, r := range rows {
			d.rows[r.ID] = r
		}
		if d.reads, err = readsOf(ctx, q, rows); err != nil {
			return nil, err
		}
		if d.outcomes, d.outcomeLb, err = loadContent(ctx, q, rows); err != nil {
			return nil, err
		}
	}
	unjudged, err := q.ListRunUnjudged(ctx, db.ListRunUnjudgedParams{RunID: run.ID, ThreadIds: threads})
	if err != nil {
		return nil, fmt.Errorf("list run unjudged: %w", err)
	}
	for _, u := range unjudged {
		d.unjudged[u.ThreadID] = append(d.unjudged[u.ThreadID], u)
	}
	refs, err := q.ListThreadMessageKeys(ctx, threads)
	if err != nil {
		return nil, fmt.Errorf("list thread messages: %w", err)
	}
	d.refs = map[uuid.UUID][]refMessage{}
	for _, r := range refs {
		d.refs[r.VenueID] = append(d.refs[r.VenueID], refMessage{key: r.MessageKey, sentAt: r.SentAt})
	}
	if d.decisionLb, err = loadDecisionLabels(ctx, q, threads); err != nil {
		return nil, err
	}
	return d, nil
}

type flagKey struct {
	message string
	kind    FlagKind
}

type clearingKey struct {
	message string
	cutOff  int64
}

// build compiles the thread's review state in the run: its nights placed over
// its messages, each night's chips, clearing decisions, closest unflagged
// messages and status, and the thread's status. The reply proposals are left
// to the caller.
func (d *reviewData) build(ctx context.Context, thread uuid.UUID) (*ThreadReview, error) {
	refs := d.refs[thread]
	index := make(map[string]int, len(refs))
	for i, r := range refs {
		index[r.key] = i
	}
	flagLabels := map[flagKey]bool{}
	hasFlagLabel := map[string]bool{}
	clearingLabels := map[clearingKey]bool{}
	labels := d.decisionLb[thread]
	for _, l := range labels.Flags {
		flagLabels[flagKey{l.MessageKey, l.Kind}] = l.Holds
		hasFlagLabel[l.MessageKey] = true
	}
	for _, l := range labels.Clearings {
		clearingLabels[clearingKey{l.CommitmentMessageKey, l.CutOff.UnixNano()}] = l.Done
	}

	review := &ThreadReview{Run: reviewRunOf(d.run), Thread: thread, Nights: []Night{}, ContextMessages: []string{}, LaterMessages: []string{}}
	for _, a := range d.appearances[thread] {
		row := d.rows[a.JudgmentID]
		read := d.reads[a.JudgmentID]
		n := Night{
			JudgmentID: a.JudgmentID, Method: MethodRef{row.MethodKey, row.MethodVersion, row.WordingVersion},
			CutOff: a.CutOff, WindowStart: read.windowStart, row: row, whole: row.WindowStart == nil,
			newKeys: read.newMessages, flagProbs: map[string]map[FlagKind]float64{},
		}
		for _, f := range read.flags {
			if n.flagProbs[f.MessageKey] == nil {
				n.flagProbs[f.MessageKey] = map[FlagKind]float64{}
			}
			n.flagProbs[f.MessageKey][f.Kind] = f.Probability
		}
		review.Nights = append(review.Nights, n)
	}
	slices.SortStableFunc(review.Nights, func(a, b Night) int {
		return cmp.Or(a.CutOff.Compare(b.CutOff), strings.Compare(a.JudgmentID.String(), b.JudgmentID.String()))
	})
	nights := review.Nights

	d.place(review, refs, index)
	reasons, unjudgedAt := d.unjudgedOf(thread)

	judged := map[string]bool{}
	for i := range nights {
		n := &nights[i]
		read := d.reads[n.JudgmentID]
		isNew := make(map[string]bool, len(n.newKeys))
		for _, k := range n.newKeys {
			isNew[k] = true
			judged[k] = true
		}
		n.NewMessageCount = len(n.newKeys) + unjudgedAt[n.CutOff.UnixNano()]
		if n.whole {
			n.NewMessageCount = len(n.placed)
		}
		n.Messages = make([]NightMessage, 0, len(n.placed))
		for _, ri := range n.placed {
			key := refs[ri].key
			m := NightMessage{Key: key, Judged: isNew[key], Chips: []FlagChip{}}
			if !m.Judged {
				m.Reason = reasons[key]
			}
			for _, kind := range FlagKinds {
				chip, ok, err := d.chip(ctx, n, key, kind, m.Judged, flagLabels)
				if err != nil {
					return nil, err
				}
				if ok {
					m.Chips = append(m.Chips, chip)
				}
			}
			n.Messages = append(n.Messages, m)
		}
		// A commitment chip names the first later night whose clearing
		// decision of it answered done.
		for mi := range n.Messages {
			for ci := range n.Messages[mi].Chips {
				chip := &n.Messages[mi].Chips[ci]
				if chip.Kind != FlagKindCommitment {
					continue
				}
				for j := i + 1; j < len(nights) && chip.ClearedAt == nil; j++ {
					for _, c := range d.reads[nights[j].JudgmentID].clearings {
						if c.CommitmentMessageKey == n.Messages[mi].Key && c.Done {
							at := nights[j].CutOff
							chip.ClearedAt = &at
							break
						}
					}
				}
			}
		}
		n.Clearings = make([]NightClearing, len(read.clearings))
		for ci, c := range read.clearings {
			nc := NightClearing{
				CommitmentMessageKey: c.CommitmentMessageKey, CutOff: n.row.CutOff, Done: c.Done, Probability: c.Probability,
				Label: ClearingLabelStateNone,
			}
			if at, ok := index[c.CommitmentMessageKey]; ok {
				nc.CommitmentSentAt = refs[at].sentAt
			}
			if done, labeled := clearingLabels[clearingKey{c.CommitmentMessageKey, n.row.CutOff.UnixNano()}]; labeled {
				nc.Label = ClearingLabelStateRemoved
				if done == c.Done {
					nc.Label = ClearingLabelStateConfirmed
				}
			}
			n.Clearings[ci] = nc
		}
		// Clearing decisions read in thread order of their commitments' messages.
		slices.SortStableFunc(n.Clearings, func(a, b NightClearing) int {
			return cmp.Or(a.CommitmentSentAt.Compare(b.CommitmentSentAt), strings.Compare(a.CommitmentMessageKey, b.CommitmentMessageKey))
		})
		n.Closest = d.closest(n, refs, index)
		n.status(d, hasFlagLabel, refs)
	}
	review.JudgedCount = len(judged)
	review.Status, review.ToReview = threadStatus(nights)
	return review, nil
}

// chip is the chip a message shows for a kind under the night's method.
func (d *reviewData) chip(ctx context.Context, n *Night, key string, kind FlagKind, judged bool, labels map[flagKey]bool) (FlagChip, bool, error) {
	holds, labeled := labels[flagKey{key, kind}]
	if p, ok := n.flagProbs[key][kind]; judged && ok {
		threshold, err := d.thresholds.get(ctx, n.Method.Key, kind)
		if err != nil {
			return FlagChip{}, false, err
		}
		if p >= threshold {
			state := ChipStateToReview
			switch {
			case labeled && holds:
				state = ChipStateConfirmed
			case labeled:
				state = ChipStateRemoved
			}
			return FlagChip{Kind: kind, State: state, Probability: &p}, true, nil
		}
	}
	if labeled && holds {
		return FlagChip{Kind: kind, State: ChipStateAdded}, true, nil
	}
	return FlagChip{}, false, nil
}

// place puts the thread's messages in the review's nights: every message from
// the first night's window start to the run's last cut-off sits once, in the
// first night whose cut-off is at or after its time. The messages the first
// night's window read before its new messages, when its judgment recorded any,
// sit in no night, and the messages after the last cut-off are later.
func (d *reviewData) place(review *ThreadReview, refs []refMessage, index map[string]int) {
	nights := review.Nights
	if len(nights) == 0 {
		return
	}
	first := nights[0]
	start := 0
	if at, ok := index[first.WindowStart]; ok {
		start = at
	}
	if len(first.newKeys) > 0 {
		earliest := -1
		for _, k := range first.newKeys {
			if at, ok := index[k]; ok && (earliest < 0 || at < earliest) {
				earliest = at
			}
		}
		if earliest > start {
			for _, r := range refs[start:earliest] {
				review.ContextMessages = append(review.ContextMessages, r.key)
			}
			start = earliest
		}
	}
	for ri := start; ri < len(refs); ri++ {
		at := slices.IndexFunc(nights, func(n Night) bool { return !refs[ri].sentAt.After(n.CutOff) })
		if at < 0 {
			review.LaterMessages = append(review.LaterMessages, refs[ri].key)
			continue
		}
		nights[at].placed = append(nights[at].placed, ri)
	}
}

// unjudgedOf reads the run's unjudged messages of the thread: the reason of
// each message, and how many were recorded at each cut-off.
func (d *reviewData) unjudgedOf(thread uuid.UUID) (map[string]UnjudgedReason, map[int64]int) {
	reasons := map[string]UnjudgedReason{}
	at := map[int64]int{}
	for _, u := range d.unjudged[thread] {
		reasons[u.MessageKey] = UnjudgedReason(u.Reason)
		at[u.CutOff.UnixNano()]++
	}
	return reasons, at
}

// closest names, per flag kind, the judged message of the night that shows no
// chip of the kind and has the highest probability for it above closestFloor;
// the earliest message wins a tie.
func (d *reviewData) closest(n *Night, refs []refMessage, index map[string]int) []ClosestUnflagged {
	out := make([]ClosestUnflagged, len(FlagKinds))
	for ki, kind := range FlagKinds {
		out[ki] = ClosestUnflagged{Kind: kind}
		for _, m := range n.Messages {
			p, ok := n.flagProbs[m.Key][kind]
			if !m.Judged || !ok || p <= closestFloor || (out[ki].Message != nil && p <= out[ki].Probability) {
				continue
			}
			if slices.ContainsFunc(m.Chips, func(c FlagChip) bool { return c.Kind == kind }) {
				continue
			}
			ref := refs[index[m.Key]]
			out[ki].Message = &ThreadMessage{Key: m.Key, SentAt: ref.sentAt}
			out[ki].Probability = p
		}
	}
	return out
}

// status computes what the night shows and what of it awaits a label.
func (n *Night) status(d *reviewData, hasFlagLabel map[string]bool, refs []refMessage) {
	n.sorted = sortOutcomes(d.outcomes[n.JudgmentID], d.outcomeLb)
	chips, unlabeled, labeledNone := 0, 0, 0
	for _, m := range n.Messages {
		for _, c := range m.Chips {
			chips++
			if c.State == ChipStateToReview {
				unlabeled++
			}
		}
		if m.Judged && len(m.Chips) == 0 && hasFlagLabel[m.Key] {
			labeledNone++
		}
		if hasFlagLabel[m.Key] {
			n.HoldsLabel = true
		}
	}
	for _, c := range n.Clearings {
		if c.Label == ClearingLabelStateNone {
			unlabeled++
		} else {
			n.HoldsLabel = true
		}
	}
	for _, pl := range n.sorted.items {
		if pl.label == nil {
			unlabeled++
		}
	}
	n.HoldsLabel = n.HoldsLabel || n.sorted.holdsLabel(d.outcomeLb)
	n.shows = chips > 0 || len(n.Clearings) > 0 || len(n.sorted.items) > 0
	n.ToReview = unlabeled
	n.ConfirmNones = !n.shows && !n.HoldsLabel
	n.LabeledNone = labeledNone
}

// threadStatus is a thread's status over its nights: reviewed when every night
// holds a label; otherwise to review, counting the items shown without a label
// and the nights that offer Confirm Nones, when any night shows an item;
// otherwise nothing flagged.
func threadStatus(nights []Night) (ThreadStatus, int) {
	reviewed, shows, toReview := len(nights) > 0, false, 0
	for _, n := range nights {
		reviewed = reviewed && n.HoldsLabel
		shows = shows || n.shows
		toReview += n.ToReview
		if n.ConfirmNones {
			toReview++
		}
	}
	switch {
	case reviewed:
		return ThreadStatusReviewed, 0
	case shows:
		return ThreadStatusToReview, toReview
	default:
		return ThreadStatusNothingFlagged, 0
	}
}
