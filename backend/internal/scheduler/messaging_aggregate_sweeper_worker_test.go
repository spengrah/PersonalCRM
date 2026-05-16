package scheduler

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/consumer/consumerjobs"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

type stubContactLister struct {
	ids    []uuid.UUID
	err    error
	called int
}

func (s *stubContactLister) ListUnprocessedContactIDs(_ context.Context) ([]uuid.UUID, error) {
	s.called++
	if s.err != nil {
		return nil, s.err
	}
	return s.ids, nil
}

type stubInserter struct {
	calls        []consumerjobs.MessagingAggregateForContactArgs
	duplicateIDs map[uuid.UUID]bool
	failOnce     bool
	failed       bool
}

func (s *stubInserter) Insert(_ context.Context, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	a, ok := args.(consumerjobs.MessagingAggregateForContactArgs)
	if !ok {
		return nil, errors.New("unexpected args type")
	}
	if s.failOnce && !s.failed {
		s.failed = true
		return nil, errors.New("transient enqueue failure")
	}
	s.calls = append(s.calls, a)
	return &rivertype.JobInsertResult{UniqueSkippedAsDuplicate: s.duplicateIDs[a.ContactID]}, nil
}

func TestSweeperWorker_EnqueuesPerContact(t *testing.T) {
	c1, c2, c3 := uuid.New(), uuid.New(), uuid.New()
	lister := &stubContactLister{ids: []uuid.UUID{c1, c2, c3}}
	inserter := &stubInserter{}

	worker := NewMessagingAggregateSweeperWorker(
		map[string]UnprocessedContactLister{"messages": lister},
		inserter,
	)
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateSweeperArgs]{})
	require.NoError(t, err)
	require.Equal(t, 1, lister.called)
	require.Len(t, inserter.calls, 3)
	gotIDs := []uuid.UUID{inserter.calls[0].ContactID, inserter.calls[1].ContactID, inserter.calls[2].ContactID}
	require.ElementsMatch(t, []uuid.UUID{c1, c2, c3}, gotIDs)
	for _, c := range inserter.calls {
		require.Equal(t, "messages", c.Source)
	}
}

// TestSweeperWorker_EmptyList_NoEnqueues asserts the sweeper does
// nothing when there are no unprocessed rows.
func TestSweeperWorker_EmptyList_NoEnqueues(t *testing.T) {
	lister := &stubContactLister{ids: nil}
	inserter := &stubInserter{}

	worker := NewMessagingAggregateSweeperWorker(
		map[string]UnprocessedContactLister{"messages": lister},
		inserter,
	)
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateSweeperArgs]{})
	require.NoError(t, err)
	require.Empty(t, inserter.calls)
}

// TestSweeperWorker_ListError_LogsAndContinues asserts a list-error
// in one source does NOT fail the whole tick; the worker keeps
// running and returns nil.
func TestSweeperWorker_ListError_LogsAndContinues(t *testing.T) {
	listerErr := &stubContactLister{err: errors.New("db down")}
	listerOK := &stubContactLister{ids: []uuid.UUID{uuid.New()}}
	inserter := &stubInserter{}

	worker := NewMessagingAggregateSweeperWorker(
		map[string]UnprocessedContactLister{
			"messages": listerErr,
			"whatsapp": listerOK,
		},
		inserter,
	)
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateSweeperArgs]{})
	require.NoError(t, err)
	// The healthy source's contact still gets enqueued.
	require.Len(t, inserter.calls, 1)
}

// TestSweeperWorker_EnqueueError_ContinuesToNextContact verifies a
// transient enqueue failure on one contact does NOT stop the worker
// from enqueuing the rest.
func TestSweeperWorker_EnqueueError_ContinuesToNextContact(t *testing.T) {
	c1, c2 := uuid.New(), uuid.New()
	lister := &stubContactLister{ids: []uuid.UUID{c1, c2}}
	inserter := &stubInserter{failOnce: true}

	worker := NewMessagingAggregateSweeperWorker(
		map[string]UnprocessedContactLister{"messages": lister},
		inserter,
	)
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateSweeperArgs]{})
	require.NoError(t, err)
	require.Len(t, inserter.calls, 1)
	require.True(t, inserter.failed)
}

// TestSweeperWorker_DuplicateSkipped_ContinuesToNextContact verifies a
// River duplicate skip is treated as a successful insert attempt rather than
// an error that stops the sweep.
func TestSweeperWorker_DuplicateSkipped_ContinuesToNextContact(t *testing.T) {
	c1, c2 := uuid.New(), uuid.New()
	lister := &stubContactLister{ids: []uuid.UUID{c1, c2}}
	inserter := &stubInserter{duplicateIDs: map[uuid.UUID]bool{c1: true}}

	worker := NewMessagingAggregateSweeperWorker(
		map[string]UnprocessedContactLister{"messages": lister},
		inserter,
	)
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateSweeperArgs]{})
	require.NoError(t, err)
	require.Len(t, inserter.calls, 2)
}

// TestSweeperWorker_NilRiverClient_NoOps verifies a nil client makes
// the worker behave as a list-only sweep (used in light unit tests).
func TestSweeperWorker_NilRiverClient_NoOps(t *testing.T) {
	lister := &stubContactLister{ids: []uuid.UUID{uuid.New()}}
	worker := NewMessagingAggregateSweeperWorker(
		map[string]UnprocessedContactLister{"messages": lister},
		nil,
	)
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateSweeperArgs]{})
	require.NoError(t, err)
	require.Equal(t, 1, lister.called)
}
