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
	"strconv"
	"sync"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/anarlog"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/matching"
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

// Stopwatch measures REAL elapsed wall-clock time. It is the single audited
// real-clock site in the synthetic toolkit: seed/reseed duration is
// INFRASTRUCTURE timing (how long an operation took), not domain time, so
// accelerated.GetCurrentTime() would report a duration scaled by
// TIME_ACCELERATION — a fictional number, useless for the one purpose the
// measurement has. This is the same judgment the settle budgets already make
// (see defaultSettleTimeout / realTimeBudget), and the same class as the request
// latency measurement in internal/api/middleware.go, which .golangci.yml names
// among the sanctioned //nolint:forbidigo exceptions.
//
// Exported because both package synthetic (whole-run + per-phase timing) and
// this package (settle accounting) need it, and synthetic imports replay — the
// reverse import would cycle.
type Stopwatch struct {
	start time.Time
}

// NewStopwatch starts a stopwatch at the current real wall-clock instant.
func NewStopwatch() *Stopwatch {
	//nolint:forbidigo // Wall-clock time for reseed/settle duration measurement, not domain time (see Stopwatch).
	return &Stopwatch{start: time.Now()}
}

// Elapsed reports the real time since the stopwatch was started. time.Since uses
// the monotonic reading captured at start, so it is immune to wall-clock jumps.
func (s *Stopwatch) Elapsed() time.Duration { return time.Since(s.start) }

// SettleTimings is the harness's cumulative settle accounting: how many Settle
// calls a run made, how long each gate waited, and — the falsifiability
// discriminator — how much of that wait was POLLING versus genuine worker/DB
// throughput. A gate satisfied on its first check cost ~one query; one that
// polled many times cost real latency that batching can remove.
//
// Counts and durations only — no PII. Excluded from determinism equality
// (durations are wall-clock and never equal across runs).
type SettleTimings struct {
	// Calls is the number of Settle invocations.
	Calls int
	// GateAWait / GateBWait / CaptureWait are the cumulative time spent in the
	// domain terminal predicate, the River-job finalization gate, and the
	// full-union event-id rebuild. GateA/GateB nest inside the caller's phase
	// timer, and Capture follows them within the same Settle.
	GateAWait   time.Duration
	GateBWait   time.Duration
	CaptureWait time.Duration
	// GateACalls is the number of waitGateA invocations that actually evaluated a
	// predicate (an adapter may pass nil). It is the denominator of the
	// inline-hit rate — Calls would overcount, since a nil-predicate Settle
	// evaluates nothing.
	GateACalls int
	// GateAPolls is the total number of predicate evaluations across all gate-A
	// waits; GateAInlineHits is how many of those waits were satisfied by the
	// FIRST evaluation, before any sleep. A high inline-hit rate means gate A
	// cost ~one query per Settle; a low one means real polling latency.
	GateAPolls      int
	GateAInlineHits int
	// GateBPolls is the total number of gate-B count-query iterations. Divided by
	// Calls it gives the average polls per Settle: ~1 means no polling overhead,
	// a large value means the reseed is waiting on River workers.
	GateBPolls int
}

// recordSettleCall / recordGateA / recordGateB / recordCapture accumulate the
// settle accounting. They take a DEDICATED mutex rather than the ledger's
// createdMu: captureEventIDs already takes createdMu inside its own body, so
// reusing it here would risk a re-entrant lock the moment recording moves inside
// a locked region. Same discipline (mutex-guarded accumulator), separate lock.
func (h *Harness) recordSettleCall() {
	h.settleMu.Lock()
	defer h.settleMu.Unlock()
	h.settle.Calls++
}

func (h *Harness) recordGateA(wait time.Duration, polls int, inlineHit bool) {
	h.settleMu.Lock()
	defer h.settleMu.Unlock()
	h.settle.GateACalls++
	h.settle.GateAWait += wait
	h.settle.GateAPolls += polls
	if inlineHit {
		h.settle.GateAInlineHits++
	}
}

func (h *Harness) recordGateB(wait time.Duration, polls int) {
	h.settleMu.Lock()
	defer h.settleMu.Unlock()
	h.settle.GateBWait += wait
	h.settle.GateBPolls += polls
}

func (h *Harness) recordCapture(wait time.Duration) {
	h.settleMu.Lock()
	defer h.settleMu.Unlock()
	h.settle.CaptureWait += wait
}

// SettleStats returns a snapshot of the run's cumulative settle accounting.
// Exported so package synthetic can fold it into the profile result; safe to
// call at any point, including from an error path, so a failed reseed still
// reports whatever settling had completed.
func (h *Harness) SettleStats() SettleTimings {
	h.settleMu.Lock()
	defer h.settleMu.Unlock()
	return h.settle
}

// CreatedContactIDs returns the exact contact IDs SeedContact has created in
// this harness. The ledger is owned by SeedContact itself, so a higher-level
// world builder can verify its caller-reported manifest without trusting that
// caller to report every row it created.
func (h *Harness) CreatedContactIDs() []uuid.UUID {
	return h.snapshotContactIDs()
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
	// externalContactIDs are the import-candidate rows the Seed* primitives wrote.
	// Two of the sources they cover have a production source_id that carries no
	// namespace-prefixed string at all — a decimal telegram peer id, a SHA-256
	// (token ‖ session) digest — so the ns-prefix delete cannot see them, and the
	// telegram-peer delete only reaches peers a MESSAGE replay tracked. Without
	// this ledger a failed run's teardown would drop the namespace's ownership
	// records, leave those rows standing, and still report the namespace clean.
	externalContactIDs []uuid.UUID
	// directSources is the set of sources the adapters published root events
	// under, so Cleanup can capture no-contact root events that the
	// contact-scoped read misses.
	directSources map[string]struct{}
}

func newCreated() *created {
	return &created{directSources: map[string]struct{}{}}
}

func (c *created) addContact(id uuid.UUID)       { c.contactIDs = append(c.contactIDs, id) }
func (c *created) addInteraction(id uuid.UUID)   { c.interactionIDs = append(c.interactionIDs, id) }
func (c *created) addVenueNode(id uuid.UUID)     { c.venueNodeIDs = append(c.venueNodeIDs, id) }
func (c *created) addSignalNode(id uuid.UUID)    { c.signalNodeIDs = append(c.signalNodeIDs, id) }
func (c *created) addTelegramPeer(id int64)      { c.telegramPeerIDs = append(c.telegramPeerIDs, id) }
func (c *created) addTelegramChat(id int64)      { c.telegramChatIDs = append(c.telegramChatIDs, id) }
func (c *created) addContactTask(id uuid.UUID)   { c.contactTaskIDs = append(c.contactTaskIDs, id) }
func (c *created) addDirectSource(source string) { c.directSources[source] = struct{}{} }
func (c *created) addExternalContact(id uuid.UUID) {
	c.externalContactIDs = append(c.externalContactIDs, id)
}

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

	// manualCohortIDs records the reserved contacts the visible-task spread
	// (SeedVisibleTaskSpread) gave manual tasks. The spread sets it via
	// SetManualCohortIDs; a caller reads it via ManualCohortIDs to assert the
	// visible-task cohorts subject-scoped. It is NOT a ProfileResult field: contact
	// ids come from uuid_generate_v4() (non-deterministic), so it stays off the
	// counts-only, determinism-compared result struct.
	manualCohortIDs []uuid.UUID

	created   *created
	createdMu sync.Mutex

	// settle is the cumulative settle accounting (call count, per-gate wait, poll
	// counts) a profile run reads via SettleStats. Guarded by its OWN mutex — see
	// recordSettleCall.
	settle   SettleTimings
	settleMu sync.Mutex

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
		FullName:  spec.FullName,
		Cadence:   spec.Cadence,
		CreatedAt: spec.CreatedAt,
		Birthday:  spec.Birthday,
		Location:  spec.Location,
		HowMet:    spec.HowMet,
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

// CorrespondenceEvidence is the co-occurrence evidence a gmail_correspondence
// candidate carries: how many messages the discoverer aggregated, and the known
// contact it most often co-appeared with.
//
// CoOccurringContactID must be a REAL contact uuid. The production builder emits
// the co_occurring_contact object only when it found a co-occurrence, and its
// first act is to write that contact's uuid — the NAME is the optional half,
// dropped when the lookup fails (google/gmail_correspondence.go buildEvidence). So
// an empty id is a shape the discoverer never writes, and seeding one would leave
// every reader of that id exercised against a value it cannot receive.
type CorrespondenceEvidence struct {
	MessageCount         int
	CoOccurringContactID string
	CoOccurringName      string
}

// SeedExternalContactCandidate writes one UNMATCHED external_contact import
// candidate through the PRODUCTION ExternalContactRepository.Upsert path — the
// SAME write the Google sync providers use for the sources the ingest registry does
// NOT allow (gcontacts: google/contacts.go; gmail_correspondence:
// google/gmail_correspondence.go; gcal_attendee: google/calendar.go). The Upsert
// hardcodes match_status='unmatched' on insert, so this produces an Imports-queue
// candidate only (matched/linked candidates need the match path and are out of
// scope here). The field shape mirrors each provider so the Imports UI renders the
// candidate: gcontacts is account-scoped with first/last name and an id-shaped
// source_id; gcal_attendee is account-scoped and keys its source_id on the
// normalized attendee email; the correspondence source keys its source_id on the
// email and carries the evidence metadata the card reads.
//
// The ns-prefixed source_id is reclaimed by the teardown's external_contact
// source_id-prefix sweep; the declared-seeding caller ALSO records namespace
// ownership by row id, which is what covers the sources whose production source_id
// carries no prefix. Returns the created row id.
func (h *Harness) SeedExternalContactCandidate(
	ctx context.Context,
	spec factory.ExternalContactCandidateSpec,
	evidence *CorrespondenceEvidence,
) (uuid.UUID, error) {
	now := accelerated.GetCurrentTime()
	emails := make([]repository.EmailEntry, 0, len(spec.Emails))
	for i, address := range spec.Emails {
		emails = append(emails, repository.EmailEntry{Value: address, Type: "personal", Primary: i == 0})
	}
	phones := make([]repository.PhoneEntry, 0, len(spec.Phones))
	for i, number := range spec.Phones {
		phones = append(phones, repository.PhoneEntry{Value: number, Type: "mobile", Primary: i == 0})
	}
	req := repository.UpsertExternalContactRequest{
		Source:      spec.Source,
		DisplayName: &spec.DisplayName,
		Emails:      emails,
		Phones:      phones,
		SyncedAt:    &now,
	}
	switch spec.Source {
	case google.CorrespondenceSource:
		// The correspondence discoverer keys source_id on the email address and
		// attaches the evidence metadata the card renders (observed names + count).
		req.SourceID = spec.Emails[0]
		metadata := map[string]any{
			"display_names_seen": []string{spec.DisplayName},
			"message_count":      1,
		}
		if evidence != nil {
			metadata["message_count"] = evidence.MessageCount
			metadata["co_occurring_contact"] = map[string]any{
				"id":   evidence.CoOccurringContactID,
				"name": evidence.CoOccurringName,
			}
		}
		req.Metadata = metadata
	case google.CalendarAttendeeSource:
		// The calendar provider dedupes attendees on the NORMALIZED email, and
		// attaches the meeting context the card's "From: <title>" badge reads. All
		// FOUR keys, because the provider writes all four unconditionally — and
		// meeting_link is the one the card branches on to decide whether that badge
		// is a link or plain text, so omitting it would leave the link branch
		// unreachable from any declared fixture.
		req.SourceID = matching.NormalizeEmail(spec.Emails[0])
		req.AccountID = &spec.AccountID
		req.Metadata = map[string]any{
			"meeting_title": spec.DisplayName + " sync",
			"meeting_date":  now.Format(time.RFC3339),
			"meeting_link":  "https://www.google.com/calendar/event?eid=" + spec.EntityID,
			"discovered_at": now.Format(time.RFC3339),
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
	h.track(func(c *created) { c.addExternalContact(ec.ID) })
	return ec.ID, nil
}

// SeedTelegramDiscoveryCandidate writes one telegram discovery candidate through
// the PRODUCTION ExternalContactRepository.UpsertTelegramDiscoveryCandidate path —
// the dedicated upsert PeerMatcher uses (telegram/matcher.go), which merges
// metadata and never clears a captured name. The source_id keeps the matcher's own
// recipe (the decimal peer user id), so the row is exactly what a discovery pass
// would have produced and carries no namespace-prefixed string; the caller records
// namespace ownership by row id. Returns the created row id.
func (h *Harness) SeedTelegramDiscoveryCandidate(ctx context.Context, spec factory.TelegramDiscoveryCandidateSpec) (uuid.UUID, string, error) {
	now := accelerated.GetCurrentTime()
	metadata := map[string]any{
		"message_count":  spec.MessageCount,
		"outbound_count": 0,
		"inbound_count":  spec.MessageCount,
	}
	if !spec.LastMessageAt.IsZero() {
		// The matcher's own layout, which is NOT RFC3339 — it has no offset and a
		// literal Z. A fixture that reformatted this would store a string the
		// matcher never writes.
		metadata["last_message_at"] = spec.LastMessageAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if spec.Username != "" {
		metadata["username"] = spec.Username
	}
	req := repository.UpsertTelegramDiscoveryCandidateRequest{
		SourceID:    strconv.FormatInt(spec.PeerUserID, 10),
		DisplayName: nilIfBlank(spec.DisplayName),
		FirstName:   nilIfBlank(spec.FirstName),
		LastName:    nilIfBlank(spec.LastName),
		Metadata:    metadata,
		SyncedAt:    &now,
	}
	ec, err := h.externalRepo.UpsertTelegramDiscoveryCandidate(ctx, req)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("seed telegram discovery candidate: %w", err)
	}
	h.track(func(c *created) { c.addExternalContact(ec.ID) })
	return ec.ID, derefOrEmpty(ec.DisplayName), nil
}

// SeedTitleCandidate writes ONE anarlog_title weak candidate through the
// PRODUCTION anarlog.DiscoveryWriter — orchestration over the real writer, so the
// row keeps its SHA-256 (token ‖ session) source_id and the writer's own
// title-casing of the display token. Each call mints a fresh session uuid, so two
// calls sharing a token produce one grouped candidate with evidence count two.
// Returns the created row id and the display token AS STORED — the writer
// title-cases it, so re-deriving that casing in a caller would be a second copy of
// production logic that could silently disagree with it.
func (h *Harness) SeedTitleCandidate(ctx context.Context, spec factory.AnarlogTitleCandidateSpec) (uuid.UUID, string, error) {
	sessionID := uuid.New()
	tx, err := h.database.Pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("seed title candidate: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	writer := anarlog.NewDiscoveryWriter(h.externalRepo)
	if err := writer.UpsertTitleCandidateTx(ctx, tx, sessionID, spec.NormalizedToken, spec.DisplayToken); err != nil {
		return uuid.Nil, "", fmt.Errorf("seed title candidate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, "", fmt.Errorf("seed title candidate: commit: %w", err)
	}

	ec, err := h.externalRepo.GetBySource(ctx, anarlogTitleSource, anarlog.ComputeAnarlogTitleSourceIDForTest(spec.NormalizedToken, sessionID), nil)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("seed title candidate: read back: %w", err)
	}
	if ec == nil {
		return uuid.Nil, "", fmt.Errorf("seed title candidate: the writer reported success but no row exists for token %q", spec.NormalizedToken)
	}
	h.track(func(c *created) { c.addExternalContact(ec.ID) })
	return ec.ID, derefOrEmpty(ec.DisplayName), nil
}

// anarlogTitleSource is the source the discovery writer stamps on its rows.
const anarlogTitleSource = "anarlog_title"

// SeedMethodSuggestion turns an ALREADY-SEEDED contact into the method-suggestion
// surface: a LINKED `imported` address-book row carrying one pending method the
// contact does not have yet. It mirrors the production reconcile outcome by
// composing the same three repository writes the reconciler ends at — upsert the
// address-book row, link it to the contact as imported, then store the pending
// suggestion set. Returns the external row id and the pending email.
func (h *Harness) SeedMethodSuggestion(
	ctx context.Context,
	contactID uuid.UUID,
	spec factory.ExternalContactCandidateSpec,
	displayName string,
) (uuid.UUID, string, error) {
	now := accelerated.GetCurrentTime()
	pendingEmail := spec.Emails[0]
	external, err := h.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      spec.Source,
		SourceID:    spec.EntityID,
		AccountID:   &spec.AccountID,
		DisplayName: &displayName,
		Emails:      []repository.EmailEntry{{Value: pendingEmail, Type: "personal", Primary: true}},
		SyncedAt:    &now,
	})
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("seed method suggestion: upsert external: %w", err)
	}
	// Tracked BEFORE the two writes below: they can fail with the row already
	// created, and the failure-path teardown has to be able to find it.
	h.track(func(c *created) { c.addExternalContact(external.ID) })
	if _, err := h.externalRepo.UpdateMatch(ctx, external.ID, &contactID, repository.MatchStatusImported); err != nil {
		return uuid.Nil, "", fmt.Errorf("seed method suggestion: link external: %w", err)
	}
	pending := []repository.PendingMethodSuggestion{{
		Type:  "email",
		Value: repository.NormalizeContactMethodValue("email", pendingEmail),
	}}
	if _, err := h.externalRepo.SetMethodSuggestions(ctx, external.ID, pending); err != nil {
		return uuid.Nil, "", fmt.Errorf("seed method suggestion: set pending: %w", err)
	}
	return external.ID, pendingEmail, nil
}

// nilIfBlank maps "" to nil so a blanked name field lands NULL rather than an
// empty string. The unresolved-peer predicate treats both as absent, but the
// production matcher normalizes empty peer fields to nil for exactly this reason
// and the seed must not store a shape it would not.
func nilIfBlank(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// derefOrEmpty reads a nullable stored string, so a manifest reports what the
// column actually holds rather than a value the caller hoped for.
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
// set — no extra cleanup step. Returns the soft-deleted contact id.
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
// together at teardown. Returns (winnerID, loserID).
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
	return winner.ID, loser.ID, nil
}

// SoftDeleteSeededContact routes an ALREADY-SEEDED contact through the
// PRODUCTION ContactService.DeleteContact path, tombstoning it and its person
// node exactly as SeedSoftDeletedContact does for a fresh one.
//
// The difference from SeedSoftDeletedContact is the whole point: this one takes
// an id, so the contact can be given children (notes, interactions, tasks)
// BEFORE it is deleted. "Soft-deleted parent with live children" is not
// reachable through a primitive that seeds and deletes in one call.
func (h *Harness) SoftDeleteSeededContact(ctx context.Context, contactID uuid.UUID) error {
	if err := h.contactService.DeleteContact(ctx, contactID); err != nil {
		return fmt.Errorf("soft-delete seeded contact %s: %w", contactID, err)
	}
	return nil
}

// MergeSeededContacts merges an ALREADY-SEEDED loser INTO an already-seeded
// winner through the PRODUCTION ContactService.MergeContacts path — the same
// re-point / tombstone / soft-delete effects SeedMergedContact produces.
//
// The difference from SeedMergedContact is again the point: that one seeds both
// sides itself, so calling it twice yields two independent pairs rather than a
// CHAIN. Taking ids lets a world merge A into B and then B into C, which is what
// makes two-hop reparenting observable.
func (h *Harness) MergeSeededContacts(ctx context.Context, winnerID, loserID uuid.UUID) error {
	if _, err := h.contactService.MergeContacts(ctx, service.MergeContactsRequest{
		SourceContactID: loserID,  // source = archived loser
		TargetContactID: winnerID, // target = surviving winner
	}); err != nil {
		return fmt.Errorf("merge contact %s into %s: %w", loserID, winnerID, err)
	}
	return nil
}

// SeedNote attaches a notepad note to a seeded contact via the EXISTING
// NoteRepository (orchestration over an existing repo, not new machinery). The
// contact must be one the harness seeded (so the teardown's note step — delete
// by tracked contact id — cleans it). Gives the catalog its "≥1 contact with
// notes" bucket. The body is namespace-tagged so it is identifiable. Returns the
// created note id so a manifest can name the row.
func (h *Harness) SeedNote(ctx context.Context, contactID uuid.UUID, body string) (uuid.UUID, error) {
	noteRepo := repository.NewNoteRepository(h.database.Queries)
	note, err := noteRepo.CreateNotepad(ctx, contactID, h.gen.Prefix()+body)
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed note: %w", err)
	}
	return note.ID, nil
}

// SeedOrphanMeetingNote inserts a single orphan_needs_review meeting_note row
// against the harness's seeded synthetic mac_host (the Imports Interactions
// "orphan" surface). It uses the EXISTING MeetingNoteRepository — not a new
// replay adapter — so it is orchestration over an existing repo. The session id
// is a fresh random UUID; cleanup is by the harness's host id (the teardown's
// meeting_note step), so no namespace prefix is needed. Returns the created
// session id.
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
	h.recordSettleCall()
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
	// The deferred record runs on every exit path, so a gate that TIMED OUT still
	// reports what it cost — that is true by construction, not by test: the unit
	// tests pin the satisfied paths only (a timeout case would cost
	// defaultSettleTimeout of wall clock to reach the same line).
	sw := NewStopwatch()
	polls := 0
	inlineHit := false
	defer func() { h.recordGateA(sw.Elapsed(), polls, inlineHit) }()
	open, cancel := realTimeBudget(defaultSettleTimeout)
	defer cancel()
	var lastErr error
	for open() {
		polls++
		ok, err := predicate(ctx)
		if err == nil && ok {
			// An inline hit is a gate satisfied BEFORE the first sleep: it cost one
			// query, not polling latency. A gate that timed out is never one, however
			// few evaluations it managed.
			inlineHit = polls == 1
			return nil
		}
		lastErr = err
		time.Sleep(settlePollInterval)
	}
	return fmt.Errorf("synthetic settle: Gate A (domain terminal predicate) not met within %s: %v", defaultSettleTimeout, lastErr)
}

func (h *Harness) waitGateB(ctx context.Context, source string) error {
	sw := NewStopwatch()
	polls := 0
	defer func() { h.recordGateB(sw.Elapsed(), polls) }()
	contactIDs := h.snapshotContactIDs()
	open, cancel := realTimeBudget(defaultSettleTimeout)
	defer cancel()
	var lastEventJobs, lastAggJobs int64
	var lastErr error
	for open() {
		polls++
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
	sw := NewStopwatch()
	defer func() { h.recordCapture(sw.Elapsed()) }()
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

// SetManualCohortIDs records the reserved contacts the visible-task spread gave
// manual tasks (the 1-visible + >1-visible cohorts). Called by
// SeedVisibleTaskSpread — hence exported. Guarded by the ledger mutex.
func (h *Harness) SetManualCohortIDs(ids []uuid.UUID) {
	h.createdMu.Lock()
	defer h.createdMu.Unlock()
	h.manualCohortIDs = append([]uuid.UUID(nil), ids...)
}

// ManualCohortIDs returns the reserved contacts that received manual tasks (nil
// if the spread did not run). Read by a caller to assert the manual cohorts
// subject-scoped without putting UUIDs in ProfileResult.
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
