package consumer

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestAwaitingReplyWindow_BuildInteractionWrite(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeCutover)
	occurredAt := time.Date(2026, 9, 15, 13, 30, 0, 0, time.Local)
	wantUntil := cadence.AwaitingReplyUntil(occurredAt, 7)

	for _, tc := range []struct {
		name      string
		direction string
		source    string
		cadence   string
		wantApply bool
		wantUntil *time.Time
	}{
		{name: "outbound forward", direction: repository.InteractionDirectionOutbound, source: repository.InteractionSourceTelegram, cadence: "monthly", wantApply: true, wantUntil: &wantUntil},
		{name: "outbound manual", direction: repository.InteractionDirectionOutbound, source: repository.InteractionSourceManual, cadence: "monthly", wantApply: true, wantUntil: &wantUntil},
		{name: "inbound", direction: repository.InteractionDirectionInbound, source: repository.InteractionSourceTelegram, cadence: "monthly"},
		{name: "mutual", direction: repository.InteractionDirectionMutual, source: repository.InteractionSourceTelegram, cadence: "monthly"},
		{name: "outbound without cadence", direction: repository.InteractionDirectionOutbound, source: repository.InteractionSourceTelegram},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := h.buildInteractionWrite(uuid.New(), tc.direction, tc.source, occurredAt, repository.ContactCadenceFields{}, tc.cadence)
			require.Equal(t, tc.wantApply, req.ApplyAwaitingReplyUntil)
			if tc.wantUntil == nil {
				require.Nil(t, req.AwaitingReplyUntil)
				return
			}
			require.NotNil(t, req.AwaitingReplyUntil)
			require.Equal(t, cadence.CalendarDate(*tc.wantUntil), cadence.CalendarDate(*req.AwaitingReplyUntil))
		})
	}
}

func TestAwaitingReplyWindow_ApplyTxNoOpWithoutExpiry(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeCutover)
	require.NoError(t, h.applyTx(context.Background(), nil, cadenceWriteRequest{
		Branch: repository.CadenceBranchForward,
	}))
}

type awaitingReplyWindowCreateWriter struct {
	*stubFollowUpTaskWriter
}

func (*awaitingReplyWindowCreateWriter) CreateContactTaskTx(_ context.Context, _ pgx.Tx, req repository.CreateContactTaskRequest, _ *string) (*repository.ContactTask, error) {
	return &repository.ContactTask{
		ID:        uuid.New(),
		ContactID: req.ContactID,
		Kind:      req.Kind,
		Lifecycle: req.Lifecycle,
		State:     repository.ContactTaskStatePendingRemoteCreate,
		Metadata:  req.Metadata,
	}, nil
}

type awaitingReplyWindowTx struct{ pgx.Tx }

func (*awaitingReplyWindowTx) Begin(context.Context) (pgx.Tx, error) {
	return &awaitingReplyWindowSavepoint{}, nil
}

type awaitingReplyWindowSavepoint struct{ pgx.Tx }

func (*awaitingReplyWindowSavepoint) Commit(context.Context) error   { return nil }
func (*awaitingReplyWindowSavepoint) Rollback(context.Context) error { return nil }

func TestAwaitingReplyWindow_FollowUpCreateDeadline(t *testing.T) {
	contactID := uuid.New()
	cadenceStr := "monthly"
	tasks := &stubFollowUpTaskReader{err: db.ErrNotFound}
	contacts := &stubFollowUpContactReader{cadence: &cadenceStr}
	interactions := &stubInteractionResponseReader{}
	claims := &stubEventClaimer{}
	writer := &awaitingReplyWindowCreateWriter{stubFollowUpTaskWriter: &stubFollowUpTaskWriter{}}
	inserter := &recordingInserter{}
	settings := func(context.Context) (*todoist.Settings, string, error) {
		return &todoist.Settings{ProjectID: "project", LabelName: "label", IntegrationInstanceID: "instance"}, "token", nil
	}

	var observed []Decision
	h := NewFollowUpManager(FollowUpModeCutover, claims, contacts, tasks, writer, interactions, inserter, settings, "", testWatchdog())
	h.SetDecisionObserver(func(d Decision) { observed = append(observed, d) })
	occurredAt := time.Date(2026, 9, 15, 13, 30, 0, 0, time.Local)
	env := buildRecordedEnv(t, contactID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, occurredAt, cadenceStr)
	require.NoError(t, h.HandleEvent(context.Background(), &awaitingReplyWindowTx{}, env))
	require.Len(t, observed, 1)
	require.Equal(t, repository.FollowUpActionCreate, observed[0].Action)
	require.NotNil(t, observed[0].WouldDeadline)
	want := cadence.AwaitingReplyUntil(occurredAt, testWatchdog().DaysForCadence("monthly"))
	require.Equal(t, cadence.CalendarDate(want), cadence.CalendarDate(*observed[0].WouldDeadline))
	_, ok := inserter.args[0].(consumerjobs.TodoistTaskOpArgs)
	require.True(t, ok)
}

// The settings callback's concrete return type is referenced above; importing
// todoist here keeps this test's harness local to the consumer package.
