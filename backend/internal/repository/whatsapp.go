package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// HistoryNotification is one outstanding WhatsApp history-sync chunk. It holds
// a media POINTER (the marshalled waE2E.HistorySyncNotification, with any
// inline bootstrap payload nil'd before marshalling) and NEVER message content;
// the downloaded chunk is clamped and projected in memory and is not persisted.
//
// State is the lifecycle (pending | processing | done | failed). Disposition
// records WHY the row exists in the shape it does: `project` is a media-backed
// chunk; `dropped_inline` is one the server inlined against our explicit
// request, whose payload was discarded un-projected — it still runs the phase
// machine so its protocol receipt is sent exactly once. Phase is the durable
// resume point (recorded → downloaded → projected → acked → deleted).
//
// ClaimToken is the fencing token: every transition after a claim must present
// it, so a worker whose lease expired mid-work cannot clobber its successor.
type HistoryNotification struct {
	ID            uuid.UUID
	ProtocolMsgID string
	Notification  []byte
	SyncType      string
	ChunkOrder    int32
	State         string
	Disposition   string
	Phase         string
	OldestMsgTS   *time.Time
	Attempts      int32
	ClaimToken    *uuid.UUID
	LastError     *string
	Checkpoint    []byte
	ReceivedAt    time.Time
	ClaimedAt     *time.Time
	ProcessedAt   *time.Time
}

// WhatsAppChatConfig is the persistent per-chat gate state. MemberCount nil
// means UNRESOLVED, and the gate treats unresolved as NOT tracked — the
// deliberate divergence from Telegram, whose gate tracks by default on an
// unknown size.
type WhatsAppChatConfig struct {
	ChatJID      string
	ChatTitle    *string
	ChatType     string
	MemberCount  *int32
	Status       string
	LastLookupAt *time.Time
}

// History-notification lifecycle values. The DB CHECKs are the durable
// contract; these keep call sites from spelling them by hand.
const (
	HistoryNotificationStatePending    = "pending"
	HistoryNotificationStateProcessing = "processing"
	HistoryNotificationStateDone       = "done"
	HistoryNotificationStateFailed     = "failed"

	HistoryDispositionProject       = "project"
	HistoryDispositionDroppedInline = "dropped_inline"

	HistoryPhaseRecorded   = "recorded"
	HistoryPhaseDownloaded = "downloaded"
	HistoryPhaseProjected  = "projected"
	HistoryPhaseAcked      = "acked"
	HistoryPhaseDeleted    = "deleted"
)

// WhatsAppRepository wraps the WhatsApp-owned queries: the durable
// history-sync notification inbox and the per-chat group gate. Neither is
// message content — that stages into comms_message through
// CommsMessageRepository — which is why they share one repository over one
// query file rather than hanging off a type named for message content.
type WhatsAppRepository struct {
	queries db.Querier
}

// NewWhatsAppRepository creates a new WhatsApp repository.
func NewWhatsAppRepository(queries db.Querier) *WhatsAppRepository {
	return &WhatsAppRepository{queries: queries}
}

func convertDbHistoryNotification(n *db.WhatsappHistoryNotification) HistoryNotification {
	out := HistoryNotification{
		ProtocolMsgID: n.ProtocolMsgID,
		Notification:  n.Notification,
		SyncType:      n.SyncType,
		ChunkOrder:    n.ChunkOrder,
		State:         n.State,
		Disposition:   n.Disposition,
		Phase:         n.Phase,
		Attempts:      n.Attempts,
		Checkpoint:    n.Checkpoint,
		OldestMsgTS:   pgTimestamptzToTimePtr(n.OldestMsgTs),
		ClaimedAt:     pgTimestamptzToTimePtr(n.ClaimedAt),
		ProcessedAt:   pgTimestamptzToTimePtr(n.ProcessedAt),
	}
	if n.ID.Valid {
		out.ID = uuid.UUID(n.ID.Bytes)
	}
	if n.ClaimToken.Valid {
		token := uuid.UUID(n.ClaimToken.Bytes)
		out.ClaimToken = &token
	}
	if n.LastError.Valid {
		out.LastError = &n.LastError.String
	}
	if n.ReceivedAt.Valid {
		out.ReceivedAt = n.ReceivedAt.Time.UTC()
	}
	return out
}

func convertDbWhatsAppChatConfig(c *db.WhatsappChatConfig) WhatsAppChatConfig {
	out := WhatsAppChatConfig{
		ChatJID:      c.ChatJid,
		ChatType:     c.ChatType,
		Status:       c.Status,
		LastLookupAt: pgTimestamptzToTimePtr(c.LastLookupAt),
	}
	if c.ChatTitle.Valid {
		out.ChatTitle = &c.ChatTitle.String
	}
	if c.MemberCount.Valid {
		count := c.MemberCount.Int32
		out.MemberCount = &count
	}
	return out
}

// RecordNotification durably records an outstanding history chunk and returns
// its row id. Idempotent under WhatsApp's redelivery of an already-recorded
// protocol message. `notification` MUST already have any inline bootstrap
// payload nil'd — this table never stores message content. The starting phase
// is derived from the disposition inside the query.
func (r *WhatsAppRepository) RecordNotification(ctx context.Context, protocolMsgID string, notification []byte, syncType string, chunkOrder int32, oldestMsgTS *time.Time, disposition string) (uuid.UUID, error) {
	id, err := r.queries.RecordWhatsAppHistoryNotification(ctx, db.RecordWhatsAppHistoryNotificationParams{
		ProtocolMsgID: protocolMsgID,
		Notification:  notification,
		SyncType:      syncType,
		ChunkOrder:    chunkOrder,
		OldestMsgTs:   timeToPgTimestamptz(oldestMsgTS),
		Disposition:   disposition,
	})
	if err != nil {
		return uuid.Nil, err
	}
	if !id.Valid {
		return uuid.Nil, errors.New("record whatsapp history notification: no id returned")
	}
	return uuid.UUID(id.Bytes), nil
}

// ClaimNextNotification takes the next pending chunk (or reclaims one whose
// lease expired) and stamps a fresh claim token. Returns db.ErrNotFound when
// nothing is claimable.
func (r *WhatsAppRepository) ClaimNextNotification(ctx context.Context) (*HistoryNotification, error) {
	row, err := r.queries.ClaimNextWhatsAppHistoryNotification(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	n := convertDbHistoryNotification(row)
	return &n, nil
}

// SaveCheckpoint records projection progress. Returns false when the claim
// token no longer owns the row — the caller must then abandon the chunk
// without writing further state.
func (r *WhatsAppRepository) SaveCheckpoint(ctx context.Context, id uuid.UUID, token uuid.UUID, checkpoint []byte) (bool, error) {
	rows, err := r.queries.SaveWhatsAppHistoryCheckpoint(ctx, db.SaveWhatsAppHistoryCheckpointParams{
		ID:         uuidToPgUUID(id),
		ClaimToken: uuidToPgUUID(token),
		Checkpoint: jsonbOrEmpty(checkpoint),
	})
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// legalPhaseEdges is the phase machine, in full. The DB CHECK constrains the
// VALUES a phase may take; only this map constrains the TRANSITIONS. Without it
// the predecessor fence alone would happily accept a caller that names its own
// jump (from='projected', to='deleted'), skipping the protocol receipt — after
// which WhatsApp redelivers the chunk forever.
var legalPhaseEdges = map[string]string{
	HistoryPhaseRecorded:   HistoryPhaseDownloaded,
	HistoryPhaseDownloaded: HistoryPhaseProjected,
	HistoryPhaseProjected:  HistoryPhaseAcked,
	HistoryPhaseAcked:      HistoryPhaseDeleted,
}

// ErrIllegalPhaseEdge is returned by AdvancePhase for a (from, to) pair that is
// not one of the four steps of the history protocol.
var ErrIllegalPhaseEdge = errors.New("whatsapp: illegal history phase edge")

// AdvancePhase moves the durable resume point one legal edge forward. Three
// things must hold or nothing changes: the edge is one of the protocol's own
// steps (else ErrIllegalPhaseEdge), the caller still holds the claim token, and
// the row is actually at `from`. A false return means the lease moved on or the
// predecessor no longer holds.
func (r *WhatsAppRepository) AdvancePhase(ctx context.Context, id, token uuid.UUID, from, to string) (bool, error) {
	if next, ok := legalPhaseEdges[from]; !ok || next != to {
		return false, fmt.Errorf("%w: %s -> %s", ErrIllegalPhaseEdge, from, to)
	}
	rows, err := r.queries.AdvanceWhatsAppHistoryPhase(ctx, db.AdvanceWhatsAppHistoryPhaseParams{
		ID:         uuidToPgUUID(id),
		ClaimToken: uuidToPgUUID(token),
		FromPhase:  from,
		ToPhase:    to,
	})
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// MarkNotificationDone closes a chunk successfully. Token-fenced AND fenced on
// the terminal phase: a chunk that has not reached 'deleted' has not finished
// the protocol, and 'done' is unreachable by any later claim, so completing one
// early would abandon it silently. false means the lease moved on or the chunk
// is not at the end of the phase machine; in both cases nothing changed.
func (r *WhatsAppRepository) MarkNotificationDone(ctx context.Context, id uuid.UUID, token uuid.UUID) (bool, error) {
	rows, err := r.queries.MarkWhatsAppHistoryNotificationDone(ctx, db.MarkWhatsAppHistoryNotificationDoneParams{
		ID:         uuidToPgUUID(id),
		ClaimToken: uuidToPgUUID(token),
	})
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// MarkNotificationFailed records a terminal failure. Token-fenced. Recoverable
// only through RequeueFailedNotification.
func (r *WhatsAppRepository) MarkNotificationFailed(ctx context.Context, id uuid.UUID, token uuid.UUID, errMsg string) (bool, error) {
	rows, err := r.queries.MarkWhatsAppHistoryNotificationFailed(ctx, db.MarkWhatsAppHistoryNotificationFailedParams{
		ID:         uuidToPgUUID(id),
		ClaimToken: uuidToPgUUID(token),
		LastError:  stringToPgText(&errMsg),
	})
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// RequeueFailedNotification is the operator path: it returns a failed chunk to
// pending and clears its lease. Returns db.ErrNotFound when the row does not
// exist or is not failed.
func (r *WhatsAppRepository) RequeueFailedNotification(ctx context.Context, id uuid.UUID) error {
	rows, err := r.queries.RequeueFailedWhatsAppHistoryNotification(ctx, uuidToPgUUID(id))
	if err != nil {
		return err
	}
	if rows == 0 {
		return db.ErrNotFound
	}
	return nil
}

// ListNotifications returns the chunks in the given states, in claim order.
func (r *WhatsAppRepository) ListNotifications(ctx context.Context, states []string) ([]HistoryNotification, error) {
	rows, err := r.queries.ListWhatsAppHistoryNotifications(ctx, states)
	if err != nil {
		return nil, err
	}
	out := make([]HistoryNotification, 0, len(rows))
	for _, row := range rows {
		out = append(out, convertDbHistoryNotification(row))
	}
	return out, nil
}

// CountByStateAndDisposition returns chunk counts keyed "<state>/<disposition>"
// (e.g. "pending/project", "done/dropped_inline"). The caller sums the slices
// it needs; the dropped-inline count is the sum over the "*/dropped_inline"
// keys.
func (r *WhatsAppRepository) CountByStateAndDisposition(ctx context.Context) (map[string]int, error) {
	rows, err := r.queries.CountWhatsAppHistoryNotificationsByStateAndDisposition(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		out[row.State+"/"+row.Disposition] = int(row.NotificationCount)
	}
	return out, nil
}

// ObservedFloor reports the oldest staged WhatsApp message timestamp — the
// empirical answer to how deep the one-shot history actually reached. nil when
// nothing is staged yet.
func (r *WhatsAppRepository) ObservedFloor(ctx context.Context) (*time.Time, error) {
	oldest, err := r.queries.GetOldestCommsMessageSentAtForSource(ctx, InteractionSourceWhatsApp)
	if err != nil {
		return nil, err
	}
	return pgTimestamptzToTimePtr(oldest), nil
}

// GetChatConfig reads one chat's gate state. Returns db.ErrNotFound when the
// chat has never been observed.
func (r *WhatsAppRepository) GetChatConfig(ctx context.Context, chatJID string) (*WhatsAppChatConfig, error) {
	row, err := r.queries.GetWhatsAppChatConfig(ctx, chatJID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	cfg := convertDbWhatsAppChatConfig(row)
	return &cfg, nil
}

// UpsertChatConfig records what a re-observation resolved. A nil title or
// member count preserves the stored value, and the user's status override is
// never overwritten — pass it through UpsertChatConfig only when creating the
// row for the first time.
func (r *WhatsAppRepository) UpsertChatConfig(ctx context.Context, cfg WhatsAppChatConfig) (*WhatsAppChatConfig, error) {
	status := cfg.Status
	if status == "" {
		status = "auto"
	}
	row, err := r.queries.UpsertWhatsAppChatConfig(ctx, db.UpsertWhatsAppChatConfigParams{
		ChatJid:      cfg.ChatJID,
		ChatTitle:    stringToPgText(cfg.ChatTitle),
		ChatType:     cfg.ChatType,
		MemberCount:  int32ToPgInt4(cfg.MemberCount),
		Status:       status,
		LastLookupAt: timeToPgTimestamptz(cfg.LastLookupAt),
	})
	if err != nil {
		return nil, err
	}
	out := convertDbWhatsAppChatConfig(row)
	return &out, nil
}

// BackdateClaimForTest is a test-only helper that ages a chunk's claim past the
// 15-minute lease so a fresh claim reclaims it — the "worker died mid-chunk"
// case. Production code MUST NOT call this.
func (r *WhatsAppRepository) BackdateClaimForTest(ctx context.Context, id uuid.UUID) error {
	return r.queries.BackdateWhatsAppHistoryClaim(ctx, uuidToPgUUID(id))
}
