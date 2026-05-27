// Package service — MeetingNoteService owns the user-driven
// conflict-resolution surface for meeting_note rows. It is the
// counterpart to the daemon-driven IngestService.handleMeetingNoteRecorded
// path: where the daemon flow runs inside a per-event savepoint and
// publishes through the event bus, the resolve-link flow opens its own
// short-lived tx and writes interactions through the same
// ContactInteractionRecorder.RecordInteractionTx surface so cadence and
// follow-up state apply atomically.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"personal-crm/backend/internal/anarlog"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LinkageTargetReader is the narrow surface MeetingNoteService needs
// for both the resolve-link target-existence check (must run inside
// the resolve tx so the read+write are serializable) and the
// needs-attention preview projection (best-effort, no tx). Concrete is
// a small adapter over CalendarEventRepository + PhoneCallRepository
// (see meetingNoteLinkageTargetReader in cmd/crm-api/main.go).
type LinkageTargetReader interface {
	GetEventByID(ctx context.Context, id uuid.UUID) (*repository.CalendarEvent, error)
	GetPhoneCallByID(ctx context.Context, id uuid.UUID) (*repository.PhoneCall, error)
	GetEventByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.CalendarEvent, error)
	GetPhoneCallByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.PhoneCall, error)
}

// MeetingNoteResolveReader is the narrow read surface MeetingNoteService
// needs. Concrete is *repository.MeetingNoteRepository.
type MeetingNoteResolveReader interface {
	GetMeetingNoteByID(ctx context.Context, id uuid.UUID) (*repository.MeetingNote, error)
	GetMeetingNoteByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.MeetingNote, error)
	GetMeetingNoteByIDForUpdateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.MeetingNote, error)
	ListMeetingNotesNeedingAttention(ctx context.Context, hostID *uuid.UUID) ([]repository.MeetingNote, error)
}

// MeetingNoteResolveWriter is the narrow write surface needed by the
// resolve-link flow. Concrete is *repository.MeetingNoteRepository.
type MeetingNoteResolveWriter interface {
	ResolveMeetingNoteToLinkedTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, kind string, linkedID uuid.UUID) (*repository.MeetingNote, error)
	ClearMeetingNoteConflictTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, newState string, newResolvedSetHash string) (*repository.MeetingNote, error)
}

// ErrResolveLinkRowNotFound is returned by MeetingNoteService.ResolveLink
// when the meeting_note row does not exist. Handler maps to 404.
var ErrResolveLinkRowNotFound = errors.New("meeting_note not found")

// ErrResolveLinkNotPending is returned when the row exists but is not in
// linkage_state = 'conflict_pending'. Handler maps to 409.
var ErrResolveLinkNotPending = errors.New("meeting_note is not awaiting conflict resolution")

// ErrResolveLinkSnapshotMissing is returned when the row is in
// conflict_pending but conflict_candidates is NULL — a defensive
// invariant that should not fire in steady state. Handler maps to 422.
var ErrResolveLinkSnapshotMissing = errors.New("conflict_candidates snapshot missing on conflict_pending row")

// ErrResolveLinkIDNotCandidate is returned when the user-supplied
// (kind, id) tuple is not present in the persisted snapshot. Handler
// maps to 400.
var ErrResolveLinkIDNotCandidate = errors.New("id is not one of the recorded candidates")

// ErrResolveLinkTargetMissing is returned when the target row referenced
// by a snapshot entry no longer exists. Handler maps to 404.
var ErrResolveLinkTargetMissing = errors.New("linked target no longer exists")

// MeetingNoteService owns the user-driven conflict resolution flow.
type MeetingNoteService struct {
	database        *db.Database
	meetingReader   MeetingNoteResolveReader
	meetingWriter   MeetingNoteResolveWriter
	linkageTargets  LinkageTargetReader
	identityLookup  AnarlogIdentityLookup
	titleMatcher    IngestTitleMatcher
	titleDiscovery  IngestTitleDiscoveryWriter
	contactRecorder ContactInteractionRecorder
}

// NewMeetingNoteService constructs a MeetingNoteService bound to its
// narrow dependencies.
func NewMeetingNoteService(
	database *db.Database,
	meetingReader MeetingNoteResolveReader,
	meetingWriter MeetingNoteResolveWriter,
	linkageTargets LinkageTargetReader,
	identityLookup AnarlogIdentityLookup,
	titleMatcher IngestTitleMatcher,
	titleDiscovery IngestTitleDiscoveryWriter,
	contactRecorder ContactInteractionRecorder,
) *MeetingNoteService {
	return &MeetingNoteService{
		database:        database,
		meetingReader:   meetingReader,
		meetingWriter:   meetingWriter,
		linkageTargets:  linkageTargets,
		identityLookup:  identityLookup,
		titleMatcher:    titleMatcher,
		titleDiscovery:  titleDiscovery,
		contactRecorder: contactRecorder,
	}
}

// ResolveLinkInput captures the user's "link to this candidate" choice.
// The nil-input variant of ResolveLink encodes the "none of these"
// branch (the discriminated-union HTTP body collapses to this two-state
// API at the service layer).
type ResolveLinkInput struct {
	Kind string    // "event" | "phone_call"
	ID   uuid.UUID // target row UUID
}

// CreatedInteraction is a service-layer view of an interaction created
// during a resolve-link flow. Returned to the handler so the HTTP
// response can include the audit trail.
type CreatedInteraction struct {
	ID         uuid.UUID
	ContactID  uuid.UUID
	SourceRef  string
	OccurredAt string
	Direction  string
}

// ResolveLinkResult is the value returned by ResolveLink.
type ResolveLinkResult struct {
	MeetingNote         *repository.MeetingNote
	InteractionsCreated []CreatedInteraction
}

// ResolveLink implements the POST /meeting-notes/:id/resolve-link state
// machine. nil input encodes "none of these"; a populated pointer is
// the "link to this candidate" branch.
func (s *MeetingNoteService) ResolveLink(ctx context.Context, mnID uuid.UUID, input *ResolveLinkInput) (*ResolveLinkResult, error) {
	if s.database == nil || s.meetingReader == nil || s.meetingWriter == nil {
		return nil, errors.New("MeetingNoteService not configured for resolve-link")
	}

	var result *ResolveLinkResult
	var followUps []func(context.Context)

	// Service-layer errors are returned from the tx closure so
	// pgx.BeginTxFunc rolls back any partial writes (interactions,
	// discovery rows) that happened before the failure. Read-only
	// pre-check failures (ErrResolveLinkRowNotFound,
	// ErrResolveLinkNotPending) also propagate this way — rolling
	// back a no-write tx is harmless and the rule stays uniform.
	// The handler maps the sentinel errors back to the right HTTP
	// codes via errors.Is.
	txErr := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		prior, err := s.meetingReader.GetMeetingNoteByIDForUpdateTx(ctx, tx, mnID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return ErrResolveLinkRowNotFound
			}
			return fmt.Errorf("read meeting_note: %w", err)
		}
		if prior.LinkageState != repository.LinkageStateConflictPending {
			return ErrResolveLinkNotPending
		}

		if input != nil {
			res, created, fus, ferr := s.resolveToLinked(ctx, tx, prior, *input)
			if ferr != nil {
				return ferr
			}
			result = &ResolveLinkResult{MeetingNote: res, InteractionsCreated: created}
			followUps = fus
			return nil
		}

		res, created, fus, ferr := s.resolveNoneOfThese(ctx, tx, prior)
		if ferr != nil {
			return ferr
		}
		result = &ResolveLinkResult{MeetingNote: res, InteractionsCreated: created}
		followUps = fus
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	// Run follow-up closures best-effort outside the tx. The user-facing
	// HTTP response does not fail when a follow-up callback panics or
	// returns an error — mirrors the daemon-side dispatch loop's
	// post-commit semantics. We recover panics per-closure so one bad
	// closure cannot poison the rest.
	for _, fn := range followUps {
		runFollowUpBestEffort(ctx, fn)
	}

	s.logResolution(result, input)
	return result, nil
}

// resolveToLinked implements the action="link" branch.
func (s *MeetingNoteService) resolveToLinked(ctx context.Context, tx pgx.Tx, prior *repository.MeetingNote, input ResolveLinkInput) (*repository.MeetingNote, []CreatedInteraction, []func(context.Context), error) {
	if len(prior.ConflictCandidates) == 0 {
		return nil, nil, nil, ErrResolveLinkSnapshotMissing
	}
	var snapshot []repository.ConflictCandidateSummary
	if err := json.Unmarshal(prior.ConflictCandidates, &snapshot); err != nil {
		return nil, nil, nil, fmt.Errorf("decode conflict_candidates: %w", err)
	}
	if !snapshotContains(snapshot, input.Kind, input.ID) {
		return nil, nil, nil, ErrResolveLinkIDNotCandidate
	}

	// Verify the target row still exists inside the SAME tx as the
	// state transition so a concurrent delete cannot land between the
	// existence check and the linked-pointer write. Outside the
	// snapshot we have no other anchor on the target's existence; a
	// stale snapshot is the user-visible failure mode and we want a
	// clear 404 over a silently broken pointer.
	candidate, err := s.fetchCandidateAsLinkageTx(ctx, tx, input.Kind, input.ID)
	if err != nil {
		return nil, nil, nil, err
	}

	// Re-resolve tagged participants against the current contact
	// catalog so a newly-created contact between snapshot-time and
	// resolve-time still produces walk-in interactions.
	resolvedTagged, err := s.reResolveTagged(ctx, tx, prior.Participants)
	if err != nil {
		return nil, nil, nil, err
	}

	walkins := stepFiveWalkins(*candidate, resolvedTagged, prior.AnarlogSessionID)
	created, followUps, err := s.writeDesiredInteractions(ctx, tx, prior, walkins)
	if err != nil {
		return nil, nil, nil, err
	}

	updated, err := s.meetingWriter.ResolveMeetingNoteToLinkedTx(ctx, tx, prior.ID, input.Kind, input.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// A concurrent writer moved the row out of conflict_pending
			// between our pre-read and the state-guarded UPDATE. Surface
			// as 409 — the resolve attempt lost the race.
			return nil, nil, nil, ErrResolveLinkNotPending
		}
		return nil, nil, nil, fmt.Errorf("resolve meeting_note: %w", err)
	}
	return updated, created, followUps, nil
}

// resolveNoneOfThese implements the action="none_of_these" branch by
// re-running Step 4 logic with the row's persisted participants and a
// freshly-extracted title-match set.
func (s *MeetingNoteService) resolveNoneOfThese(ctx context.Context, tx pgx.Tx, prior *repository.MeetingNote) (*repository.MeetingNote, []CreatedInteraction, []func(context.Context), error) {
	resolvedTagged, err := s.reResolveTagged(ctx, tx, prior.Participants)
	if err != nil {
		return nil, nil, nil, err
	}

	titleStr := ""
	if prior.Title != nil {
		titleStr = *prior.Title
	}
	titleTokens := anarlog.ExtractNameTokens(titleStr)
	titleMatched := make([]resolvedTitle, 0, len(titleTokens))
	titleUnmatched := make([]string, 0, len(titleTokens))
	for _, token := range titleTokens {
		if s.titleMatcher == nil {
			titleUnmatched = append(titleUnmatched, token)
			continue
		}
		match, mErr := s.titleMatcher.MatchTitleToken(ctx, token)
		if mErr != nil {
			return nil, nil, nil, fmt.Errorf("match title token %q: %w", token, mErr)
		}
		if match == nil {
			titleUnmatched = append(titleUnmatched, token)
			continue
		}
		alreadyTagged := false
		for _, r := range resolvedTagged {
			if r.ContactID == match.Contact.ID {
				alreadyTagged = true
				break
			}
		}
		if alreadyTagged {
			continue
		}
		titleMatched = append(titleMatched, resolvedTitle{
			Token:           token,
			NormalizedToken: strings.ToLower(token),
			ContactID:       match.Contact.ID,
		})
	}

	resolvedIDs := make([]uuid.UUID, len(resolvedTagged))
	for i, r := range resolvedTagged {
		resolvedIDs[i] = r.ContactID
	}
	titleMatchedIDs := make([]uuid.UUID, len(titleMatched))
	for i, tm := range titleMatched {
		titleMatchedIDs[i] = tm.ContactID
	}
	newResolvedSetHash, err := computeResolvedSetHash(resolvedIDs, titleMatchedIDs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("compute resolved set hash: %w", err)
	}

	// Force zero candidates so decideLinkage walks the Step 4 path.
	newState, _, _, desired, _ := decideLinkage(
		events.MeetingNoteRecordedPayload{Title: prior.Title, MeetingAt: prior.MeetingAt},
		prior.AnarlogSessionID,
		nil,
		resolvedTagged,
		titleMatched,
	)

	created, followUps, err := s.writeDesiredInteractions(ctx, tx, prior, desired)
	if err != nil {
		return nil, nil, nil, err
	}

	// Discovery upsert for unmatched title tokens. Surfaces the same
	// rows the daemon-side ingest path would have created had the
	// user's "none of these" choice been the original outcome.
	if s.titleDiscovery != nil {
		for _, token := range titleUnmatched {
			if dErr := s.titleDiscovery.UpsertTitleCandidateTx(ctx, tx, prior.AnarlogSessionID, strings.ToLower(token), token); dErr != nil {
				return nil, nil, nil, fmt.Errorf("upsert title candidate %q: %w", token, dErr)
			}
		}
	}

	updated, err := s.meetingWriter.ClearMeetingNoteConflictTx(ctx, tx, prior.ID, newState, newResolvedSetHash)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, nil, nil, ErrResolveLinkNotPending
		}
		return nil, nil, nil, fmt.Errorf("clear meeting_note conflict: %w", err)
	}
	return updated, created, followUps, nil
}

// reResolveTagged maps prior.Participants (anarlog_human UUIDs) into
// the resolvedTag list, deduplicated by contact_id — same recipe as
// handleMeetingNoteRecorded step 3.
func (s *MeetingNoteService) reResolveTagged(ctx context.Context, tx pgx.Tx, participants []string) ([]resolvedTag, error) {
	if s.identityLookup == nil {
		return nil, nil
	}
	out := make([]resolvedTag, 0, len(participants))
	seen := make(map[uuid.UUID]struct{}, len(participants))
	for _, pid := range participants {
		contactID, err := s.identityLookup.FindContactIDByAnarlogHumanIDTx(ctx, tx, pid)
		if err != nil {
			return nil, fmt.Errorf("resolve participant %s: %w", pid, err)
		}
		if contactID == nil {
			continue
		}
		if _, dup := seen[*contactID]; dup {
			continue
		}
		seen[*contactID] = struct{}{}
		out = append(out, resolvedTag{AnarlogID: pid, ContactID: *contactID})
	}
	return out, nil
}

// writeDesiredInteractions persists each desiredInteraction through
// ContactInteractionRecorder.RecordInteractionTx so cadence + follow-up
// state apply correctly. Returns the materialized list for the response
// PLUS the post-commit follow-up closures.
func (s *MeetingNoteService) writeDesiredInteractions(ctx context.Context, tx pgx.Tx, prior *repository.MeetingNote, desired []desiredInteraction) ([]CreatedInteraction, []func(context.Context), error) {
	if s.contactRecorder == nil || len(desired) == 0 {
		return nil, nil, nil
	}
	created := make([]CreatedInteraction, 0, len(desired))
	var followUps []func(context.Context)
	for _, d := range desired {
		sourceRef := d.SourceRef
		req := repository.RecordInteractionRequest{
			ContactID:   d.ContactID,
			Source:      repository.InteractionSourceAnarlogSessions,
			SourceRef:   &sourceRef,
			OccurredAt:  prior.MeetingAt,
			Description: prior.Title,
			Direction:   repository.InteractionDirectionMutual,
		}
		res, err := s.contactRecorder.RecordInteractionTx(ctx, tx, false, req)
		if err != nil {
			return nil, nil, fmt.Errorf("record session interaction: %w", err)
		}
		if res != nil && res.FollowUpFn != nil {
			followUps = append(followUps, res.FollowUpFn)
		}
		if res != nil && res.Interaction != nil {
			created = append(created, CreatedInteraction{
				ID:         res.Interaction.ID,
				ContactID:  res.Interaction.ContactID,
				SourceRef:  d.SourceRef,
				OccurredAt: res.Interaction.OccurredAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
				Direction:  res.Interaction.Direction,
			})
		}
	}
	return created, followUps, nil
}

// fetchCandidateAsLinkageTx reads the target row referenced by a
// snapshot entry and projects it back into a LinkageCandidate so
// stepFiveWalkins can use the kind-aware ImpliedAttendeeSet helper.
// Uses tx-bound reads so the existence check is serializable with the
// subsequent state transition.
func (s *MeetingNoteService) fetchCandidateAsLinkageTx(ctx context.Context, tx pgx.Tx, kind string, id uuid.UUID) (*repository.LinkageCandidate, error) {
	if s.linkageTargets == nil {
		return nil, errors.New("linkage target reader not configured")
	}
	switch kind {
	case repository.LinkedKindEvent:
		evt, err := s.linkageTargets.GetEventByIDTx(ctx, tx, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, ErrResolveLinkTargetMissing
			}
			return nil, fmt.Errorf("read calendar event: %w", err)
		}
		return &repository.LinkageCandidate{
			Kind:               repository.LinkedKindEvent,
			ID:                 evt.ID,
			OccurredAt:         evt.StartTime,
			AttendeeContactIDs: append([]uuid.UUID(nil), evt.MatchedContactIDs...),
		}, nil
	case repository.LinkedKindPhoneCall:
		call, err := s.linkageTargets.GetPhoneCallByIDTx(ctx, tx, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, ErrResolveLinkTargetMissing
			}
			return nil, fmt.Errorf("read phone_call: %w", err)
		}
		var peer *uuid.UUID
		if call.MatchedContactID != nil {
			id := *call.MatchedContactID
			peer = &id
		}
		return &repository.LinkageCandidate{
			Kind:          repository.LinkedKindPhoneCall,
			ID:            call.ID,
			OccurredAt:    call.StartedAt,
			PeerContactID: peer,
		}, nil
	default:
		return nil, ErrResolveLinkIDNotCandidate
	}
}

// snapshotContains is true when the (kind, id) tuple appears in the
// persisted snapshot.
func snapshotContains(snap []repository.ConflictCandidateSummary, kind string, id uuid.UUID) bool {
	for _, s := range snap {
		if s.Kind == kind && s.ID == id {
			return true
		}
	}
	return false
}

// logResolution emits the linkage_resolution structured event so the
// audit trail covers both daemon-driven and user-driven decisions.
func (s *MeetingNoteService) logResolution(result *ResolveLinkResult, input *ResolveLinkInput) {
	if result == nil || result.MeetingNote == nil {
		return
	}
	row := result.MeetingNote
	linkedKindStr := ""
	if row.LinkedKind != nil {
		linkedKindStr = *row.LinkedKind
	}
	linkedIDStr := ""
	if row.LinkedID != nil {
		linkedIDStr = row.LinkedID.String()
	}
	logger.Info().
		Str("event", "linkage_resolution").
		Str("meeting_note_id", row.ID.String()).
		Str("session_id", row.AnarlogSessionID.String()).
		Str("decision", row.LinkageState).
		Str("linked_kind", linkedKindStr).
		Str("linked_id", linkedIDStr).
		Int("interactions_created", len(result.InteractionsCreated)).
		Bool("none_of_these", input == nil).
		Msg("meeting_note conflict resolution")
}

// runFollowUpBestEffort runs a post-commit closure with panic recovery
// + structured error logging. Failures do not bubble up to the
// HTTP-visible path — the linkage_resolution log line and the
// follow-up's own diagnostics suffice for audit.
func runFollowUpBestEffort(ctx context.Context, fn func(context.Context)) {
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Error().
				Interface("panic", r).
				Msg("meeting_note resolve-link follow-up panicked")
		}
	}()
	fn(ctx)
}

// NeedsAttentionCandidatePreview is a sparse projection of a candidate
// row, sufficient for the UI to disambiguate. Sub-fields populated per
// kind ("event" → Title + AttendeeCount; "phone_call" → PeerHandle).
type NeedsAttentionCandidatePreview struct {
	Title         *string `json:"title,omitempty"`
	AttendeeCount *int    `json:"attendee_count,omitempty"`
	PeerHandle    *string `json:"peer_handle,omitempty"`
}

// NeedsAttentionCandidate is a single row in the candidates array on a
// needs-attention response. TargetMissing is true when the snapshot
// references a row that has since been deleted; in that case Preview
// is nil and the entry stays in the response so the UI can render a
// "this candidate no longer exists" hint.
type NeedsAttentionCandidate struct {
	Kind          string                          `json:"kind"`
	ID            uuid.UUID                       `json:"id"`
	OccurredAt    string                          `json:"occurred_at"`
	OverlapCount  int                             `json:"overlap_count"`
	TargetMissing bool                            `json:"target_missing"`
	Preview       *NeedsAttentionCandidatePreview `json:"preview"`
}

// NeedsAttentionItemResponse is the per-row response shape for the
// needs-attention list endpoint.
type NeedsAttentionItemResponse struct {
	ID               uuid.UUID                 `json:"id"`
	AnarlogSessionID uuid.UUID                 `json:"anarlog_session_id"`
	MacHostID        *uuid.UUID                `json:"mac_host_id"`
	Title            *string                   `json:"title"`
	SummaryExcerpt   *string                   `json:"summary_excerpt"`
	MeetingAt        string                    `json:"meeting_at"`
	LinkageState     string                    `json:"linkage_state"`
	Candidates       []NeedsAttentionCandidate `json:"candidates"`
}

// summaryExcerptMaxRunes is the rune budget for summary_excerpt in the
// needs-attention response.
const summaryExcerptMaxRunes = 200

// ListNeedsAttention returns the projection of every live meeting_note
// row in linkage_state conflict_pending or orphan_needs_review,
// optionally scoped to a single mac_host. The candidates array is
// non-nil only for conflict_pending rows.
func (s *MeetingNoteService) ListNeedsAttention(ctx context.Context, hostID *uuid.UUID) ([]NeedsAttentionItemResponse, error) {
	if s.meetingReader == nil {
		return nil, errors.New("MeetingNoteService not configured for list-needs-attention")
	}
	rows, err := s.meetingReader.ListMeetingNotesNeedingAttention(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("list meeting_notes needing attention: %w", err)
	}

	out := make([]NeedsAttentionItemResponse, 0, len(rows))
	for i := range rows {
		row := rows[i]
		item := NeedsAttentionItemResponse{
			ID:               row.ID,
			AnarlogSessionID: row.AnarlogSessionID,
			MacHostID:        row.MacHostID,
			Title:            row.Title,
			MeetingAt:        row.MeetingAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			LinkageState:     row.LinkageState,
		}
		if row.Summary != nil {
			excerpt := truncateRunes(*row.Summary, summaryExcerptMaxRunes)
			item.SummaryExcerpt = &excerpt
		}
		if row.LinkageState == repository.LinkageStateConflictPending && len(row.ConflictCandidates) > 0 {
			var snap []repository.ConflictCandidateSummary
			if uErr := json.Unmarshal(row.ConflictCandidates, &snap); uErr != nil {
				return nil, fmt.Errorf("decode conflict_candidates for meeting_note %s: %w", row.ID, uErr)
			}
			cands, projErr := s.projectCandidates(ctx, snap)
			if projErr != nil {
				return nil, projErr
			}
			item.Candidates = cands
		}
		out = append(out, item)
	}
	return out, nil
}

// projectCandidates enriches each snapshot entry with a preview block
// read from the target row. When the target is missing (ErrNotFound)
// the entry stays with TargetMissing=true and a nil preview; transient
// read errors propagate so the list endpoint can return 500.
func (s *MeetingNoteService) projectCandidates(ctx context.Context, snap []repository.ConflictCandidateSummary) ([]NeedsAttentionCandidate, error) {
	out := make([]NeedsAttentionCandidate, 0, len(snap))
	for _, c := range snap {
		entry := NeedsAttentionCandidate{
			Kind:         c.Kind,
			ID:           c.ID,
			OccurredAt:   c.OccurredAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			OverlapCount: c.OverlapCount,
		}
		preview, missing, err := s.candidatePreview(ctx, c.Kind, c.ID)
		if err != nil {
			return nil, err
		}
		entry.TargetMissing = missing
		entry.Preview = preview
		out = append(out, entry)
	}
	return out, nil
}

// candidatePreview reads the target row to populate the preview block.
// Returns (nil, true) ONLY when the target row has actually been
// deleted (db.ErrNotFound). Any other read error is logged + bubbles
// up; callers degrade to a missing preview but the surface above
// (ListNeedsAttention) propagates the error so transient DB issues
// don't silently masquerade as user-visible "missing target" state.
func (s *MeetingNoteService) candidatePreview(ctx context.Context, kind string, id uuid.UUID) (*NeedsAttentionCandidatePreview, bool, error) {
	if s.linkageTargets == nil {
		return nil, true, nil
	}
	switch kind {
	case repository.LinkedKindEvent:
		evt, err := s.linkageTargets.GetEventByID(ctx, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, true, nil
			}
			return nil, false, fmt.Errorf("read calendar event preview: %w", err)
		}
		count := len(evt.Attendees)
		return &NeedsAttentionCandidatePreview{
			Title:         evt.Title,
			AttendeeCount: &count,
		}, false, nil
	case repository.LinkedKindPhoneCall:
		call, err := s.linkageTargets.GetPhoneCallByID(ctx, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, true, nil
			}
			return nil, false, fmt.Errorf("read phone_call preview: %w", err)
		}
		handle := call.PeerHandle
		return &NeedsAttentionCandidatePreview{
			PeerHandle: &handle,
		}, false, nil
	default:
		return nil, true, nil
	}
}

// truncateRunes returns s truncated to at most max runes. Produces
// valid UTF-8 even when the input contains multi-byte characters.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// ResolveLinkInputFromKind is a constructor convenience: build a
// ResolveLinkInput when the handler has already parsed and validated
// kind + id. Keeps the service-level type from leaking through the
// handler's request-decode path.
func ResolveLinkInputFromKind(kind string, id uuid.UUID) ResolveLinkInput {
	return ResolveLinkInput{Kind: kind, ID: id}
}
