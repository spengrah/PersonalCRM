package replay

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// Batch replay drives N source payloads through ONE provider pass and settles
// ONCE per dependency generation, instead of N single replays each performing a
// full Settle. Settle is the expensive step: Gate B polls River-job finalization
// over every contact in the harness ledger, so N serialized Settles mean N
// serialized waits on worker throughput. Batching collapses those into one wait
// per generation, letting the work that used to be waited on serially overlap.
// It does NOT make any individual query cheaper.
//
// Nothing in the seed profiles calls these adapters yet.

// BatchResult is the settled outcome of one batch replay.
type BatchResult struct {
	// Payloads is the number of items driven through the provider.
	Payloads int
	// Contacts is the number of DISTINCT contacts the batch touched.
	Contacts int
	// Interactions is the number of interaction rows the batch LANDED. It is
	// deliberately not Payloads: aggregation collapses, so a promotion pair or a
	// burst conversation turns many payloads into one row. Any reconciliation
	// claim has to name which unit it means.
	Interactions int
	// SyncCalls is the total number of provider drive iterations across ALL
	// generations. Per generation it is one for Gmail, Telegram and iMessage, and
	// more than one for GCal (which DRAINS: the same input each Sync, the
	// provider advancing irreversibly) and GChat (which BUCKETS: the adapter
	// partitions the input because the provider cannot advance). So a
	// pair-bearing Gmail/Telegram/iMessage batch reports 2, not 1 — the count
	// tracks generations as well as the per-source drive shape.
	SyncCalls int
	// SettleCalls is one per dependency GENERATION: 1 normally, 2 when the batch
	// carries promotion pairs whose inbound half needs its outbound's interaction
	// to already exist. It is a correctness signal as much as a cost model — a
	// batch that reports 1 where a pair is present is racing.
	SettleCalls int
}

// Batch preflight rejections. Each is distinct and named so a rejection is an
// immediate, self-describing failure instead of a 30-second Gate A timeout that
// blames the wrong thing. They are raised BEFORE anything is driven.
var (
	// ErrBatchEmpty — a batch with no items. The count-based gate would be
	// trivially satisfied and the call is meaningless.
	ErrBatchEmpty = errors.New("synthetic batch replay: empty batch")
	// ErrBatchDuplicateIdentifier — two items addressing the same source
	// identifier. Source-message replay is idempotent (the event bus dedups on
	// (source, source_id)), so K duplicates land ONE row and count == len(items)
	// can never be reached.
	ErrBatchDuplicateIdentifier = errors.New("synthetic batch replay: duplicate source identifier")
	// ErrBatchIntentNotSeeded — an item whose Intent is not MatchSeeded. An
	// unknown-intent payload produces a pending/stranded row and no settled
	// interaction by design, so it can never satisfy the gate.
	ErrBatchIntentNotSeeded = errors.New("synthetic batch replay: item intent is not MatchSeeded")
	// ErrBatchIdentifierNotOwned — an item whose ContactID does not own the
	// identifier its spec addresses. Passing a ContactID does not force a match;
	// the payload's identifier does, so this would strand rather than settle.
	ErrBatchIdentifierNotOwned = errors.New("synthetic batch replay: contact does not own the addressed identifier")
	// ErrBatchPairKeyMalformed — a PairKey group that is not exactly two items
	// with differing directions. The generation partition is defined on PairKey,
	// so a malformed group has no defined split.
	ErrBatchPairKeyMalformed = errors.New("synthetic batch replay: PairKey group is not an outbound/inbound pair")
	// ErrBatchMixedAccounts — a batch spanning more than one connected account.
	// The me-set and the sync state are per account, so the payloads belonging to
	// the account the batch is NOT driven under would be silently dropped.
	ErrBatchMixedAccounts = errors.New("synthetic batch replay: batch spans more than one connected account")
	// ErrBatchGmailSpanExceeded — a Gmail batch whose age span exceeds what one
	// Sync can reach. See gmailBatchMaxSpan.
	ErrBatchGmailSpanExceeded = errors.New("synthetic batch replay: gmail batch age span exceeds one sync's reach")
	// ErrBatchDrainIncomplete — a provider drive loop that stopped making
	// progress, or hit its iteration cap, before every payload settled.
	ErrBatchDrainIncomplete = errors.New("synthetic batch replay: provider drive loop did not settle every payload")
)

const (
	// gmailBatchMaxSpan bounds the age span (oldest payload to newest) of one
	// Gmail batch. The Gmail provider reaches google.GmailScanReachForTest()
	// (7-day windows × a 24-window cap = 168 days) forward from one Sync's
	// backfill_since, and floors that at the OLDEST payload's send time. Anything
	// newer than oldest + 168d falls beyond the last window and is silently never
	// processed — no error, just a missing row and a Gate A timeout naming the
	// wrong cause. 150d sits deliberately under the hard bound.
	//
	// Exceeding it is an ERROR, not an auto-split: the caller is the only layer
	// that knows whether splitting a contact's history across two Syncs is
	// semantically fine, and hiding the constraint would keep it out of the
	// timeline designs built on top of this.
	gmailBatchMaxSpan = 150 * 24 * time.Hour

	// gcalPastEventPageSize mirrors the page the calendar provider's past-event
	// publish loop reads (google.CalendarPastEventPageLimitForTest). More than
	// this many past events need several Syncs; MarkLastContactedUpdated makes
	// each one advance irreversibly.
	gcalPastEventPageSize = 100

	// gchatBatchDefaultSpacesPerSync is how many spaces the GChat batch adapter
	// presents per Sync. GChat has ONE page budget of 100 shared across the
	// membership, content, and edit passes of ALL spaces in a sweep, and unlike
	// GCal it never drains: a fully processed space still costs 2 pages every
	// sweep, because the content and edit passes each issue a list call before
	// they can observe the window is empty. So the adapter partitions instead —
	// it owns the ListSpaces closure, and each bucket is processed entirely
	// within its own budget.
	//
	// The size is the budget arithmetic, not a round number. A first-sight space
	// costs gchatBatchPagesPerFirstSightSpace pages (membership + content + edit).
	// The same budget ALSO funds the reverse email→id warm-up, which is bounded
	// per sweep by the provider's member-resolve cap — every budget-consuming
	// resolution decrements both. So the worst case for B spaces is
	// 3B + resolveCap pages, and 3(16) + 50 = 98 ≤ 100. The unit test asserts
	// that against the provider's OWN exported values rather than local copies,
	// so a change to either budget fails here instead of silently truncating the
	// tail of every bucket.
	gchatBatchDefaultSpacesPerSync = 16

	// gchatBatchPagesPerFirstSightSpace is what one previously-unseen space costs
	// the shared page budget: one membership page, one content page, one
	// edit/delete page. Both content passes issue their list call BEFORE they can
	// observe the window is empty, which is why an already-drained space keeps
	// costing pages on every later sweep and why bucketing — not draining — is
	// the mitigation.
	gchatBatchPagesPerFirstSightSpace = 3

	// gchatBatchDrainSlackSyncs is how many extra Syncs the GChat adapter's
	// fallback drain allows past the bucket count. It is generous on purpose: a
	// residual that is genuinely converging should finish inside it, and one that
	// is not should be reported as a STALL — no progress across an iteration,
	// which names the real cause — rather than as a cap hit, which is ambiguous
	// between "needed one more pass" and "will never finish".
	gchatBatchDrainSlackSyncs = 8
)

// --- per-call tuning --------------------------------------------------------

// BatchOption tunes ONE batch call. Only the two sources with a drive loop take
// options; the rest need none. They exist so a test can drive the failure shapes
// the defaults exist to prevent — a GChat sweep with bucketing disabled, a GCal
// drain that hits its cap — without a package-level global. A global would be
// safe while these adapters have no caller and become real coupling the moment a
// seed profile calls them.
type BatchOption func(*batchOptions)

type batchOptions struct {
	// gchatSpacesPerSync overrides how many spaces a Sync is shown. Nil uses the
	// budget-derived default; a non-positive value disables bucketing entirely.
	gchatSpacesPerSync *int
	// gchatDrainSlackSyncs overrides the fallback drain's iteration slack.
	gchatDrainSlackSyncs *int
	// gcalMaxSyncs overrides the drain-loop iteration cap, which otherwise
	// derives from the batch size.
	gcalMaxSyncs *int
}

func applyBatchOptions(opts []BatchOption) batchOptions {
	var o batchOptions
	for _, apply := range opts {
		apply(&o)
	}
	return o
}

// WithGChatSpacesPerSync overrides the GChat bucket size for one call. A
// non-positive value presents EVERY space on every Sync — the unbucketed shape,
// which the provider's shared page budget cannot survive for a large batch.
func WithGChatSpacesPerSync(n int) BatchOption {
	return func(o *batchOptions) { o.gchatSpacesPerSync = &n }
}

// WithGChatDrainSlackSyncs overrides how many extra Syncs the GChat fallback
// drain allows past the bucket count. Raise it when a caller needs the loop to
// reach a plateau (and report a stall) rather than exit on its cap.
func WithGChatDrainSlackSyncs(n int) BatchOption {
	return func(o *batchOptions) { o.gchatDrainSlackSyncs = &n }
}

// WithGCalMaxSyncs overrides the GCal drain-loop iteration cap, which otherwise
// derives from the batch size and the provider's past-event page limit.
func WithGCalMaxSyncs(n int) BatchOption {
	return func(o *batchOptions) { o.gcalMaxSyncs = &n }
}

func (o batchOptions) spacesPerSync() int {
	if o.gchatSpacesPerSync != nil {
		return *o.gchatSpacesPerSync
	}
	return gchatBatchDefaultSpacesPerSync
}

func (o batchOptions) drainSlackSyncs() int {
	if o.gchatDrainSlackSyncs != nil {
		return *o.gchatDrainSlackSyncs
	}
	return gchatBatchDrainSlackSyncs
}

func (o batchOptions) maxSyncs(derived int) int {
	if o.gcalMaxSyncs != nil {
		return *o.gcalMaxSyncs
	}
	return derived
}

// --- shared batch machinery -------------------------------------------------

// batchEntry is the source-neutral view of one batch item that the shared
// preflight, partition, and accounting helpers work over. Each adapter projects
// its typed items into these; nothing here knows about a specific source.
type batchEntry struct {
	// contactID is the seeded contact the item targets.
	contactID uuid.UUID
	// identifier is the item's SOURCE identifier — the dedup key (a gmail
	// Message-ID, a gchat message name, a gcal event id, a peer/message-id pair,
	// an iMessage guid).
	identifier string
	// seeded is Intent == factory.MatchSeeded.
	seeded bool
	// outbound is the direction the provider will derive from the payload.
	outbound bool
	// pairKey marks the two items of a promotion pair (0 = not part of one).
	pairKey int
	// addressed is the identifier the payload addresses on the peer side — the
	// value that decides whether the contact actually matches. Empty means the
	// source has no per-item peer identifier to check.
	addressed string
	// addressedType is how to normalize addressed for comparison against the
	// contact's methods.
	addressedType identity.IdentifierType
}

// validateBatchStructure runs the preflight checks that need no DB read: a
// non-empty batch, no duplicate source identifiers, every item MatchSeeded, and
// well-formed PairKey groups. These are what make `count == len(items)` a sound
// terminal condition — without them, legal-looking input can never satisfy the
// gate and the adapter would report a timeout instead of the real cause.
func validateBatchStructure(source string, entries []batchEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("%s: %w", source, ErrBatchEmpty)
	}
	seen := make(map[string]struct{}, len(entries))
	for i, e := range entries {
		if !e.seeded {
			return fmt.Errorf("%s: item %d (%s): %w", source, i, e.identifier, ErrBatchIntentNotSeeded)
		}
		if _, dup := seen[e.identifier]; dup {
			return fmt.Errorf("%s: item %d: %w %q", source, i, ErrBatchDuplicateIdentifier, e.identifier)
		}
		seen[e.identifier] = struct{}{}
	}
	return validatePairKeys(source, entries)
}

// validatePairKeys enforces the PairKey contract: exactly two items per key,
// with differing directions. The generation partition is "the inbound member of
// each PairKey group", which is undefined for any other group shape.
func validatePairKeys(source string, entries []batchEntry) error {
	groups := map[int][]int{}
	for i, e := range entries {
		if e.pairKey == 0 {
			continue
		}
		groups[e.pairKey] = append(groups[e.pairKey], i)
	}
	keys := make([]int, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		idx := groups[k]
		if len(idx) != 2 {
			return fmt.Errorf("%s: PairKey %d has %d members (want 2): %w", source, k, len(idx), ErrBatchPairKeyMalformed)
		}
		if entries[idx[0]].outbound == entries[idx[1]].outbound {
			return fmt.Errorf("%s: PairKey %d members share direction (outbound=%t): %w",
				source, k, entries[idx[0]].outbound, ErrBatchPairKeyMalformed)
		}
	}
	return nil
}

// validateBatchOwnership is the preflight tier that needs a DB read: every
// item's ContactID must actually own the identifier its spec addresses. This
// cannot be pure — the spec types carry wire payloads, not the expected target —
// so it resolves through the harness's contact-method repository. One read per
// DISTINCT contact, not per item.
//
// Passing a ContactID to an adapter does not force a match; the payload's
// identifier does. An unmatchable payload produces a stranded row and a Gate A
// timeout, so refusing it up front turns a silent 30-second wait into an
// immediate, self-describing failure.
func (h *Harness) validateBatchOwnership(ctx context.Context, source string, entries []batchEntry) error {
	owned := map[uuid.UUID]map[string]struct{}{}
	for i, e := range entries {
		if e.addressed == "" {
			continue
		}
		set, ok := owned[e.contactID]
		if !ok {
			methods, err := h.methodRepo.ListContactMethodsByContact(ctx, e.contactID)
			if err != nil {
				return fmt.Errorf("%s: list methods for contact %s: %w", source, e.contactID, err)
			}
			set = make(map[string]struct{}, len(methods))
			for _, m := range methods {
				set[identity.Normalize(m.Value, identifierTypeForMethod(m.Type))] = struct{}{}
			}
			owned[e.contactID] = set
		}
		if _, ok := set[identity.Normalize(e.addressed, e.addressedType)]; !ok {
			return fmt.Errorf("%s: item %d (%s) addresses %q which contact %s does not own: %w",
				source, i, e.identifier, e.addressed, e.contactID, ErrBatchIdentifierNotOwned)
		}
	}
	return nil
}

// identifierTypeForMethod maps a contact_method type to the identifier type that
// normalizes its value the same way the matching path does. Comparison is on the
// normalized STRING, not on the type, so a gchat sender address (an email) still
// matches a contact carrying only an email method.
func identifierTypeForMethod(methodType string) identity.IdentifierType {
	switch repository.ContactMethodType(methodType) {
	case repository.ContactMethodEmail:
		return identity.IdentifierTypeEmail
	case repository.ContactMethodGChat:
		return identity.IdentifierTypeGChat
	case repository.ContactMethodPhone:
		return identity.IdentifierTypePhone
	case repository.ContactMethodTelegram:
		return identity.IdentifierTypeTelegram
	case repository.ContactMethodWhatsApp:
		return identity.IdentifierTypeWhatsApp
	default:
		return identity.IdentifierType(methodType)
	}
}

// partitionGenerations splits a batch into DEPENDENCY GENERATIONS, preserving
// the caller's chronological order within each.
//
//   - generation 0 — every item that is not the inbound member of a PairKey
//     group, including every non-pair member of a burst conversation;
//   - generation 1 — the inbound member of each PairKey group.
//
// The split exists because a reply bridge promotes only when the inbound session
// finds an ALREADY-EXISTING outbound interaction, and that interaction is
// written asynchronously: aggregation claims rows and publishes an envelope, and
// a River consumer writes the interaction later. Driving an inbound immediately
// after its outbound therefore races — the inbound can aggregate first, find
// nothing to promote, and land a second one-sided interaction. The single-replay
// path never saw this because its per-payload Settle WAS the barrier.
//
// A PairKey group has exactly two members, so there are at most two generations.
func partitionGenerations(entries []batchEntry) [][]int {
	gen0 := make([]int, 0, len(entries))
	var gen1 []int
	for i, e := range entries {
		if e.pairKey != 0 && !e.outbound {
			gen1 = append(gen1, i)
			continue
		}
		gen0 = append(gen0, i)
	}
	if len(gen1) == 0 {
		return [][]int{gen0}
	}
	if len(gen0) == 0 {
		return [][]int{gen1}
	}
	return [][]int{gen0, gen1}
}

// distinctContactIDs returns the batch's distinct contact ids in first-seen
// order, so per-contact work runs once rather than once per payload.
func distinctContactIDs(entries []batchEntry) []uuid.UUID {
	seen := map[uuid.UUID]struct{}{}
	out := make([]uuid.UUID, 0, len(entries))
	for _, e := range entries {
		if _, ok := seen[e.contactID]; ok {
			continue
		}
		seen[e.contactID] = struct{}{}
		out = append(out, e.contactID)
	}
	return out
}

// ageSpan returns the span between the earliest and latest of the given
// instants. An empty slice spans zero.
func ageSpan(instants []time.Time) time.Duration {
	if len(instants) == 0 {
		return 0
	}
	oldest, newest := instants[0], instants[0]
	for _, t := range instants[1:] {
		if t.Before(oldest) {
			oldest = t
		}
		if t.After(newest) {
			newest = t
		}
	}
	return newest.Sub(oldest)
}

// oldestInstant returns the earliest of the given instants (zero for an empty
// slice). Gmail floors its scan window at it.
func oldestInstant(instants []time.Time) time.Time {
	var oldest time.Time
	for i, t := range instants {
		if i == 0 || t.Before(oldest) {
			oldest = t
		}
	}
	return oldest
}

// --- interaction accounting + ledger ----------------------------------------

// batchInteractionPageSize pages the per-contact interaction reads the batch
// accounting does. It matches the single-replay ledger helper's page, which
// reads exactly one such page — fine for one payload, but a batch can put more
// than a page on one contact, and an under-read would both undercount
// Interactions and leave interaction / venue rows untracked for cleanup. Keeping
// the sizes equal also means any fixture that outgrows the single adapter's read
// exercises this loop's second page rather than fitting inside the first.
const batchInteractionPageSize = 100

// listAllContactInteractions pages a contact's interactions to exhaustion.
func (h *Harness) listAllContactInteractions(ctx context.Context, contactID uuid.UUID) ([]repository.Interaction, error) {
	var out []repository.Interaction
	for offset := int32(0); ; offset += batchInteractionPageSize {
		page, err := h.interactionRepo.ListContactInteractions(ctx, contactID, batchInteractionPageSize, offset)
		if err != nil {
			return nil, fmt.Errorf("list interactions for contact %s: %w", contactID, err)
		}
		out = append(out, page...)
		if len(page) < batchInteractionPageSize {
			return out, nil
		}
	}
}

// snapshotInteractionIDs reads the current interaction id set for each contact,
// so the batch can report how many rows IT landed rather than how many the
// contact happens to have.
//
// A read error is RETURNED, not swallowed. An empty snapshot would make every
// pre-existing row look newly landed and OVERSTATE BatchResult.Interactions —
// the field downstream reconciliation is written against — and it would do so
// silently. The snapshot runs before anything is driven, so failing here costs
// nothing and names its own cause.
func (h *Harness) snapshotInteractionIDs(ctx context.Context, contactIDs []uuid.UUID) (map[uuid.UUID]map[uuid.UUID]struct{}, error) {
	out := make(map[uuid.UUID]map[uuid.UUID]struct{}, len(contactIDs))
	for _, id := range contactIDs {
		rows, err := h.listAllContactInteractions(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("snapshot interactions before batch: %w", err)
		}
		set := make(map[uuid.UUID]struct{}, len(rows))
		for _, r := range rows {
			set[r.ID] = struct{}{}
		}
		out[id] = set
	}
	return out, nil
}

// trackBatchInteractions records every interaction id (and its venue node id)
// for the given contacts into the cleanup ledger and reports how many are NEW
// relative to before. It is the batch-scale form of the single adapters'
// per-replay tracker: same ledger writes, but paged, because a batch can put
// more interactions on one contact than a single page holds and an untracked
// venue node has no prefix-cleanup fallback (it is not a contact and its
// canonical label is empty).
//
// Best-effort, like the single-replay tracker: a read error leaves the
// by-contact path to cover interactions rather than failing the replay.
func (h *Harness) trackBatchInteractions(ctx context.Context, contactIDs []uuid.UUID, before map[uuid.UUID]map[uuid.UUID]struct{}) int {
	landed := 0
	for _, contactID := range contactIDs {
		rows, err := h.listAllContactInteractions(ctx, contactID)
		if err != nil {
			continue
		}
		prior := before[contactID]
		h.track(func(c *created) {
			for _, r := range rows {
				c.addInteraction(r.ID)
				if r.VenueID != nil {
					c.addVenueNode(*r.VenueID)
				}
			}
		})
		for _, r := range rows {
			if _, existed := prior[r.ID]; !existed {
				landed++
			}
		}
	}
	return landed
}

// drainPartial makes a mid-batch failure RECLAIMABLE before the error is
// returned. Registering source identifiers per payload is not enough on its own:
// interaction and venue ids are captured only by the post-Settle ledger pass,
// venue nodes have no prefix-cleanup fallback, and teardown skips EVERY delete
// when Gate B has not cleared — which is exactly the state a mid-batch failure
// leaves.
//
// So on any error after the first payload is driven, the adapter (a)
// bounded-waits Gate B, (b) records the touched contacts' interactions and venue
// nodes, (c) rebuilds the cleanup event-id union, and only then returns the
// ORIGINAL error, wrapped and never replaced. If the bounded wait does not
// clear, teardown's skip-all-deletes behaviour is the correct fallback and the
// returned error says so.
func (h *Harness) drainPartial(ctx context.Context, source string, aggSource string, contactIDs []uuid.UUID, cause error) error {
	gateBErr := h.waitGateB(ctx, aggSource)
	h.trackBatchInteractions(ctx, contactIDs, nil)
	captureErr := h.captureEventIDs(ctx)

	msg := fmt.Sprintf("%s batch: partial drain after failure", source)
	if gateBErr != nil {
		msg += fmt.Sprintf(" (Gate B did not clear: %v — teardown will skip its deletes and leave the namespace intact)", gateBErr)
	}
	if captureErr != nil {
		msg += fmt.Sprintf(" (event-id capture failed: %v)", captureErr)
	}
	return fmt.Errorf("%s: %w", msg, cause)
}

// --- provider drive helpers -------------------------------------------------

// gateCount is a batch Gate A count read, scoped to one generation's
// identifiers. The drain/bucket loops poll it between provider iterations —
// never Settle, because a Settle whose predicate demands all N must time out on
// the first iteration when only a prefix can have landed.
type gateCount func(ctx context.Context) (int64, error)

// driveUntilCount runs drive repeatedly until the gate count reaches want. It is
// the GCal drain shape: the same input every iteration, the provider advancing
// irreversibly, so re-driving makes progress. It stops on completion, on an
// iteration that made NO progress (a stall), or at the iteration cap — the last
// two as a loud error naming the shortfall, never a spin.
//
// A stall is not a flake to be absorbed by a more generous cap. The calendar
// provider's past-event read takes a DB-wide LIMIT and the namespace scoping is
// applied after it, so another namespace owning the oldest unprocessed page
// starves this one on every iteration regardless of cap. That is why these
// adapters are exercised on a per-test isolated database, where DB-wide is this
// namespace.
func driveUntilCount(ctx context.Context, want int64, maxIterations int, drive func(context.Context) error, count gateCount) (syncCalls int, err error) {
	prev := int64(-1)
	for i := 0; i < maxIterations; i++ {
		if err := drive(ctx); err != nil {
			return syncCalls, err
		}
		syncCalls++
		n, err := count(ctx)
		if err != nil {
			return syncCalls, err
		}
		if n >= want {
			return syncCalls, nil
		}
		if n <= prev {
			return syncCalls, fmt.Errorf("stalled at %d of %d after %d drive iterations: %w", n, want, syncCalls, ErrBatchDrainIncomplete)
		}
		prev = n
	}
	n, cErr := count(ctx)
	if cErr != nil {
		return syncCalls, cErr
	}
	return syncCalls, fmt.Errorf("reached %d of %d at the %d-iteration cap: %w", n, want, maxIterations, ErrBatchDrainIncomplete)
}

// chunkStrings partitions xs into consecutive buckets of at most size. A size of
// zero or less yields ONE bucket holding everything — the "no bucketing" shape
// the GChat negative-control test drives, which the provider's shared page
// budget cannot survive past roughly half the catalog.
func chunkStrings(xs []string, size int) [][]string {
	if size <= 0 || len(xs) <= size {
		return [][]string{xs}
	}
	out := make([][]string, 0, (len(xs)+size-1)/size)
	for start := 0; start < len(xs); start += size {
		end := start + size
		if end > len(xs) {
			end = len(xs)
		}
		out = append(out, xs[start:end])
	}
	return out
}
