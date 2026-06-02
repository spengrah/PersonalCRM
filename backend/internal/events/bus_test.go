package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// mustMarshalContactMethodsAdded builds a valid contact_methods.added
// payload for tests that need a routable envelope.
func mustMarshalContactMethodsAdded(t *testing.T, contactID, jobID uuid.UUID) json.RawMessage {
	t.Helper()
	raw, err := Marshal(KindContactMethodsAdded, ContactMethodsAddedPayload{
		Version:      1,
		ContactID:    contactID,
		Methods:      []ContactMethodRef{{Type: "email", Value: "a@b.c"}},
		RematchJobID: jobID,
	})
	require.NoError(t, err)
	return raw
}

// TestConsumerJobsForKind_FiveAsyncKindsEnqueueInteractionRecorder asserts
// the PR 5 routing: async-publisher kinds enqueue exactly one
// InteractionRecorder river job each with MaxAttempts=5 (plan Decision 8).
func TestConsumerJobsForKind_FiveAsyncKindsEnqueueInteractionRecorder(t *testing.T) {
	asyncKinds := []Kind{
		KindMessageReceived, KindMessageSent, KindCalendarAttended,
		KindTaskCompleted, KindTaskOutreachDetected,
	}
	for _, k := range asyncKinds {
		t.Run(string(k), func(t *testing.T) {
			eventID := uuid.New()
			env := &Envelope{ID: eventID, Kind: k}
			jobs, err := consumerJobsForKind(env)
			require.NoError(t, err)
			require.Len(t, jobs, 1, "kind %s should enqueue exactly one consumer job", k)
			args, ok := jobs[0].Args.(consumerjobs.InteractionRecorderJobArgs)
			require.True(t, ok, "job args type must be InteractionRecorderJobArgs, got %T", jobs[0].Args)
			require.Equal(t, eventID, args.EventID)
			require.Equal(t, "interaction_recorder", args.Kind())
			require.NotNil(t, jobs[0].Opts, "InsertOpts must be set to pin MaxAttempts")
			require.Equal(t, 5, jobs[0].Opts.MaxAttempts)
		})
	}
}

// TestConsumerJobsForKind_InteractionRecordedEnqueuesCadenceAndFollowUp
// asserts that interaction.recorded enqueues BOTH the CadenceUpdater
// and FollowUpManager consumer jobs, each with MaxAttempts=5. Routing
// is config-blind — the mode gate runs inside each handler's
// HandleEvent, not at the routing layer.
func TestConsumerJobsForKind_InteractionRecordedEnqueuesCadenceAndFollowUp(t *testing.T) {
	eventID := uuid.New()
	env := &Envelope{ID: eventID, Kind: KindInteractionRecorded}
	jobs, err := consumerJobsForKind(env)
	require.NoError(t, err)
	require.Len(t, jobs, 2, "interaction.recorded should enqueue cadence + follow-up jobs")

	cadenceArgs, ok := jobs[0].Args.(consumerjobs.CadenceUpdaterJobArgs)
	require.True(t, ok, "job[0] args must be CadenceUpdaterJobArgs, got %T", jobs[0].Args)
	require.Equal(t, eventID, cadenceArgs.EventID)
	require.Equal(t, "cadence_updater", cadenceArgs.Kind())
	require.NotNil(t, jobs[0].Opts, "InsertOpts must be set to pin MaxAttempts")
	require.Equal(t, 5, jobs[0].Opts.MaxAttempts)

	followupArgs, ok := jobs[1].Args.(consumerjobs.FollowUpManagerJobArgs)
	require.True(t, ok, "job[1] args must be FollowUpManagerJobArgs, got %T", jobs[1].Args)
	require.Equal(t, eventID, followupArgs.EventID)
	require.Equal(t, "followup_manager", followupArgs.Kind())
	require.NotNil(t, jobs[1].Opts, "InsertOpts must be set to pin MaxAttempts")
	require.Equal(t, 5, jobs[1].Opts.MaxAttempts)
}

// TestConsumerJobsForKind_EmptyForDeferredKinds asserts the kinds that
// still have no active consumer return nil:
//   - interaction.manual: inline-invoked by the manual UI handler
//     (spec §3.4 "other consumers only").
//   - task.skipped: consumer for that kind is not yet wired.
//
// (contact_methods.added HAS a consumer now — see the dedicated
// TestConsumerJobsForKind_ContactMethodsAdded tests below.
// calendar.declined HAS a consumer now — see
// TestConsumerJobsForKind_CalendarDeclinedEnqueuesDeclineHandler.
// email.received / email.sent HAVE a consumer now — see
// TestConsumerJobsForKind_EmailEnqueuesEmailConsumer.)
func TestConsumerJobsForKind_EmptyForDeferredKinds(t *testing.T) {
	deferred := []Kind{
		KindInteractionManual,
		KindTaskSkipped,
	}
	for _, k := range deferred {
		t.Run(string(k), func(t *testing.T) {
			env := &Envelope{ID: uuid.New(), Kind: k}
			jobs, err := consumerJobsForKind(env)
			require.NoError(t, err)
			require.Empty(t, jobs, "kind %s: expected empty consumer-job slice", k)
		})
	}
}

// TestConsumerJobsForKind_CalendarDeclinedEnqueuesDeclineHandler asserts
// that calendar.declined enqueues exactly one CalendarDeclineHandler river
// job with MaxAttempts=5.
func TestConsumerJobsForKind_CalendarDeclinedEnqueuesDeclineHandler(t *testing.T) {
	eventID := uuid.New()
	env := &Envelope{ID: eventID, Kind: KindCalendarDeclined}
	jobs, err := consumerJobsForKind(env)
	require.NoError(t, err)
	require.Len(t, jobs, 1, "calendar.declined should enqueue exactly one consumer job")

	args, ok := jobs[0].Args.(consumerjobs.CalendarDeclineHandlerJobArgs)
	require.True(t, ok, "job args type must be CalendarDeclineHandlerJobArgs, got %T", jobs[0].Args)
	require.Equal(t, eventID, args.EventID)
	require.Equal(t, "calendar_decline_handler", args.Kind())
	require.NotNil(t, jobs[0].Opts, "InsertOpts must be set to pin MaxAttempts")
	require.Equal(t, 5, jobs[0].Opts.MaxAttempts)
}

// TestConsumerJobsForKind_EmailEnqueuesEmailConsumer asserts that each of
// email.received / email.sent enqueues exactly one EmailInteractionConsumer
// river job with MaxAttempts=5 and the event id threaded through.
func TestConsumerJobsForKind_EmailEnqueuesEmailConsumer(t *testing.T) {
	for _, k := range []Kind{KindEmailReceived, KindEmailSent} {
		t.Run(string(k), func(t *testing.T) {
			eventID := uuid.New()
			env := &Envelope{ID: eventID, Kind: k}
			jobs, err := consumerJobsForKind(env)
			require.NoError(t, err)
			require.Len(t, jobs, 1, "kind %s should enqueue exactly one consumer job", k)

			args, ok := jobs[0].Args.(consumerjobs.EmailInteractionConsumerJobArgs)
			require.True(t, ok, "job args type must be EmailInteractionConsumerJobArgs, got %T", jobs[0].Args)
			require.Equal(t, eventID, args.EventID)
			require.Equal(t, "email_interaction_consumer", args.Kind())
			require.NotNil(t, jobs[0].Opts, "InsertOpts must be set to pin MaxAttempts")
			require.Equal(t, 5, jobs[0].Opts.MaxAttempts)
		})
	}
}

// TestConsumerJobsForKind_ContactMethodsAdded asserts the PR 10 routing:
// contact_methods.added enqueues exactly one RematchDispatcher job with
// the payload-derived ContactID + RematchJobID and UniqueOpts set for
// belt-and-suspenders dedup behind the event.source_id unique index.
func TestConsumerJobsForKind_ContactMethodsAdded(t *testing.T) {
	eventID := uuid.New()
	contactID := uuid.New()
	jobID := uuid.New()
	env := &Envelope{
		ID:         eventID,
		Kind:       KindContactMethodsAdded,
		Source:     "manual",
		SourceID:   jobID.String(),
		Payload:    mustMarshalContactMethodsAdded(t, contactID, jobID),
		ObservedAt: accelerated.GetCurrentTime(),
	}
	jobs, err := consumerJobsForKind(env)
	require.NoError(t, err)
	require.Len(t, jobs, 1)

	args, ok := jobs[0].Args.(consumerjobs.RematchDispatcherJobArgs)
	require.True(t, ok, "job args must be RematchDispatcherJobArgs, got %T", jobs[0].Args)
	require.Equal(t, eventID, args.EventID)
	require.Equal(t, contactID, args.ContactID)
	require.Equal(t, jobID, args.RematchJobID)
	require.Equal(t, "rematch_dispatcher", args.Kind())

	require.NotNil(t, jobs[0].Opts)
	require.Equal(t, 3, jobs[0].Opts.MaxAttempts)
	require.True(t, jobs[0].Opts.UniqueOpts.ByArgs, "ByArgs must be true so UniqueOpts considers args; river:\"unique\" tags on ContactID+RematchJobID scope the hash")
	// ByState intentionally empty — river applies its default set
	// (Pending/Scheduled/Available/Running/Retryable) when ByState is
	// nil. A partial list must include Pending or river rejects the
	// InsertOpts.
	require.Empty(t, jobs[0].Opts.UniqueOpts.ByState)
}

// TestConsumerJobsForKind_ContactMethodsAdded_MalformedPayload_Errors
// confirms a non-JSON payload fails routing (Design Decision 3).
func TestConsumerJobsForKind_ContactMethodsAdded_MalformedPayload_Errors(t *testing.T) {
	env := &Envelope{
		ID:      uuid.New(),
		Kind:    KindContactMethodsAdded,
		Payload: json.RawMessage(`{not valid json`),
	}
	jobs, err := consumerJobsForKind(env)
	require.Error(t, err)
	require.Nil(t, jobs)
	require.Contains(t, err.Error(), "decode contact_methods.added")
}

// TestConsumerJobsForKind_ContactMethodsAdded_ZeroContactID_Errors
// rejects payloads with uuid.Nil ContactID. Downstream rematch handlers
// cannot do anything with a nil contact.
func TestConsumerJobsForKind_ContactMethodsAdded_ZeroContactID_Errors(t *testing.T) {
	payload, err := Marshal(KindContactMethodsAdded, ContactMethodsAddedPayload{
		Version:      1,
		ContactID:    uuid.Nil,
		Methods:      []ContactMethodRef{{Type: "email", Value: "x"}},
		RematchJobID: uuid.New(),
	})
	require.NoError(t, err)
	env := &Envelope{ID: uuid.New(), Kind: KindContactMethodsAdded, Payload: payload}
	jobs, err := consumerJobsForKind(env)
	require.Error(t, err)
	require.Nil(t, jobs)
	require.Contains(t, err.Error(), "empty contact_id")
}

// TestConsumerJobsForKind_ContactMethodsAdded_ZeroRematchJobID_Errors
// rejects payloads with uuid.Nil RematchJobID. UniqueOpts{ByArgs} would
// collapse every nil-jobID publish to a single River job — never what
// the publisher intends.
func TestConsumerJobsForKind_ContactMethodsAdded_ZeroRematchJobID_Errors(t *testing.T) {
	payload, err := Marshal(KindContactMethodsAdded, ContactMethodsAddedPayload{
		Version:      1,
		ContactID:    uuid.New(),
		Methods:      []ContactMethodRef{{Type: "email", Value: "x"}},
		RematchJobID: uuid.Nil,
	})
	require.NoError(t, err)
	env := &Envelope{ID: uuid.New(), Kind: KindContactMethodsAdded, Payload: payload}
	jobs, err := consumerJobsForKind(env)
	require.Error(t, err)
	require.Nil(t, jobs)
	require.Contains(t, err.Error(), "empty rematch_job_id")
}

// stubEventRepo is a test-only EventRepository that lets Bus_test files
// exercise envelope validation without touching the database or river.
type stubEventRepo struct {
	insertCalls  int
	insertErr    error
	lastInsertID uuid.UUID
}

func (s *stubEventRepo) InsertEvent(_ context.Context, _ pgx.Tx, env *Envelope) error {
	s.insertCalls++
	if env != nil {
		s.lastInsertID = env.ID
	}
	return s.insertErr
}

func (s *stubEventRepo) GetEvent(_ context.Context, _ uuid.UUID) (*Envelope, error) {
	return nil, db.ErrNotFound
}

func (s *stubEventRepo) FindEventBySource(_ context.Context, _, _ string) (*Envelope, error) {
	return nil, db.ErrNotFound
}

// nonNilStubTx returns a non-nil pgx.Tx for tests that only exercise
// validation paths (the stub repo never touches the tx). Implementing the
// full pgx.Tx interface for a mock is overkill — a typed-nil interface
// value is non-nil to the Bus's nil check while never being dereferenced
// because validation errors fire before InsertEvent runs.
func nonNilStubTx() pgx.Tx {
	var t *stubTxImpl
	return t
}

// stubTxImpl is a compile-time marker: it satisfies pgx.Tx by embedding a
// valid interface satisfier. The typed-nil receiver never has a method
// called on it in these tests (validation short-circuits before
// InsertEvent runs).
type stubTxImpl struct{ pgx.Tx }

// TestPublishTx_ValidatesEnvelope asserts that PublishTx rejects malformed
// envelopes BEFORE calling InsertEvent. Because validation fails first, the
// stub's InsertEvent is never called and riverClient stays nil-safe.
func TestPublishTx_ValidatesEnvelope(t *testing.T) {
	ctx := context.Background()
	validPayload := json.RawMessage(`{"version":1,"peer_ref":"tg:1:2","message_at":"2026-04-10T12:00:00Z"}`)
	validObservedAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		tx       pgx.Tx
		env      *Envelope
		wantSubs string
	}{
		{
			name:     "nil tx",
			tx:       nil,
			env:      nil, // irrelevant; tx check fires first
			wantSubs: "nil tx",
		},
		{
			name:     "nil envelope",
			tx:       nonNilStubTx(),
			env:      nil,
			wantSubs: "nil envelope",
		},
		{
			name: "empty kind",
			tx:   nonNilStubTx(),
			env: &Envelope{
				Source:     "telegram",
				Payload:    validPayload,
				ObservedAt: validObservedAt,
			},
			wantSubs: "empty kind",
		},
		{
			name: "unknown kind",
			tx:   nonNilStubTx(),
			env: &Envelope{
				Kind:       Kind("made.up"),
				Source:     "telegram",
				Payload:    validPayload,
				ObservedAt: validObservedAt,
			},
			wantSubs: "unknown kind",
		},
		{
			name: "empty source",
			tx:   nonNilStubTx(),
			env: &Envelope{
				Kind:       KindMessageReceived,
				Payload:    validPayload,
				ObservedAt: validObservedAt,
			},
			wantSubs: "empty source",
		},
		{
			name: "empty payload",
			tx:   nonNilStubTx(),
			env: &Envelope{
				Kind:       KindMessageReceived,
				Source:     "telegram",
				ObservedAt: validObservedAt,
			},
			wantSubs: "empty payload",
		},
		{
			name: "zero observed_at",
			tx:   nonNilStubTx(),
			env: &Envelope{
				Kind:    KindMessageReceived,
				Source:  "telegram",
				Payload: validPayload,
			},
			wantSubs: "empty observed_at",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubEventRepo{}
			// Bus with nil pool + nil riverClient is safe as long as
			// validation fails before we reach InsertEvent.
			bus := &Bus{eventRepo: stub}
			err := bus.PublishTx(ctx, tc.tx, tc.env)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantSubs)
			require.Zero(t, stub.insertCalls, "InsertEvent must not be called on validation failure")
		})
	}
}

// TestPublishTx_DuplicateIsNoOp asserts that a repo returning db.ErrDuplicate
// surfaces as a nil error from PublishTx (idempotent no-op). Since the stub's
// InsertEvent returns ErrDuplicate BEFORE the consumer-job loop runs, nil
// riverClient is still safe.
func TestPublishTx_DuplicateIsNoOp(t *testing.T) {
	ctx := context.Background()
	stub := &stubEventRepo{insertErr: db.ErrDuplicate}
	bus := &Bus{eventRepo: stub}

	env := &Envelope{
		Kind:       KindMessageReceived,
		Source:     "telegram",
		SourceID:   "tg:1:2:42",
		Payload:    json.RawMessage(`{"version":1}`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	err := bus.PublishTx(ctx, nonNilStubTx(), env)
	require.NoError(t, err)
	require.Equal(t, 1, stub.insertCalls)
}

// TestPublishTx_RepoErrorPropagates asserts that a non-ErrDuplicate repo
// error propagates wrapped to the caller.
func TestPublishTx_RepoErrorPropagates(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("db down")
	stub := &stubEventRepo{insertErr: sentinel}
	bus := &Bus{eventRepo: stub}

	env := &Envelope{
		Kind:       KindMessageReceived,
		Source:     "telegram",
		SourceID:   "tg:1:2:42",
		Payload:    json.RawMessage(`{"version":1}`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	err := bus.PublishTx(ctx, nonNilStubTx(), env)
	require.Error(t, err)
	require.ErrorIs(t, err, sentinel)
	require.Contains(t, err.Error(), "insert event")
}

// TestPublishTx_PreassignsEventID confirms the pre-insert env.ID
// assignment fires before routing so RematchDispatcherJobArgs.EventID
// (derived from env.ID at routing time) is never uuid.Nil.
//
// On ErrDuplicate with a bus-assigned ID, env.ID is intentionally
// reset to uuid.Nil so the IngestService duplicate sentinel
// (env.ID == uuid.Nil) works correctly. We verify preassignment by
// observing that the stub's insertCalls saw a non-zero ID at insert
// time — that's the window where routing has populated the consumer
// job args.
func TestPublishTx_PreassignsEventID(t *testing.T) {
	ctx := context.Background()
	// Stub returns ErrDuplicate after capturing the insert params so we
	// can verify env.ID was populated at insert time. The bus then
	// resets env.ID on the dup path.
	stub := &stubEventRepo{insertErr: db.ErrDuplicate}
	bus := &Bus{eventRepo: stub}

	contactID := uuid.New()
	jobID := uuid.New()
	payload, err := Marshal(KindContactMethodsAdded, ContactMethodsAddedPayload{
		Version:      1,
		ContactID:    contactID,
		Methods:      []ContactMethodRef{{Type: "email", Value: "a@b.c"}},
		RematchJobID: jobID,
	})
	require.NoError(t, err)

	env := &Envelope{
		Kind:       KindContactMethodsAdded,
		Source:     "manual",
		SourceID:   jobID.String(),
		Payload:    payload,
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, bus.PublishTx(ctx, nonNilStubTx(), env))
	require.Equal(t, 1, stub.insertCalls)
	require.NotEqual(t, uuid.Nil, stub.lastInsertID,
		"PublishTx must preassign env.ID before InsertEvent — the stub captures the insert-time ID")
	// Post-duplicate, env.ID is reset so IngestService's
	// `env.ID == uuid.Nil` duplicate sentinel works.
	require.Equal(t, uuid.Nil, env.ID,
		"PublishTx must reset env.ID to uuid.Nil on duplicate when it pre-assigned the ID")
}

// TestPublishTx_MalformedPayload_RollsBackTx confirms that a bad
// contact_methods.added payload fails the publish before event insert,
// so caller tx can roll back without a stranded event row. Regression
// guard for Design Decision 3 (Codex round-1 P2).
func TestPublishTx_MalformedPayload_RollsBackTx(t *testing.T) {
	ctx := context.Background()
	stub := &stubEventRepo{}
	bus := &Bus{eventRepo: stub}

	env := &Envelope{
		Kind:       KindContactMethodsAdded,
		Source:     "manual",
		SourceID:   "job-x",
		Payload:    json.RawMessage(`{definitely not json`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	err := bus.PublishTx(ctx, nonNilStubTx(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "route consumer jobs")
	require.Zero(t, stub.insertCalls, "InsertEvent must not run when routing fails")
}

// TestPublishTx_EnqueueRivers_NilClientPanicGuard is a NEGATIVE-space
// sanity: when routing yields no jobs (e.g. interaction.manual), nil
// riverClient is safe because the enqueue loop is skipped entirely.
// Existing TestPublishTx_DuplicateIsNoOp covers the post-duplicate case;
// this documents the same invariant for the routing-yields-empty path.
func TestPublishTx_NoJobsMeansNilRiverOK(t *testing.T) {
	ctx := context.Background()
	stub := &stubEventRepo{}
	bus := &Bus{eventRepo: stub}

	env := &Envelope{
		Kind:       KindInteractionManual,
		Source:     "manual",
		Payload:    json.RawMessage(`{"version":1,"contact_id":"00000000-0000-0000-0000-000000000001","direction":"outbound","occurred_at":"2026-04-10T12:00:00Z"}`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, bus.PublishTx(ctx, nonNilStubTx(), env))
	require.Equal(t, 1, stub.insertCalls)
}

var _ = river.UniqueOpts{}
