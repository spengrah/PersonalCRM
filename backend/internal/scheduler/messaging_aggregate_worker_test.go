package scheduler

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/consumer/consumerjobs"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// stubAggregator records every AggregateForContact call. Tests assert
// on the recorded calls to verify per-chat invocation ordering and
// counts.
type stubAggregator struct {
	calls   []stubAggCall
	failOn  string // if non-empty, return error for this chatID
	failErr error
}

type stubAggCall struct {
	contactID uuid.UUID
	chatID    string
}

func (s *stubAggregator) AggregateForContact(_ context.Context, contactID uuid.UUID, chatID string) error {
	s.calls = append(s.calls, stubAggCall{contactID, chatID})
	if s.failOn != "" && s.failOn == chatID {
		return s.failErr
	}
	return nil
}

// stubChatLister returns canned chat lists per call. The lists slice
// is consumed left-to-right; once exhausted ListUnprocessedChats
// returns the last value (typically an empty slice) on subsequent
// calls. errAfter > 0 causes the (errAfter+1)-th call to error.
type stubChatLister struct {
	source    string
	lists     [][]string
	calls     int
	errAfter  int
	errReturn error
}

func (s *stubChatLister) ListUnprocessedChats(_ context.Context, source string, _ uuid.UUID) ([]string, error) {
	s.calls++
	if s.errAfter > 0 && s.calls > s.errAfter {
		return nil, s.errReturn
	}
	if source != s.source {
		return nil, nil
	}
	if len(s.lists) == 0 {
		return nil, nil
	}
	if s.calls > len(s.lists) {
		return s.lists[len(s.lists)-1], nil
	}
	return s.lists[s.calls-1], nil
}

func TestMessagingAggregateWorker_HappyPath_DrainsAndCallsPerChat(t *testing.T) {
	agg := &stubAggregator{}
	lister := &stubChatLister{
		source: "messages",
		lists: [][]string{
			{"chat-A", "chat-B"},
			{}, // second list-call drains
		},
	}
	worker := NewMessagingAggregateForContactWorker(
		map[string]ChatAwareAggregator{"messages": agg},
		lister,
	)

	contactID := uuid.New()
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateForContactArgs]{
		Args: consumerjobs.MessagingAggregateForContactArgs{ContactID: contactID, Source: "messages"},
	})
	require.NoError(t, err)
	require.Len(t, agg.calls, 2)
	require.Equal(t, "chat-A", agg.calls[0].chatID)
	require.Equal(t, "chat-B", agg.calls[1].chatID)
	require.Equal(t, contactID, agg.calls[0].contactID)
}

// TestMessagingAggregateWorker_RelistLoopDrainsLandedRows asserts the
// worker re-lists after every iteration: chats that land during an
// engine call are picked up before the worker returns.
func TestMessagingAggregateWorker_RelistLoopDrainsLandedRows(t *testing.T) {
	agg := &stubAggregator{}
	lister := &stubChatLister{
		source: "messages",
		lists: [][]string{
			{"chat-A"},
			{"chat-B"}, // landed during chat-A processing
			{},         // drained
		},
	}
	worker := NewMessagingAggregateForContactWorker(
		map[string]ChatAwareAggregator{"messages": agg},
		lister,
	)
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateForContactArgs]{
		Args: consumerjobs.MessagingAggregateForContactArgs{ContactID: uuid.New(), Source: "messages"},
	})
	require.NoError(t, err)
	require.Len(t, agg.calls, 2)
	require.Equal(t, "chat-A", agg.calls[0].chatID)
	require.Equal(t, "chat-B", agg.calls[1].chatID)
}

// TestMessagingAggregateWorker_UnknownSource_NoOp confirms the worker
// returns nil and does NOT call the engine when the args carry an
// unregistered source.
func TestMessagingAggregateWorker_UnknownSource_NoOp(t *testing.T) {
	agg := &stubAggregator{}
	lister := &stubChatLister{source: "messages", lists: [][]string{{"chat-A"}, {}}}
	worker := NewMessagingAggregateForContactWorker(
		map[string]ChatAwareAggregator{"messages": agg},
		lister,
	)
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateForContactArgs]{
		Args: consumerjobs.MessagingAggregateForContactArgs{ContactID: uuid.New(), Source: "whatsapp"},
	})
	require.NoError(t, err)
	require.Empty(t, agg.calls)
}

// TestMessagingAggregateWorker_EngineError_Propagates verifies an
// engine failure propagates through the worker so River's retry
// machinery kicks in.
func TestMessagingAggregateWorker_EngineError_Propagates(t *testing.T) {
	agg := &stubAggregator{failOn: "chat-A", failErr: errors.New("boom")}
	lister := &stubChatLister{source: "messages", lists: [][]string{{"chat-A"}, {}}}
	worker := NewMessagingAggregateForContactWorker(
		map[string]ChatAwareAggregator{"messages": agg},
		lister,
	)
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateForContactArgs]{
		Args: consumerjobs.MessagingAggregateForContactArgs{ContactID: uuid.New(), Source: "messages"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

// TestMessagingAggregateWorker_ListerError_Propagates verifies a
// lister failure surfaces. Lister errors during the first iteration
// fail the job; an error after some chats were already processed
// also fails the run so River retries the whole pass.
func TestMessagingAggregateWorker_ListerError_Propagates(t *testing.T) {
	agg := &stubAggregator{}
	lister := &stubChatLister{
		source:    "messages",
		lists:     [][]string{{"chat-A"}, {"chat-B"}},
		errAfter:  1,
		errReturn: errors.New("db down"),
	}
	worker := NewMessagingAggregateForContactWorker(
		map[string]ChatAwareAggregator{"messages": agg},
		lister,
	)
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateForContactArgs]{
		Args: consumerjobs.MessagingAggregateForContactArgs{ContactID: uuid.New(), Source: "messages"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "list unprocessed chats")
}

// TestMessagingAggregateWorker_LoopBound_TerminatesAndLogs asserts the
// worker exits cleanly (returns nil) after workerLoopMaxIterations
// even if the lister keeps returning non-empty results.
func TestMessagingAggregateWorker_LoopBound_TerminatesAndLogs(t *testing.T) {
	agg := &stubAggregator{}
	// Build a lists slice that returns a single chat ad infinitum
	// (stubChatLister returns the last list once exhausted).
	lists := make([][]string, 0, workerLoopMaxIterations+1)
	for i := 0; i < workerLoopMaxIterations+1; i++ {
		lists = append(lists, []string{"chat-A"})
	}
	lister := &stubChatLister{source: "messages", lists: lists}
	worker := NewMessagingAggregateForContactWorker(
		map[string]ChatAwareAggregator{"messages": agg},
		lister,
	)
	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateForContactArgs]{
		Args: consumerjobs.MessagingAggregateForContactArgs{ContactID: uuid.New(), Source: "messages"},
	})
	require.NoError(t, err)
	require.Equal(t, workerLoopMaxIterations, len(agg.calls),
		"worker should have called engine exactly workerLoopMaxIterations times before bailing")
}

// TestPerSourceChatListerRegistry_DispatchesAndNoOpsUnknown verifies
// the registry behavior at the unit level.
func TestPerSourceChatListerRegistry_DispatchesAndNoOpsUnknown(t *testing.T) {
	called := 0
	reg := NewPerSourceChatListerRegistry(map[string]func(ctx context.Context, contactID uuid.UUID) ([]string, error){
		"messages": func(_ context.Context, _ uuid.UUID) ([]string, error) {
			called++
			return []string{"chat-A"}, nil
		},
	})

	got, err := reg.ListUnprocessedChats(context.Background(), "messages", uuid.New())
	require.NoError(t, err)
	require.Equal(t, []string{"chat-A"}, got)
	require.Equal(t, 1, called)

	got, err = reg.ListUnprocessedChats(context.Background(), "unknown", uuid.New())
	require.NoError(t, err)
	require.Nil(t, got)
	require.Equal(t, 1, called, "unknown source should not call any entry")
}
