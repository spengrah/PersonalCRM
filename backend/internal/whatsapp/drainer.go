package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
)

const (
	// historyConversationPacing bounds how fast a chunk's conversations are
	// projected. Each staged message costs three to five database round trips
	// through IngestMessage, so a large conversation is seconds of pool time on
	// a machine that is also serving the API; this is a POOL-FAIRNESS guard,
	// not a flood-wait guard — no network call runs per conversation, because
	// group metadata is resolved in the pre-pass. It is skipped entirely for
	// conversations that staged nothing, which is the majority on a clamped
	// bootstrap chunk and the only case where it could have dominated.
	historyConversationPacing = 200 * time.Millisecond

	// maxTransientAttempts caps how many times one chunk may be re-claimed
	// after a transient failure before the next one is made terminal. Without a
	// cap a permanently-transient chunk re-downloads its full payload every 15
	// minutes for the life of the deployment, visible only as a processing
	// count that never moves. The operator requeue is the escape hatch.
	maxTransientAttempts = 8
)

var (
	// errLeaseLost means a fenced write found the claim token no longer owns
	// the row. It is not a failure: a successor is doing our work. The chunk is
	// abandoned immediately, without writing anything further.
	errLeaseLost = errors.New("whatsapp: history chunk lease was lost")

	// errChunkFailed reports that the chunk was durably marked failed. The
	// drain loop treats it as a handled outcome and moves on: the row is no
	// longer claimable, so there is no loop to spin.
	errChunkFailed = errors.New("whatsapp: history chunk failed permanently")
)

// historyNotificationStore is the slice of repository.WhatsAppRepository the
// drainer writes through. Every method after the claim is token-fenced, so a
// worker whose lease expired cannot clobber its successor.
type historyNotificationStore interface {
	ClaimNextNotification(ctx context.Context) (*repository.HistoryNotification, error)
	SaveCheckpoint(ctx context.Context, id, token uuid.UUID, checkpoint []byte) (bool, error)
	AdvancePhase(ctx context.Context, id, token uuid.UUID, from, to string) (bool, error)
	MarkNotificationDone(ctx context.Context, id, token uuid.UUID) (bool, error)
	MarkNotificationFailed(ctx context.Context, id, token uuid.UUID, errMsg string) (bool, error)
}

// HistoryDrainer executes the history protocol for every recorded chunk:
// download, gate the whole chunk, project through the shared ingest choke
// point, acknowledge, delete the server-side media, mark done — each step
// durably fenced by phase and claim token so any interruption resumes at the
// phase it reached.
//
// It is SINGLE-GOROUTINE by construction: it runs on River's worker goroutine,
// starts nothing, and holds no shared mutable state — every field is set at
// construction and read-only afterwards. That is what lets the package's own
// design guards stay green without an amendment.
type HistoryDrainer struct {
	repo          historyNotificationStore
	ingestor      MessageIngestor
	gate          *ChatGate
	fetcherSource func() HistoryFetcher
	// pacing is a field rather than the constant read directly, mirroring
	// ChatGate.lookupTimeout, so a test can zero it without a mutable global a
	// parallel test could race.
	pacing time.Duration
}

// NewHistoryDrainer builds the drainer.
//
// A nil fetcherSource is normalized into one that returns nil. The composition
// root really does produce a nil FUNC VALUE — the drain worker is registered
// whenever the feature flag is on, including on a boot where the device store
// failed to open and no manager exists — and calling it would panic once a
// minute, forever. Normalizing collapses "no source" and "source with no
// client" into the single deferral the drainer already handles.
func NewHistoryDrainer(repo historyNotificationStore, ingestor MessageIngestor, gate *ChatGate, fetcherSource func() HistoryFetcher) *HistoryDrainer {
	if fetcherSource == nil {
		fetcherSource = func() HistoryFetcher { return nil }
	}
	return &HistoryDrainer{
		repo:          repo,
		ingestor:      ingestor,
		gate:          gate,
		fetcherSource: fetcherSource,
		pacing:        historyConversationPacing,
	}
}

// Drain drains the whole claimable backlog, in chunk order, sequentially.
//
// A bootstrap sync arrives as many chunks; at one chunk per tick a backfill
// would take an hour of wall time for no reason. Termination is STRUCTURAL
// rather than by a counter: every iteration either drives its row to done
// (unclaimable), leaves it processing under a fresh 15-minute lease
// (unclaimable), or returns.
//
// A transient failure is RETURNED so River records it — a silently-swallowed
// retry would make a stuck backfill invisible.
func (d *HistoryDrainer) Drain(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			// The job timeout cancelled us between chunks. That is the ordinary
			// resume path, not a failure: the next tick picks up where we
			// stopped, from the phase each row reached.
			return nil
		}

		fetcher := d.fetcherSource()
		if fetcher == nil {
			logger.Debug().Msg("whatsapp: no connected client; deferring the history drain")
			return nil
		}
		if d.ingestor == nil {
			// Unreachable in production: without an ingestor the readiness gate
			// keeps the client from ever connecting, so nothing records a
			// chunk. The drainer must not be the thing that discovers that.
			logger.Warn().Msg("whatsapp: no message ingestor; deferring the history drain")
			return nil
		}

		n, err := d.repo.ClaimNextNotification(ctx)
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("whatsapp: claim history chunk: %w", err)
		}
		if n.ClaimToken == nil {
			// Unreachable: the claim query stamps a fresh token in the same
			// statement that returns the row.
			return fmt.Errorf("whatsapp: claimed history chunk %s carries no claim token", n.ID)
		}

		if err := d.drainOne(ctx, fetcher, n); err != nil {
			switch {
			case errors.Is(err, errLeaseLost):
				logger.Info().Str("chunk_id", n.ID.String()).
					Msg("whatsapp: history chunk lease moved on; a successor owns it")
			case errors.Is(err, errChunkFailed):
				// Already recorded as failed and no longer claimable.
			default:
				return err
			}
		}
	}
}

// drainOne runs the protocol for ONE chunk as a sequential fall-through, never
// a switch.
//
// Written as a switch with one arm per claim, a fresh chunk would need four
// claims to finish — and because each claim leaves a live 15-minute lease
// behind it, that is roughly an hour per chunk. The if-chain lets a
// freshly-recorded chunk run all four steps under one claim while a reclaimed
// one enters at its stored phase and runs only what remains.
func (d *HistoryDrainer) drainOne(ctx context.Context, fetcher HistoryFetcher, n *repository.HistoryNotification) error {
	token := *n.ClaimToken
	phase := n.Phase

	if phase == repository.HistoryPhaseRecorded || phase == repository.HistoryPhaseDownloaded {
		next, err := d.downloadAndProject(ctx, fetcher, n, token, phase)
		if err != nil {
			return err
		}
		phase = next
	}

	if phase == repository.HistoryPhaseProjected {
		if err := fetcher.AckHistorySync(ctx, n.ProtocolMsgID); err != nil {
			return d.transient(ctx, n, token, err)
		}
		if err := d.advance(ctx, n.ID, token, repository.HistoryPhaseProjected, repository.HistoryPhaseAcked); err != nil {
			return err
		}
		phase = repository.HistoryPhaseAcked
	}

	if phase == repository.HistoryPhaseAcked {
		if err := d.deleteMedia(ctx, fetcher, n, token); err != nil {
			return err
		}
		if err := d.advance(ctx, n.ID, token, repository.HistoryPhaseAcked, repository.HistoryPhaseDeleted); err != nil {
			return err
		}
	}

	// The chunk is now at 'deleted' whichever step this claim entered at. The
	// completion is fenced on that phase in SQL, not on the local variable, so
	// there is no fourth assignment to make.
	done, err := d.repo.MarkNotificationDone(ctx, n.ID, token)
	if err != nil {
		return fmt.Errorf("whatsapp: mark history chunk done: %w", err)
	}
	if !done {
		return errLeaseLost
	}
	logger.Info().Str("chunk_id", n.ID.String()).Str("sync_type", n.SyncType).
		Int32("chunk_order", n.ChunkOrder).Msg("whatsapp: history chunk drained")
	return nil
}

// downloadAndProject runs the download, the chunk-wide gate pre-pass and the
// projection, and returns the phase the chunk reached (always 'projected').
func (d *HistoryDrainer) downloadAndProject(ctx context.Context, fetcher HistoryFetcher, n *repository.HistoryNotification, token uuid.UUID, phase string) (string, error) {
	chunk, err := fetcher.DownloadHistorySync(ctx, n.Notification)
	if err != nil {
		return d.classifyDownloadFailure(ctx, n, token, phase, err)
	}

	if phase == repository.HistoryPhaseRecorded {
		if err := d.advance(ctx, n.ID, token, repository.HistoryPhaseRecorded, repository.HistoryPhaseDownloaded); err != nil {
			return "", err
		}
	}

	// The pre-pass runs to completion BEFORE anything is projected, so a chunk
	// is never half-gated: an undecidable group aborts the whole chunk with
	// nothing stored, rather than leaving the first half persisted and the
	// second half not.
	decisions, err := d.resolveChunkGates(ctx, fetcher, chunk)
	if err != nil {
		return "", d.transient(ctx, n, token, err)
	}

	if err := d.project(ctx, fetcher, n, token, chunk, decisions); err != nil {
		if errors.Is(err, errLeaseLost) {
			return "", err
		}
		return "", d.transient(ctx, n, token, err)
	}

	if err := d.advance(ctx, n.ID, token, repository.HistoryPhaseDownloaded, repository.HistoryPhaseProjected); err != nil {
		return "", err
	}
	return repository.HistoryPhaseProjected, nil
}

// classifyDownloadFailure applies the download half of the failure taxonomy.
//
// The media-gone branch is the subtle one. "Has a conversation completed?" is a
// SEMANTIC test — the checkpoint parses to a conversation_index — never a byte
// test: the column is JSONB NOT NULL DEFAULT '{}' and the repository writes
// through a helper that substitutes '{}' for empty input, so len(Checkpoint) > 0
// is true for EVERY row and would classify a chunk that stored nothing as
// terminal-successful, sending its receipt and discarding its one-shot history.
func (d *HistoryDrainer) classifyDownloadFailure(ctx context.Context, n *repository.HistoryNotification, token uuid.UUID, phase string, cause error) (string, error) {
	switch {
	case errors.Is(cause, ErrLIDMappingsIncomplete):
		// The chunk downloaded but its PN-LID mappings did not read back.
		// Projecting now would attribute every resolvable LID-only peer as
		// permanently unmatched, so nothing is projected and nothing advances.
		return "", d.transient(ctx, n, token, cause)

	case errors.Is(cause, ErrHistoryNotificationMalformed):
		return "", d.terminalFailure(ctx, n, token, cause)

	case isMediaGone(cause):
		checkpoint, resumable := parseHistoryCheckpoint(n.Checkpoint)
		if phase != repository.HistoryPhaseDownloaded || !resumable {
			// Nothing was ever stored from this chunk and no retry can produce
			// the blob. Terminal, and operator-recoverable by requeue.
			return "", d.terminalFailure(ctx, n, token, cause)
		}
		// At least one conversation completed, so its content is already in
		// comms_message and no retry can produce the rest. Completing the
		// protocol is strictly better than throwing the staged half away.
		logger.Warn().Err(cause).Str("chunk_id", n.ID.String()).
			Int("conversations_completed", *checkpoint.ConversationIndex+1).
			Msg("whatsapp: history media is gone after a partial projection; completing the chunk with what was stored")
		if err := d.advance(ctx, n.ID, token, repository.HistoryPhaseDownloaded, repository.HistoryPhaseProjected); err != nil {
			return "", err
		}
		return repository.HistoryPhaseProjected, nil

	default:
		return "", d.transient(ctx, n, token, cause)
	}
}

// deleteMedia removes our own history blob from WhatsApp's media server.
//
// A dropped-inline row never downloaded anything, so there is no blob to
// delete — a documented no-op, not a skipped step. A delete that finds the
// media already gone is SUCCESS: that is exactly the outcome the step wanted.
func (d *HistoryDrainer) deleteMedia(ctx context.Context, fetcher HistoryFetcher, n *repository.HistoryNotification, token uuid.UUID) error {
	if n.Disposition == repository.HistoryDispositionDroppedInline {
		logger.Debug().Str("chunk_id", n.ID.String()).
			Msg("whatsapp: dropped-inline chunk has no server-side media to delete")
		return nil
	}
	err := fetcher.DeleteHistoryMedia(ctx, n.Notification)
	if err == nil {
		return nil
	}
	if isMediaGone(err) {
		logger.Debug().Err(err).Str("chunk_id", n.ID.String()).
			Msg("whatsapp: history media was already gone; the delete step is satisfied")
		return nil
	}
	if errors.Is(err, ErrHistoryNotificationMalformed) {
		return d.terminalFailure(ctx, n, token, err)
	}
	return d.transient(ctx, n, token, err)
}

// chatDecision is one conversation's pre-pass result.
type chatDecision struct {
	// Track reports whether this conversation's messages may be stored.
	Track bool
	// EffectiveJID is the chat JID the projection passes for every message in
	// this conversation. It is not always the conversation's own id: a DM keyed
	// on a LID is canonicalized to the phone-number thread when the chunk names
	// one, so history and live messages for one peer land under one thread_id.
	EffectiveJID string
}

// resolveChunkGates decides tracking for EVERY conversation in the chunk before
// any of it is stored.
//
// The map is keyed on the RAW conversation id so the projection loop looks a
// decision up without re-parsing; the decision itself is made on the NORMALIZED
// JID, because whatsapp_chat_config rows and comms_message.thread_id are keyed
// on that form and a second un-normalized row for one chat would split the
// gate's memory.
func (d *HistoryDrainer) resolveChunkGates(ctx context.Context, fetcher HistoryFetcher, chunk *waHistorySync.HistorySync) (map[string]chatDecision, error) {
	conversations := chunk.GetConversations()
	decisions := make(map[string]chatDecision, len(conversations))
	if len(conversations) == 0 {
		return decisions, nil
	}
	if d.gate == nil {
		// Fail closed: an absent gate is not a decision that a group is safe.
		return nil, fmt.Errorf("%w: no chat gate", ErrChatGateUndecided)
	}
	accountJID := fetcher.AccountJID()

	for _, conv := range conversations {
		raw := conv.GetID()
		if _, seen := decisions[raw]; seen {
			continue
		}

		jid, err := types.ParseJID(raw)
		if err != nil {
			logger.Warn().Err(err).Msg("whatsapp: unparseable history conversation id; skipping the conversation")
			decisions[raw] = chatDecision{}
			continue
		}
		normalized := normalizeServer(jid).ToNonAD()

		switch normalized.Server {
		case types.GroupServer:
			var snapshot *ChatGroupInfo
			if participants := conv.GetParticipant(); len(participants) > 0 {
				snapshot = &ChatGroupInfo{Title: conv.GetName(), MemberCount: len(participants)}
			}
			tracked, err := d.gate.ShouldTrackHistoryChat(ctx, normalized.String(), snapshot, accountJID)
			if err != nil {
				// Undecided: abort the WHOLE chunk before anything is stored.
				return nil, err
			}
			decisions[raw] = chatDecision{Track: tracked, EffectiveJID: normalized.String()}

		case types.DefaultUserServer, types.HiddenUserServer:
			// A DM is decided per message by the parser, which owns the
			// self-chat rejection because that needs the account identity.
			decisions[raw] = chatDecision{Track: true, EffectiveJID: effectiveDMChatJID(conv, normalized)}
			if conv.GetPnhDuplicateLidThread() {
				// Its documented semantics are absent from the module, so it is
				// OBSERVED rather than acted on: because the message dedup
				// index excludes thread_id, a genuinely duplicated thread's
				// messages collapse onto the rows already staged.
				logger.Warn().Msg("whatsapp: history conversation is flagged as a duplicate LID thread")
			}

		default:
			// Broadcast, status broadcast, newsletter and anything the protocol
			// grows later: never a person-to-person turn.
			decisions[raw] = chatDecision{}
		}
	}
	return decisions, nil
}

// effectiveDMChatJID canonicalizes a LID-keyed DM onto its phone-number thread
// when the chunk names one.
//
// normalizeServer deliberately does not map @lid to @s.whatsapp.net — those are
// two identities of one person, not two transports — but a Conversation states
// the pair explicitly. Without this, history keyed on @lid and live messages
// arriving on @s.whatsapp.net land under different thread_ids. The message
// dedup index excludes thread_id, so a split can never duplicate a row; what it
// costs is chat scope, so bursts do not coalesce across the boundary.
func effectiveDMChatJID(conv *waHistorySync.Conversation, normalized types.JID) string {
	if normalized.Server != types.HiddenUserServer {
		return normalized.String()
	}
	pn := conv.GetPnJID()
	if pn == "" {
		return normalized.String()
	}
	parsed, err := types.ParseJID(pn)
	if err != nil {
		logger.Warn().Err(err).Msg("whatsapp: unparseable pnJID on a history conversation; keying the chat on its own id")
		return normalized.String()
	}
	return normalizeServer(parsed).ToNonAD().String()
}

// project stages the chunk's conversations, checkpointing at each conversation
// boundary.
//
// The resume unit is the CONVERSATION, never a message inside one: an
// intra-conversation resume point would depend on the messages within a
// conversation coming back in the same order on a re-download, which nothing in
// the protocol documents and whose violation would be silent history loss.
// Re-walking a conversation costs nothing, because the staging upsert is
// content-immutable on conflict, so every already-staged message is a no-op.
func (d *HistoryDrainer) project(ctx context.Context, fetcher HistoryFetcher, n *repository.HistoryNotification, token uuid.UUID, chunk *waHistorySync.HistorySync, decisions map[string]chatDecision) error {
	conversations := chunk.GetConversations()
	floor := BackfillFloorTime()

	var staged, stubSkipped, stubWithContent, malformed, preClamp int

	for i := resumePoint(n.Checkpoint, conversations); i < len(conversations); i++ {
		conv := conversations[i]
		decision := decisions[conv.GetID()]
		if !decision.Track {
			continue
		}

		conversationStaged := 0
		for _, historyMsg := range conv.GetMessages() {
			web := historyMsg.GetMessage()

			// A WebMessageInfo is also WhatsApp's envelope for NON-messages:
			// revokes, undecryptable ciphertexts, missed calls, membership and
			// security notices. A nil Message is the verified marker for those,
			// and it is the ONLY skip condition here.
			if web.GetMessage() == nil {
				stubSkipped++
				continue
			}
			if web.GetMessageStubType() != waWeb.WebMessageInfo_UNKNOWN {
				// A stub type ON CONTENT is staged, not guessed away: nothing
				// in the pinned library confirms or refutes that a genuine turn
				// can carry one, and this payload is one-shot, so a wrong guess
				// is irreversible loss. The counter is the evidence a future
				// tightening would need.
				stubWithContent++
				logger.Warn().Str("stub_type", web.GetMessageStubType().String()).
					Msg("whatsapp: history message carries a stub type alongside real content; staging it")
			}
			if web.GetKey().GetID() == "" {
				// IngestMessage refuses an empty id; skipping one malformed row
				// must not strand a chunk that is otherwise projectable.
				malformed++
				continue
			}
			// The clamp runs on the RAW timestamp, before the parser: a
			// pre-horizon message is never decoded, never reaches
			// IngestedMessage, and cannot reach any writer.
			if time.Unix(int64(web.GetMessageTimestamp()), 0).UTC().Before(floor) {
				preClamp++
				continue
			}

			msg, eligible, err := fetcher.ProjectHistoryMessage(ctx, decision.EffectiveJID, web)
			if err != nil {
				malformed++
				logger.Warn().Err(err).Msg("whatsapp: could not project a history message; skipping it")
				continue
			}
			if !eligible || msg.MessageID == "" || msg.ChatJID == "" {
				continue
			}
			if err := d.ingestor.IngestMessage(ctx, msg); err != nil {
				return fmt.Errorf("whatsapp: ingest history message: %w", err)
			}
			conversationStaged++
		}
		staged += conversationStaged

		ok, err := d.repo.SaveCheckpoint(ctx, n.ID, token, checkpointJSON(i, conv.GetID()))
		if err != nil {
			return fmt.Errorf("whatsapp: save history checkpoint: %w", err)
		}
		if !ok {
			return errLeaseLost
		}
		if conversationStaged > 0 {
			d.pace(ctx)
		}
	}

	logger.Info().Str("chunk_id", n.ID.String()).
		Int("staged", staged).
		Int("stub_skipped", stubSkipped).
		Int("stub_with_content", stubWithContent).
		Int("malformed", malformed).
		Int("pre_clamp", preClamp).
		Msg("whatsapp: history chunk projected")
	return nil
}

// pace yields between conversations that actually staged something. It is a
// select rather than a sleep so the job timeout can cut it short, and it starts
// no goroutine of its own.
func (d *HistoryDrainer) pace(ctx context.Context) {
	if d.pacing <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d.pacing):
	}
}

// advance moves the durable resume point one legal edge forward, turning a
// fenced no-op into errLeaseLost.
func (d *HistoryDrainer) advance(ctx context.Context, id, token uuid.UUID, from, to string) error {
	ok, err := d.repo.AdvancePhase(ctx, id, token, from, to)
	if err != nil {
		return fmt.Errorf("whatsapp: advance history phase %s -> %s: %w", from, to, err)
	}
	if !ok {
		return errLeaseLost
	}
	return nil
}

// transient returns the cause so River records it — unless this chunk has
// already burned its attempt budget, in which case the failure is made terminal
// so it surfaces in the status counts and the operator listing instead of
// grinding through a full re-download every 15 minutes forever.
func (d *HistoryDrainer) transient(ctx context.Context, n *repository.HistoryNotification, token uuid.UUID, cause error) error {
	if n.Attempts > maxTransientAttempts {
		return d.terminalFailure(ctx, n, token,
			fmt.Errorf("gave up after %d attempts: %w", n.Attempts, cause))
	}
	return cause
}

// terminalFailure records a permanent failure. Recoverable only through
// crm-admin --whatsapp-requeue-history.
func (d *HistoryDrainer) terminalFailure(ctx context.Context, n *repository.HistoryNotification, token uuid.UUID, cause error) error {
	ok, err := d.repo.MarkNotificationFailed(ctx, n.ID, token, cause.Error())
	if err != nil {
		return fmt.Errorf("whatsapp: mark history chunk failed: %w", err)
	}
	if !ok {
		return errLeaseLost
	}
	logger.Error().Err(cause).Str("chunk_id", n.ID.String()).
		Msg("whatsapp: history chunk failed permanently; recover with crm-admin --whatsapp-requeue-history")
	return errChunkFailed
}

// historyCheckpoint is the durable resume point: the index of the LAST
// COMPLETED conversation, plus that conversation's id so the index can be
// VERIFIED against a re-downloaded chunk rather than trusted.
//
// ConversationIndex is a POINTER because the column is JSONB NOT NULL DEFAULT
// '{}': a value type could not tell "no checkpoint" from "completed
// conversation 0", and that distinction decides whether a chunk whose media has
// gone missing completes or fails.
type historyCheckpoint struct {
	ConversationIndex *int   `json:"conversation_index"`
	ChatJID           string `json:"chat_jid"`
}

// parseHistoryCheckpoint reports whether a conversation has completed, and
// which. ok=false covers absent, empty, unparseable and index-less checkpoints
// alike — every shape that means "nothing has been staged from this chunk".
func parseHistoryCheckpoint(raw []byte) (historyCheckpoint, bool) {
	if len(raw) == 0 {
		return historyCheckpoint{}, false
	}
	var checkpoint historyCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		logger.Warn().Err(err).Msg("whatsapp: unparseable history checkpoint; re-projecting from the start")
		return historyCheckpoint{}, false
	}
	if checkpoint.ConversationIndex == nil {
		return historyCheckpoint{}, false
	}
	return checkpoint, true
}

// resumePoint returns the conversation index the projection loop starts at.
//
// The stored index is the last COMPLETED conversation, so the loop resumes at
// index+1; returning it unchanged would re-walk a conversation that already
// finished. The chat_jid check turns "two downloads decode to the same
// conversation order" from an assumption into a verified precondition: on a
// mismatch the checkpoint is discarded and the chunk is re-projected from the
// start, which is safe because the staging upsert is content-immutable.
func resumePoint(raw []byte, conversations []*waHistorySync.Conversation) int {
	checkpoint, ok := parseHistoryCheckpoint(raw)
	if !ok {
		return 0
	}
	index := *checkpoint.ConversationIndex
	if index < 0 || index >= len(conversations) {
		logger.Warn().Int("conversation_index", index).Int("conversations", len(conversations)).
			Msg("whatsapp: history checkpoint is out of range; re-projecting from the start")
		return 0
	}
	if conversations[index].GetID() != checkpoint.ChatJID {
		logger.Warn().Int("conversation_index", index).
			Msg("whatsapp: history checkpoint does not match the re-downloaded chunk; re-projecting from the start")
		return 0
	}
	return index + 1
}

// checkpointJSON renders the resume point for one completed conversation.
func checkpointJSON(index int, chatJID string) []byte {
	raw, err := json.Marshal(historyCheckpoint{ConversationIndex: &index, ChatJID: chatJID})
	if err != nil {
		// Unreachable: the struct is two JSON-native fields.
		logger.Warn().Err(err).Msg("whatsapp: could not render the history checkpoint")
		return nil
	}
	return raw
}

// isMediaGone reports whether an error means the server-side blob no longer
// exists. For a DOWNLOAD that is terminal; for the DELETE it is success, since
// a blob that is already gone is exactly what the step wanted.
func isMediaGone(err error) bool {
	return errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) ||
		errors.Is(err, whatsmeow.ErrNoURLPresent)
}
