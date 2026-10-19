package judgment

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// recordedRead is what a judgment read and answered about messages, as the
// reads return it.
type recordedRead struct {
	windowStart string
	newMessages []string
	flags       []FlagDecision
	clearings   []ClearingDecision
}

// readsOf returns what each given judgment read and answered about messages. A
// judgment of a version before migration 099 read its whole context, so its
// window starts at the context's first message and it records no decision.
func readsOf(ctx context.Context, q db.Querier, rows []*db.ListJudgmentRowsRow) (map[uuid.UUID]recordedRead, error) {
	reads := make(map[uuid.UUID]recordedRead, len(rows))
	if len(rows) == 0 {
		return reads, nil
	}
	ids, whole := make([]uuid.UUID, len(rows)), []uuid.UUID{}
	for i, j := range rows {
		ids[i] = j.ID
		if j.WindowStart == nil {
			whole = append(whole, j.ID)
		}
	}
	firsts, err := q.ListJudgmentFirstMessages(ctx, whole)
	if err != nil {
		return nil, fmt.Errorf("list first messages: %w", err)
	}
	first := make(map[uuid.UUID]string, len(firsts))
	for _, f := range firsts {
		first[f.JudgmentID] = f.MessageKey
	}
	flagRows, err := q.ListJudgmentFlags(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list flag decisions: %w", err)
	}
	clearingRows, err := q.ListJudgmentClearings(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list clearing decisions: %w", err)
	}
	for _, j := range rows {
		r := recordedRead{windowStart: first[j.ID], newMessages: nonNil(j.NewMessages)}
		if j.WindowStart != nil {
			r.windowStart = *j.WindowStart
		}
		reads[j.ID] = r
	}
	for _, f := range flagRows {
		r := reads[f.JudgmentID]
		r.flags = append(r.flags, FlagDecision{MessageKey: f.MessageKey, Kind: FlagKind(f.Kind), Probability: f.Probability})
		reads[f.JudgmentID] = r
	}
	for _, c := range clearingRows {
		r := reads[c.JudgmentID]
		r.clearings = append(r.clearings, ClearingDecision{
			CommitmentMessageKey: c.CommitmentMessageKey, CommitmentJudgment: c.CommitmentJudgmentID,
			Done: c.Done, Probability: c.Probability,
		})
		reads[c.JudgmentID] = r
	}
	for id, r := range reads {
		// Flags read in thread order of the new messages, each message's kinds in
		// FlagKinds order.
		position := make(map[string]int, len(r.newMessages))
		for i, key := range r.newMessages {
			position[key] = i
		}
		slices.SortStableFunc(r.flags, func(a, b FlagDecision) int {
			return cmp.Or(
				cmp.Compare(position[a.MessageKey], position[b.MessageKey]),
				cmp.Compare(slices.Index(FlagKinds, a.Kind), slices.Index(FlagKinds, b.Kind)),
			)
		})
		r.flags, r.clearings = nonNil(r.flags), nonNil(r.clearings)
		reads[id] = r
	}
	return reads, nil
}

// DisplayThreshold returns the probability at which a flag decision of the kind
// shows for the method: the one stored threshold every reader reads. It
// returns ErrNoFlagSettings for a method with none.
func (s *Store) DisplayThreshold(ctx context.Context, methodKey string, kind FlagKind) (float64, error) {
	threshold, err := s.queries.GetFlagDisplayThreshold(ctx, db.GetFlagDisplayThresholdParams{MethodKey: methodKey, Kind: string(kind)})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("display threshold of %s for %s: %w", kind, methodKey, ErrNoFlagSettings)
	}
	if err != nil {
		return 0, fmt.Errorf("get display threshold: %w", err)
	}
	return threshold, nil
}

// ClearingFloor returns the probability a method's commitment flag decision
// must exceed to be re-checked at later cut-offs: the one stored floor every
// reader reads. It returns ErrNoFlagSettings for a method with none.
func (s *Store) ClearingFloor(ctx context.Context, methodKey string) (float64, error) {
	floor, err := s.queries.GetFlagClearingFloor(ctx, methodKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("clearing floor for %s: %w", methodKey, ErrNoFlagSettings)
	}
	if err != nil {
		return 0, fmt.Errorf("get clearing floor: %w", err)
	}
	return floor, nil
}

// EarlierJudgment is a judgment of a method version's earlier cut-off, with the
// decisions a later cut-off re-checks and clears.
type EarlierJudgment struct {
	ID          uuid.UUID
	CutOff      time.Time
	WindowStart string
	NewMessages []string
	Flags       []FlagDecision
	Clearings   []ClearingDecision
}

// EarlierJudgments returns the method version's judgments of the thread whose
// cut-off is before cutOff, oldest first. A judgment of a version before
// migration 099 records no decision.
func (s *Store) EarlierJudgments(ctx context.Context, method MethodRef, thread threadctx.VenueID, cutOff time.Time) ([]EarlierJudgment, error) {
	rows, err := s.queries.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{
		Ids: []uuid.UUID{}, ThreadID: &thread, ExampleIds: []string{},
		MethodKey: &method.Key, MethodVersion: &method.Version, WordingVersion: &method.WordingVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("list earlier judgments: %w", err)
	}
	rows = slices.DeleteFunc(rows, func(j *db.ListJudgmentRowsRow) bool { return !j.CutOff.Before(cutOff) })
	reads, err := readsOf(ctx, s.queries, rows)
	if err != nil {
		return nil, err
	}
	earlier := make([]EarlierJudgment, len(rows))
	for i, j := range rows {
		r := reads[j.ID]
		earlier[i] = EarlierJudgment{ID: j.ID, CutOff: j.CutOff, WindowStart: r.windowStart, NewMessages: r.newMessages, Flags: r.flags, Clearings: r.clearings}
	}
	return earlier, nil
}
