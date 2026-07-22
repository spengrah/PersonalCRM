// Package replay feeds synthetic input (from internal/synthetic/factory) through
// the REAL ingestion pipeline — provider normalization → matching → dedup →
// event bus → consumers → downstream graph — and drains River synchronously so
// the graph is settled on return.
//
// The Harness builds the real pipeline (a live river.Client with real consumer
// workers via the deferred-shim construction order, mirroring the canonical
// setupTestEventBus + main.go aggregate wiring), plus a REVOKED synthetic Mac
// host for the host-only ingest kinds. After each Replay* adapter runs, the
// harness Settles via TWO per-replay-scoped gates (Gate A domain terminal
// predicate + Gate B contact-scoped River-job finalization) and Cleans up via an
// ID-tracked, FK-ordered teardown — never by a DB-wide source/kind value.
//
// All DB access routes through repository sqlc-backed wrappers (no raw SQL).
package replay

import (
	"context"
	"fmt"
	"sync"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

const (
	// defaultSettleTimeout bounds each gate's polling loop. Generous to avoid
	// flakes under CI load; a timeout here names the unmet gate and indicates a
	// real wiring regression, not normal latency.
	//
	// The budget is REAL wall-clock (a context.WithTimeout / time.Timer, which
	// use the runtime monotonic clock), NOT accelerated-time. The settle budget
	// is INFRASTRUCTURE timing — how long to wait for River jobs to finalize —
	// not domain time. Under a high TIME_ACCELERATION an accelerated-time budget
	// of 30s would collapse to a fraction of a real second and spuriously time
	// out the replay-heavy graph, so the wait loops bound real elapsed time.
	defaultSettleTimeout = 30 * time.Second
	settlePollInterval   = 50 * time.Millisecond
)

// realTimeBudget returns a func reporting whether the real-wall-clock budget is
// still open, plus a cancel to release the underlying timer context. It uses
// context.WithTimeout (runtime monotonic clock) so the budget is independent of
// TIME_ACCELERATION — see defaultSettleTimeout. No time.Now() / accelerated call
// in our code; the timer is the time source.
func realTimeBudget(d time.Duration) (open func() bool, cancel func()) {
	tctx, c := context.WithTimeout(context.Background(), d)
	return func() bool { return tctx.Err() == nil }, c
}

// created is the ID ledger the harness/adapters accumulate during a run so
// Cleanup can delete by exact id (never by a DB-wide source/kind value) and
// Gate B can scope to this replay's contacts. eventIDs is the UNION of
// adapter-direct events (by synthetic source/source_id) and contact-scoped
// cascade events (ListEventIdsForContacts), populated through Gate B.
//
// contactIDs + interactionIDs are deleted by exact id (no namespace column);
// the remaining synthetic tables (external_contact, comms_message,
// external_identity, messages_message, calendar_event) all carry a namespace-
// prefixed column, so they are cleaned by prefix rather than tracked id.
type created struct {
	contactIDs      []uuid.UUID
	interactionIDs  []uuid.UUID
	eventIDs        []uuid.UUID
	telegramPeerIDs []int64
	// telegramChatIDs are group chat ids a group replay created a
	// telegram_chat_config row for, tracked so cleanup deletes those rows by id
	// (telegram_chat_config has no namespace column — keyed only by chat id).
	telegramChatIDs []int64
	// contactTaskIDs are todoist contact_task rows the Todoist replay's
	// (globally-scoped) reconcile created — tracked by a before/after diff so
	// cleanup removes exactly them, even on cadence-bearing contacts the replay
	// did not seed.
	contactTaskIDs []uuid.UUID
	// venueNodeIDs are venue node ids the real recorders minted on the replay
	// path (interaction.venue_id → node). They are NOT contacts, so the
	// person-node delete misses them, and their canonical_label is empty so the
	// ns-prefix node delete misses them too — cleanup MUST delete them by id
	// (after the interaction delete, which clears the restrict FK) or the shared
	// DB leaks a venue node per distinct container.
	venueNodeIDs []uuid.UUID
	// signalNodeIDs are the subject node ids a profile seeded a relationship_signal
	// on. relationship_signal has no namespace column + no deleted_at, and its
	// subject_node_id → node FK is NO ACTION, so cleanup MUST delete these rows by
	// the tracked node ids BEFORE the node deletes.
	signalNodeIDs []uuid.UUID
	// softDeletedNodeIDs / mergedLoserNodeIDs / mergedWinnerNodeIDs are person node
	// ids a soft-delete / merge scenario produced, tracked ONLY for the seed-shape
	// invariant assertions (tombstone + assertion re-point checks). Teardown needs no
	// separate step for them: each is a node.id == contact.id that SeedContact already
	// added to contactIDs, so the existing by-id assertion → person_node → contact
	// sweeps remove the tombstoned (soft-deleted / merged-loser) and live (merge
	// winner) rows. The merged_into self-FK is NO ACTION, so the single-statement
	// person_node delete drops a merged pair together without ordering grief.
	softDeletedNodeIDs  []uuid.UUID
	mergedLoserNodeIDs  []uuid.UUID
	mergedWinnerNodeIDs []uuid.UUID
	// directSources is the set of sources the adapters published root events
	// under, so Cleanup can capture no-contact root events that the
	// contact-scoped read misses.
	directSources map[string]struct{}
}

func newCreated() *created {
	return &created{directSources: map[string]struct{}{}}
}

func (c *created) addContact(id uuid.UUID)     { c.contactIDs = append(c.contactIDs, id) }
func (c *created) addInteraction(id uuid.UUID) { c.interactionIDs = append(c.interactionIDs, id) }
func (c *created) addVenueNode(id uuid.UUID)   { c.venueNodeIDs = append(c.venueNodeIDs, id) }
func (c *created) addSignalNode(id uuid.UUID)  { c.signalNodeIDs = append(c.signalNodeIDs, id) }
func (c *created) addSoftDeletedNode(id uuid.UUID) {
	c.softDeletedNodeIDs = append(c.softDeletedNodeIDs, id)
}
func (c *created) addMergedPair(winner, loser uuid.UUID) {
	c.mergedWinnerNodeIDs = append(c.mergedWinnerNodeIDs, winner)
	c.mergedLoserNodeIDs = append(c.mergedLoserNodeIDs, loser)
}
func (c *created) addTelegramPeer(id int64)      { c.telegramPeerIDs = append(c.telegramPeerIDs, id) }
func (c *created) addTelegramChat(id int64)      { c.telegramChatIDs = append(c.telegramChatIDs, id) }
func (c *created) addContactTask(id uuid.UUID)   { c.contactTaskIDs = append(c.contactTaskIDs, id) }
func (c *created) addDirectSource(source string) { c.directSources[source] = struct{}{} }

// Harness holds the live bus + river client + repos + engines + anchor +
// namespace + the seeded revoked Mac host. It is the single place the bus/river
// wiring lives. Build it via NewHarness (test) or NewHarnessWithDB (non-test).
type Harness struct {
	ctx       context.Context
	database  *db.Database
	bus       *events.Bus
	client    *river.Client[pgx.Tx]
	namespace string
	gen       *factory.Generator

	// repos exposed to adapters + tests.
	contactRepo     *repository.ContactRepository
	methodRepo      *repository.ContactMethodRepository
	interactionRepo *repository.InteractionRepository
	commsRepo       *repository.CommsMessageRepository
	externalRepo    *repository.ExternalContactRepository
	telegramRepo    *repository.TelegramMessageRepository
	messagesRepo    *repository.MessagesMessageRepository
	venueRepo       *repository.VenueRepository
	identityService *service.IdentityService
	contactService  *service.ContactService
	cadenceUpdater  *consumer.CadenceUpdater
	support         *repository.SyntheticSupportRepository

	// ingestService is built with the revoked host + hostLiveness=nil.
	ingestService *service.IngestService
	macHostID     uuid.UUID

	// peerMatcher bundles the telegram peer matcher + aggregation engine for
	// the telegram adapter.
	peerMatcher *telegramPeerMatcherDeps

	// groupMaxMembers is the size threshold the MessageHandler is built with,
	// sourced from the test config (TELEGRAM_GROUP_MAX_MEMBERS default) so the
	// harness tracks the production default rather than hard-coding a copy.
	groupMaxMembers int

	// mutualMessageContactID is the contact the two-sided message-direction block
	// (profiles.go) seeded as a reply-bridged telegram MUTUAL. A profile sets it via
	// SetMutualMessageContactID after seeding; the coverage check reads it via
	// MutualMessageContactID to assert the promote-to-mutual collapse. It is NOT a
	// ProfileResult field: contact ids come from uuid_generate_v4() (non-deterministic),
	// so it must stay off the counts-only, determinism-compared result struct.
	mutualMessageContactID uuid.UUID

	// dateFactContactID is the contact the profile seeded the toolkit-authored
	// date-fact birthday on (via ReplayAssertion). A profile sets it via
	// SetDateFactContactID after the seed; the coverage check reads it via
	// DateFactContactID to assert that contact's derived birthday cache — the row
	// that would otherwise be left stranded — is now populated. Like
	// mutualMessageContactID it is NOT a
	// ProfileResult field: the contact id is non-deterministic (uuid_generate_v4),
	// so it stays off the counts-only, determinism-compared result struct.
	dateFactContactID uuid.UUID

	// catalogContactIDs / manualCohortIDs record the ids the visible-task spread
	// (SeedVisibleTaskSpread) touched: every catalog contact (the universe the
	// 0-visible majority is proven over via a LEFT JOIN) and the reserved subset
	// that received manual tasks. A profile sets them via SetCatalogContactIDs /
	// SetManualCohortIDs; the coverage check reads them via CatalogContactIDs /
	// ManualCohortIDs to assert the 0/1/>1 visible-task cohorts subject-scoped.
	// Like the ids above they are NOT ProfileResult fields: contact ids come from
	// uuid_generate_v4() (non-deterministic), so they stay off the counts-only,
	// determinism-compared result struct.
	catalogContactIDs []uuid.UUID
	manualCohortIDs   []uuid.UUID

	created   *created
	createdMu sync.Mutex

	stopped bool
}

// Generator returns the harness's seeded, namespaced generator (live anchor).
func (h *Harness) Generator() *factory.Generator { return h.gen }

// Namespace returns the harness namespace token.
func (h *Harness) Namespace() string { return h.namespace }

// Database exposes the underlying database for tests that need direct repo
// construction beyond the harness's exposed set.
func (h *Harness) Database() *db.Database { return h.database }

// ContactRepo / InteractionRepo / etc. expose the repos tests assert against.
func (h *Harness) ContactRepo() *repository.ContactRepository         { return h.contactRepo }
func (h *Harness) InteractionRepo() *repository.InteractionRepository { return h.interactionRepo }
func (h *Harness) CommsRepo() *repository.CommsMessageRepository      { return h.commsRepo }
func (h *Harness) ExternalContactRepo() *repository.ExternalContactRepository {
	return h.externalRepo
}
func (h *Harness) TelegramRepo() *repository.TelegramMessageRepository { return h.telegramRepo }
func (h *Harness) MessagesRepo() *repository.MessagesMessageRepository { return h.messagesRepo }
func (h *Harness) VenueRepo() *repository.VenueRepository              { return h.venueRepo }

// AssertInteractionHasVenue fetches the interaction by id and verifies it has a
// venue_id pointing at a live venue node. Used by the replay adapters to assert
// the venue-populating recorder paths created a venue. Returns the resolved
// Venue on success.
func (h *Harness) AssertInteractionHasVenue(ctx context.Context, interactionID uuid.UUID) (*repository.Venue, error) {
	interaction, err := h.interactionRepo.GetInteraction(ctx, interactionID)
	if err != nil {
		return nil, fmt.Errorf("get interaction %s: %w", interactionID, err)
	}
	if interaction.VenueID == nil {
		return nil, fmt.Errorf("interaction %s has no venue_id", interactionID)
	}
	venue, err := h.venueRepo.GetVenue(ctx, *interaction.VenueID)
	if err != nil {
		return nil, fmt.Errorf("get venue %s for interaction %s: %w", *interaction.VenueID, interactionID, err)
	}
	return venue, nil
}

// MacHostID is the seeded revoked synthetic host id passed as hostID to ingest.
func (h *Harness) MacHostID() uuid.UUID { return h.macHostID }

// GroupMaxMembers is the size threshold the harness's MessageHandler enforces (a
// group over it is untracked-by-size under status "auto"). Exposed so group tests
// size their tracked/untracked member counts relative to the real threshold
// rather than a magic number.
func (h *Harness) GroupMaxMembers() int { return h.groupMaxMembers }

// track records ids/peers into the ledger (adapter-facing, mutex-guarded).
func (h *Harness) track(fn func(c *created)) {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	fn(h.created)
}

// SeedContact writes a factory ContactSpec through the real contact service and
// records the contact + its methods' identities in the ledger. Returns the
// persisted contact.
func (h *Harness) SeedContact(ctx context.Context, spec factory.ContactSpec) (*repository.Contact, error) {
	methods := make([]service.ContactMethodInput, 0, len(spec.Methods))
	for _, m := range spec.Methods {
		methods = append(methods, service.ContactMethodInput{Type: m.Type, Value: m.Value, IsPrimary: m.IsPrimary})
	}
	contact, _, err := h.contactService.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      spec.FullName,
		Cadence:       spec.Cadence,
		CreatedAt:     spec.CreatedAt,
		LastContacted: spec.LastContacted,
		Birthday:      spec.Birthday,
		Location:      spec.Location,
		HowMet:        spec.HowMet,
	}, methods)
	if err != nil {
		return nil, fmt.Errorf("seed contact: %w", err)
	}
	h.track(func(c *created) { c.addContact(contact.ID) })
	return contact, nil
}

// SeedEntity creates an entity node + its entity subtype row (org / topic / tag)
// in one tx, mirroring the find-or-create-tag-node path in the tag migration but
// always creating (the synthetic pool is a fresh, per-namespace set, so there is
// no find step). The node's canonical_label is namespace-prefixed (from
// gen.Entity), so the teardown's graph_entity_nodes label-prefix sweep removes it
// — no per-id ledger tracking is needed. Returns the created node id so the caller
// can point person→entity edge assertions at it.
func (h *Harness) SeedEntity(ctx context.Context, spec factory.EntitySpec) (uuid.UUID, error) {
	nodeRepo := repository.NewNodeRepository(h.database.Queries)
	entityRepo := repository.NewEntityRepository(h.database.Queries)

	nodeID := uuid.New()
	tx, err := h.database.Pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed entity: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// node + entity must commit together: the entity.node_id FK requires the node,
	// and a partial entity node must never linger on the shared DB.
	if _, err := nodeRepo.CreateNodeTx(ctx, tx, nodeID, repository.NodeTypeEntity, spec.Node.CanonicalLabel); err != nil {
		return uuid.Nil, fmt.Errorf("seed entity: create node: %w", err)
	}
	if _, err := entityRepo.CreateEntityTx(ctx, tx, repository.CreateEntityRequest{
		NodeID:         nodeID,
		Subtype:        spec.Subtype,
		NormalizedName: spec.NormalizedName,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("seed entity: create entity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("seed entity: commit: %w", err)
	}
	return nodeID, nil
}

// SeedExternalContactCandidate writes one UNMATCHED external_contact import
// candidate through the PRODUCTION ExternalContactRepository.Upsert path — the
// SAME write the Google sync providers use for sources the ingest registry does
// NOT allow (gcontacts: google/contacts.go; gmail_correspondence:
// google/gmail_correspondence.go). The Upsert hardcodes match_status='unmatched'
// on insert, so this produces an Imports-queue candidate only (matched/linked
// candidates need the match path and are out of scope here). The field shape
// mirrors each provider so the Imports UI renders the candidate: gcontacts is
// account-scoped with first/last name and an id-shaped source_id; the
// correspondence source keys its source_id on the email and carries the evidence
// metadata the card reads. The ns-prefixed source_id is reclaimed by the
// teardown's external_contact source_id-prefix sweep — no per-id tracking needed.
// Returns the created row id.
func (h *Harness) SeedExternalContactCandidate(ctx context.Context, spec factory.ExternalContactCandidateSpec) (uuid.UUID, error) {
	now := accelerated.GetCurrentTime()
	req := repository.UpsertExternalContactRequest{
		Source:      spec.Source,
		DisplayName: &spec.DisplayName,
		Emails:      []repository.EmailEntry{{Value: spec.Email, Primary: true}},
		SyncedAt:    &now,
	}
	switch spec.Source {
	case google.CorrespondenceSource:
		// The correspondence discoverer keys source_id on the email address and
		// attaches the evidence metadata the card renders (observed names + count).
		req.SourceID = spec.Email
		req.Metadata = map[string]any{
			"display_names_seen": []string{spec.DisplayName},
			"message_count":      1,
		}
	default:
		// Address-book sources (gcontacts) are account-scoped, id-keyed, and carry
		// the name parts the provider extracts from the Person record.
		req.SourceID = spec.EntityID
		req.AccountID = &spec.AccountID
		req.FirstName = &spec.FirstName
		req.LastName = &spec.LastName
	}
	ec, err := h.externalRepo.Upsert(ctx, req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed external contact candidate (%s): %w", spec.Source, err)
	}
	return ec.ID, nil
}

// SeedRelationshipSignal writes one relationship_signal row for a seeded node
// through the PRODUCTION UpsertRelationshipSignal write path (storage-only — SP1
// has no signal generators, so the toolkit direct-writes the projection the way
// SP3 eventually will). subjectNodeID must be a node the harness seeded (a person
// node, node.id == contact.id) so the teardown — which deletes signals by the
// tracked node ids BEFORE the node deletes — cleans it. Idempotent on
// (subjectNodeID, signalKey). asOf is anchor-relative (no time.Now()).
func (h *Harness) SeedRelationshipSignal(ctx context.Context, subjectNodeID uuid.UUID, signalKey string, value float64, asOf time.Time, methodVersion string) error {
	if err := h.support.UpsertRelationshipSignal(ctx, subjectNodeID, signalKey, value, asOf, methodVersion); err != nil {
		return fmt.Errorf("seed relationship signal: %w", err)
	}
	h.track(func(c *created) { c.addSignalNode(subjectNodeID) })
	return nil
}

// SeedSoftDeletedContact seeds a contact (with one assertion on its person node),
// then routes it through the PRODUCTION ContactService.DeleteContact path so the
// person node (node.id == contact.id) is tombstoned: the assertion drops from LIVE
// graph reads while remaining in the assertion table. The contact id is already in
// the cleanup ledger (SeedContact tracked it) and the assertion rides the node, so
// the existing by-id assertion → node → contact teardown sweeps remove the whole
// set — no extra cleanup step. The node id is tracked separately ONLY so the
// coverage check can assert the tombstone + retained-vs-live-assertion invariant.
// Returns the soft-deleted contact id.
func (h *Harness) SeedSoftDeletedContact(ctx context.Context, spec factory.ContactSpec, fact factory.AssertionSpec) (uuid.UUID, error) {
	contact, err := h.SeedContact(ctx, spec)
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed soft-deleted contact: %w", err)
	}
	if _, err := h.ReplayAssertion(ctx, contact.ID, fact); err != nil {
		return uuid.Nil, fmt.Errorf("seed soft-deleted contact assertion: %w", err)
	}
	if err := h.contactService.DeleteContact(ctx, contact.ID); err != nil {
		return uuid.Nil, fmt.Errorf("soft-delete contact %s: %w", contact.ID, err)
	}
	h.track(func(c *created) { c.addSoftDeletedNode(contact.ID) })
	return contact.ID, nil
}

// SeedMergedContact seeds TWO contacts — a winner + a loser, each with one assertion
// on its person node — then merges the loser INTO the winner via the PRODUCTION
// ContactService.MergeContacts path. That re-points the loser's assertions onto the
// winner (MergeAssertionsTx), tombstones the loser node (merged_into=winner +
// deleted_at via SetNodeMergedInto), and soft-deletes the loser contact. Give the
// winner + loser DISTINCT predicates so the re-pointed loser fact lands beside the
// winner's own without single-cardinality supersession (the merge internals are
// exhaustively covered elsewhere; this is a SEED, not a re-derivation). Both contact
// ids are already in the cleanup ledger (SeedContact); the merged_into self-FK is NO
// ACTION, so the existing single-statement by-id person_node sweep drops the pair
// together at teardown. The winner/loser node ids are tracked separately ONLY for
// the coverage check's tombstone + re-point invariants. Returns (winnerID, loserID).
func (h *Harness) SeedMergedContact(ctx context.Context, winnerSpec, loserSpec factory.ContactSpec, winnerFact, loserFact factory.AssertionSpec) (winnerID, loserID uuid.UUID, err error) {
	winner, err := h.SeedContact(ctx, winnerSpec)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("seed merge winner: %w", err)
	}
	if _, err := h.ReplayAssertion(ctx, winner.ID, winnerFact); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("seed merge winner assertion: %w", err)
	}
	loser, err := h.SeedContact(ctx, loserSpec)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("seed merge loser: %w", err)
	}
	if _, err := h.ReplayAssertion(ctx, loser.ID, loserFact); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("seed merge loser assertion: %w", err)
	}
	if _, err := h.contactService.MergeContacts(ctx, service.MergeContactsRequest{
		SourceContactID: loser.ID,  // source = archived loser
		TargetContactID: winner.ID, // target = surviving winner
	}); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("merge contact %s into %s: %w", loser.ID, winner.ID, err)
	}
	h.track(func(c *created) { c.addMergedPair(winner.ID, loser.ID) })
	return winner.ID, loser.ID, nil
}

// SeedNote attaches a notepad note to a seeded contact via the EXISTING
// NoteRepository (orchestration over an existing repo, not new machinery). The
// contact must be one the harness seeded (so the teardown's note step — delete
// by tracked contact id — cleans it). Gives the catalog its "≥1 contact with
// notes" bucket. The body is namespace-tagged so it is identifiable.
func (h *Harness) SeedNote(ctx context.Context, contactID uuid.UUID, body string) error {
	noteRepo := repository.NewNoteRepository(h.database.Queries)
	if _, err := noteRepo.CreateNotepad(ctx, contactID, h.gen.Prefix()+body); err != nil {
		return fmt.Errorf("seed note: %w", err)
	}
	return nil
}

// SeedOrphanMeetingNote inserts a single orphan_needs_review meeting_note row
// against the harness's seeded synthetic mac_host (the Imports Interactions
// "orphan" surface). It uses the EXISTING MeetingNoteRepository — not a new
// replay adapter — so it is orchestration over an existing repo, mirroring the
// /seed/meeting-notes route. The session id is a fresh random UUID; cleanup is by
// the harness's host id (the teardown's meeting_note step), so no namespace
// prefix is needed. Returns the created session id.
//
// Only the orphan state is produced here: conflict_pending needs a well-formed
// conflict_candidates snapshot referencing real events, which has no toolkit
// producer (a documented, deferred coverage gap).
func (h *Harness) SeedOrphanMeetingNote(ctx context.Context, title, summary string) (uuid.UUID, error) {
	tx, err := h.database.Pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed orphan meeting note: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	sessionID := uuid.New()
	hostID := h.macHostID
	params := repository.InsertMeetingNoteParams{
		AnarlogSessionID: sessionID,
		MacHostID:        &hostID,
		LinkageState:     repository.LinkageStateOrphanNeedsReview,
		MeetingAt:        accelerated.GetCurrentTime(),
		// An orphan carries no conflict_candidates snapshot; empty hashes are
		// accepted by the column CHECK.
		InputHash:       "",
		ResolvedSetHash: "",
	}
	if title != "" {
		params.Title = &title
	}
	if summary != "" {
		params.Summary = &summary
	}
	meetingNoteRepo := repository.NewMeetingNoteRepository(h.database.Queries)
	if _, err := meetingNoteRepo.InsertMeetingNoteTx(ctx, tx, params); err != nil {
		return uuid.Nil, fmt.Errorf("seed orphan meeting note: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("seed orphan meeting note: commit: %w", err)
	}
	return sessionID, nil
}

// --- Settle ----------------------------------------------------------------

// gateA is a per-replay domain terminal predicate: a sqlc-backed read scoped to
// THIS replay's exact identifiers that returns true once the replay's domain
// rows have landed. Adapters supply it.
type gateA func(ctx context.Context) (bool, error)

// Settle runs Gate A then Gate B, both per-replay-scoped, then populates
// created.eventIDs (the UNION feeding cleanup). It errors loudly naming the
// unmet gate. Cleanup runs ONLY after Settle returns successfully.
//
// source is the aggregation source for Gate B's messaging-aggregate companion
// (e.g. "messages"/"gchat"); pass "" for sources with no aggregate jobs.
func (h *Harness) Settle(ctx context.Context, predicate gateA, source string) error {
	// Gate A — domain terminal predicate.
	if err := h.waitGateA(ctx, predicate); err != nil {
		return err
	}
	// Gate B — this replay's River jobs finalized.
	if err := h.waitGateB(ctx, source); err != nil {
		return err
	}
	// Populate the cleanup event-id union now that the cascade has settled.
	return h.captureEventIDs(ctx)
}

func (h *Harness) waitGateA(ctx context.Context, predicate gateA) error {
	if predicate == nil {
		return nil
	}
	open, cancel := realTimeBudget(defaultSettleTimeout)
	defer cancel()
	var lastErr error
	for open() {
		ok, err := predicate(ctx)
		if err == nil && ok {
			return nil
		}
		lastErr = err
		time.Sleep(settlePollInterval)
	}
	return fmt.Errorf("synthetic settle: Gate A (domain terminal predicate) not met within %s: %v", defaultSettleTimeout, lastErr)
}

func (h *Harness) waitGateB(ctx context.Context, source string) error {
	contactIDs := h.snapshotContactIDs()
	open, cancel := realTimeBudget(defaultSettleTimeout)
	defer cancel()
	var lastEventJobs, lastAggJobs int64
	var lastErr error
	for open() {
		eventJobs, err := h.support.CountUnfinalizedRiverJobsForEventsByContacts(ctx, contactIDs)
		if err != nil {
			lastErr = err
			time.Sleep(settlePollInterval)
			continue
		}
		var aggJobs int64
		if source != "" {
			aggJobs, err = h.support.CountUnfinalizedMessagingAggregateJobs(ctx, contactIDs, source)
			if err != nil {
				lastErr = err
				time.Sleep(settlePollInterval)
				continue
			}
		}
		lastEventJobs, lastAggJobs = eventJobs, aggJobs
		if eventJobs == 0 && aggJobs == 0 {
			return nil
		}
		time.Sleep(settlePollInterval)
	}
	return fmt.Errorf("synthetic settle: Gate B (River-job finalization) not met within %s: event_jobs=%d agg_jobs=%d err=%v",
		defaultSettleTimeout, lastEventJobs, lastAggJobs, lastErr)
}

// captureEventIDs grows created.eventIDs to the UNION of contact-scoped cascade
// events and adapter-direct root events (by synthetic source/source_id prefix).
func (h *Harness) captureEventIDs(ctx context.Context) error {
	contactIDs := h.snapshotContactIDs()
	seen := map[uuid.UUID]struct{}{}
	var union []uuid.UUID

	add := func(ids []uuid.UUID) {
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			union = append(union, id)
		}
	}

	cascade, err := h.support.ListEventIdsForContacts(ctx, contactIDs)
	if err != nil {
		return fmt.Errorf("capture cascade event ids: %w", err)
	}
	add(cascade)

	h.createdMu.Lock()
	sources := make([]string, 0, len(h.created.directSources))
	for s := range h.created.directSources {
		sources = append(sources, s)
	}
	prefix := h.gen.Prefix()
	existing := append([]uuid.UUID(nil), h.created.eventIDs...)
	h.createdMu.Unlock()
	add(existing)

	for _, src := range sources {
		roots, err := h.support.ListEventIdsBySourceAndSourceIDPrefix(ctx, src, prefix)
		if err != nil {
			return fmt.Errorf("capture root event ids for source %q: %w", src, err)
		}
		add(roots)
	}

	h.createdMu.Lock()
	h.created.eventIDs = union
	h.createdMu.Unlock()
	return nil
}

func (h *Harness) snapshotContactIDs() []uuid.UUID {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	return append([]uuid.UUID(nil), h.created.contactIDs...)
}

func (h *Harness) snapshotVenueNodeIDs() []uuid.UUID {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	return append([]uuid.UUID(nil), h.created.venueNodeIDs...)
}

func (h *Harness) snapshotSignalNodeIDs() []uuid.UUID {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	return append([]uuid.UUID(nil), h.created.signalNodeIDs...)
}

// SoftDeletedNodeIDs / MergedLoserNodeIDs / MergedWinnerNodeIDs return THIS run's
// tracked soft-delete / merge person node ids so a coverage test can scope its
// tombstone + assertion-re-point invariant assertions to its own namespace's nodes
// on the shared DB. Snapshots (mutex-guarded copies), so a caller can range them
// without holding the ledger lock.
func (h *Harness) SoftDeletedNodeIDs() []uuid.UUID {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	return append([]uuid.UUID(nil), h.created.softDeletedNodeIDs...)
}

func (h *Harness) MergedLoserNodeIDs() []uuid.UUID {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	return append([]uuid.UUID(nil), h.created.mergedLoserNodeIDs...)
}

func (h *Harness) MergedWinnerNodeIDs() []uuid.UUID {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	return append([]uuid.UUID(nil), h.created.mergedWinnerNodeIDs...)
}

// SetMutualMessageContactID records the contact the two-sided message block seeded
// as a reply-bridged telegram mutual, so the coverage check can resolve it via
// MutualMessageContactID. Called by the seeding block (package synthetic) after it
// seeds the mutual pair — hence exported. Guarded by the ledger mutex for parity
// with the other tracked ids.
func (h *Harness) SetMutualMessageContactID(id uuid.UUID) {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	h.mutualMessageContactID = id
}

// MutualMessageContactID returns the contact the two-sided message block seeded as
// a reply-bridged telegram mutual (uuid.Nil if the block did not run — e.g. a
// profile with it disabled). Read by the coverage check to assert the promote.
func (h *Harness) MutualMessageContactID() uuid.UUID {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	return h.mutualMessageContactID
}

// SetDateFactContactID records the contact the profile seeded the toolkit
// date-fact birthday on, so the coverage check can resolve it via
// DateFactContactID. Called by the seeding block (package synthetic) after the
// date-fact ReplayAssertion — hence exported. Guarded by the ledger mutex for
// parity with the other tracked ids.
func (h *Harness) SetDateFactContactID(id uuid.UUID) {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	h.dateFactContactID = id
}

// DateFactContactID returns the contact the profile seeded the toolkit date-fact
// birthday on (uuid.Nil if the block did not run). Read by the coverage check to
// assert that contact's derived birthday cache is populated (the target row that
// exercises the cutover-predicate refresh).
func (h *Harness) DateFactContactID() uuid.UUID {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	return h.dateFactContactID
}

// SetCatalogContactIDs records the full catalog contact id set the visible-task
// spread ran over, so the coverage check can prove the 0-visible majority via a
// LEFT JOIN of the catalog universe against its visible tasks (grouping task rows
// alone can't produce the zero-task contacts). Called by the seeding block after
// the catalog is populated — hence exported. Guarded by the ledger mutex for
// parity with the other tracked ids.
func (h *Harness) SetCatalogContactIDs(ids []uuid.UUID) {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	h.catalogContactIDs = append([]uuid.UUID(nil), ids...)
}

// CatalogContactIDs returns the catalog contact ids the visible-task spread ran
// over (nil if the spread did not run). Read by the coverage check to scope the
// 0/1/>1 visible-task cohort assertions to the catalog universe.
func (h *Harness) CatalogContactIDs() []uuid.UUID {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	return append([]uuid.UUID(nil), h.catalogContactIDs...)
}

// SetManualCohortIDs records the reserved catalog contacts the visible-task
// spread gave manual tasks (the 1-visible + >1-visible cohorts). Called by
// SeedVisibleTaskSpread — hence exported. Guarded by the ledger mutex.
func (h *Harness) SetManualCohortIDs(ids []uuid.UUID) {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	h.manualCohortIDs = append([]uuid.UUID(nil), ids...)
}

// ManualCohortIDs returns the reserved catalog contacts that received manual
// tasks (nil if the spread did not run). Read by the coverage check to assert
// the manual cohorts subject-scoped without putting UUIDs in ProfileResult.
func (h *Harness) ManualCohortIDs() []uuid.UUID {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	return append([]uuid.UUID(nil), h.manualCohortIDs...)
}

// gateBClear reports whether Gate B has reached zero for this replay's contacts.
// Used by the teardown closure to gate the entire cleanup.
func (h *Harness) gateBClear(ctx context.Context, source string) bool {
	contactIDs := h.snapshotContactIDs()
	eventJobs, err := h.support.CountUnfinalizedRiverJobsForEventsByContacts(ctx, contactIDs)
	if err != nil || eventJobs != 0 {
		return false
	}
	if source != "" {
		aggJobs, err := h.support.CountUnfinalizedMessagingAggregateJobs(ctx, contactIDs, source)
		if err != nil || aggJobs != 0 {
			return false
		}
	}
	return true
}
