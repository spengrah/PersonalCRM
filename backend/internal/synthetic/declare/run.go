package declare

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
)

const (
	// defaultRunBudget bounds a whole declared seed: lock acquisition, harness
	// construction, every entity, the settle waits, and the final Gate-B drain.
	// Order-of-a-minute — comfortably above a measured multi-entity seed, low
	// enough that a wedged run fails loudly instead of pinning a handler.
	defaultRunBudget = 90 * time.Second
	// defaultTeardownBudget bounds the FAILURE-path teardown. It deliberately
	// does not inherit the (possibly expired) run deadline, but it is not
	// unbounded either: the budget threads into every cleanup DB call.
	defaultTeardownBudget = 45 * time.Second
	// lockOpBudget bounds a single unlock. Unlock runs on a FRESH context —
	// never the possibly-dead run context — so a failing run still releases.
	lockOpBudget = 10 * time.Second
	// bandClaimAttempts bounds the re-salt/revalidate retry loop.
	bandClaimAttempts = 3
)

// Seeded is one row a declaration created, as the manifest reports it.
type Seeded struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Result is the manifest of an executed declaration.
type Result struct {
	// Namespace is the EFFECTIVE namespace — it may carry a -sN re-salt suffix
	// the caller never asked for. Callers assert against this, never the input.
	Namespace string `json:"namespace"`
	// Anchor is the generator anchor the world was built against, so a caller can
	// time anchor-relative facts (a birthday, a backdated created_at) against it.
	Anchor time.Time `json:"anchor"`
	// Entities maps each declared handle to what was created.
	Entities map[string]Seeded `json:"entities"`
}

var (
	// ErrUnknownBehavior: no declaration and no no-fixture registration.
	ErrUnknownBehavior = errors.New("declare: unknown behavior id")
	// ErrNoFixtureBehavior: the behavior is resolved as needing no fixture, so
	// there is nothing to seed. A client asking for one has a bug.
	ErrNoFixtureBehavior = errors.New("declare: behavior needs no fixture")
	// ErrInvalidNamespace: charset, length, or the reserved -sN salt grammar.
	ErrInvalidNamespace = errors.New("declare: invalid namespace")
	// ErrNamespaceOccupied: the namespace already holds rows (live or tombstoned).
	ErrNamespaceOccupied = errors.New("declare: namespace is already occupied")
	// ErrNamespaceNested: the namespace is a descendant of a LIVE namespace, whose
	// prefix-scoped cleanup would sweep across it.
	ErrNamespaceNested = errors.New("declare: namespace is nested under a live namespace")
	// ErrNamespaceBusy: a concurrent run holds the namespace reservation. A
	// blocked caller is by definition a duplicate, so this answers immediately
	// rather than queueing.
	ErrNamespaceBusy = errors.New("declare: namespace reservation is held by a concurrent run")
)

// RunError carries the recovery metadata a failed run owes its caller: which
// namespace was involved, and whether the failure left rows behind. Cleaned
// reports REALITY — never a blanket value — because the client's next move
// (retry here vs clean up first) depends on it.
type RunError struct {
	Namespace string
	Cleaned   bool
	Err       error
}

func (e *RunError) Error() string {
	return fmt.Sprintf("declare: run failed for namespace %q (cleaned=%t): %v", e.Namespace, e.Cleaned, e.Err)
}

func (e *RunError) Unwrap() error { return e.Err }

// saltSuffix matches the re-salt segment resolveNamespace mints (-s1..-s7).
var saltSuffix = regexp.MustCompile(`^s\d+$`)

// maxNamespaceLen bounds a namespace token — requested or EFFECTIVE. The
// 'synth-<ns>-' prefix is added on top, so this leaves ample room under any
// column width.
const maxNamespaceLen = 60

// maxSaltSuffixLen is the width of the widest suffix re-salting can append.
// maxSaltAttempt is a single digit, so that is len("-s") plus one digit;
// TestSaltSuffixFitsItsReservedRoom keeps the two in step.
const maxSaltSuffixLen = len("-s") + 1

// maxRequestedNamespaceLen bounds a REQUESTED namespace, reserving room for the
// suffix re-salting may append. Construction resolves the effective namespace
// internally and never revalidates it, so a requested token that filled the
// whole budget would seed a world under a token LONGER than the token grammar
// allows — and that token is exactly what the client hands back to cleanup.
// Cleanup would then reject the whole request and the rows would survive the
// test that created them. Reserving the room here is what keeps the seed and
// cleanup grammars consistent: every namespace a seed can produce is a
// namespace cleanup accepts.
const maxRequestedNamespaceLen = maxNamespaceLen - maxSaltSuffixLen

// ValidateNamespaceToken checks charset and length. Cleanup uses it, because
// cleanup legitimately receives EFFECTIVE namespaces that end in -sN.
func ValidateNamespaceToken(namespace string) error {
	if l := len(namespace); l < 1 || l > maxNamespaceLen {
		return fmt.Errorf("%w: %q has length %d, want 1..%d", ErrInvalidNamespace, namespace, l, maxNamespaceLen)
	}
	if err := replay.ValidateNamespace(namespace); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidNamespace, err)
	}
	return nil
}

// ValidateRequestedNamespace is ValidateNamespaceToken plus the two reservations
// a REQUESTED namespace owes the re-salt suffix: it may not END in -sN (that is
// the suffix re-salting mints internally, and reserving it is what makes
// cleanup's requested → salted-variant expansion unambiguous), and it must leave
// ROOM for one (so the effective namespace stays a valid token).
func ValidateRequestedNamespace(namespace string) error {
	if err := ValidateNamespaceToken(namespace); err != nil {
		return err
	}
	if l := len(namespace); l > maxRequestedNamespaceLen {
		return fmt.Errorf("%w: %q has length %d, want 1..%d for a requested namespace (%d characters are reserved for the -sN re-salt suffix)",
			ErrInvalidNamespace, namespace, l, maxRequestedNamespaceLen, maxSaltSuffixLen)
	}
	segments := strings.Split(namespace, "-")
	if saltSuffix.MatchString(segments[len(segments)-1]) {
		return fmt.Errorf("%w: %q ends in the reserved -sN re-salt suffix", ErrInvalidNamespace, namespace)
	}
	return nil
}

// Run executes the declaration registered for behaviorID in the given
// namespace and returns the manifest of what it created.
//
// It owns its own context: the caller's ctx supplies values only, and the run
// executes under context.WithoutCancel + a deadline. Detached, because a client
// disconnect must not cancel River's fetch loop mid-settle (River silently
// stops fetching when its Start context dies). Deadline-bounded, because no
// step may outlive the budget. A fired deadline degrades to the loud bounded
// failure path, never a hang.
//
// On success the namespace is QUIESCENT: Gate B has drained, so no job this
// seed created still references its rows — which is what makes the later,
// stateless cleanup safe to delete by id set.
func Run(ctx context.Context, database *db.Database, behaviorID, namespace string, seed uint64) (Result, error) {
	d, err := ValidateSeedRequest(behaviorID, namespace)
	if err != nil {
		return Result{}, err
	}
	return execute(ctx, database, d, namespace, seedOrDefault(seed))
}

// ValidateSeedRequest resolves the behavior and checks the namespace grammar
// WITHOUT touching the database, so a caller can reject a malformed request at
// its own boundary before doing any wiring-dependent work. Run calls it too, so
// the two can never disagree about what is valid.
func ValidateSeedRequest(behaviorID, namespace string) (Declaration, error) {
	if reason, isNone := IsNone(behaviorID); isNone {
		return Declaration{}, fmt.Errorf("%w: %s (%s)", ErrNoFixtureBehavior, behaviorID, reason)
	}
	d, ok := Lookup(behaviorID)
	if !ok {
		return Declaration{}, fmt.Errorf("%w: %s (%d declarations registered)", ErrUnknownBehavior, behaviorID, len(Registered()))
	}
	if err := ValidateRequestedNamespace(namespace); err != nil {
		return Declaration{}, err
	}
	return d, nil
}

func seedOrDefault(seed uint64) uint64 {
	if seed == 0 {
		return factory.DefaultSeed
	}
	return seed
}

// execute is the full protocol: reserve, claim bands, construct, run, drain.
func execute(parent context.Context, database *db.Database, d Declaration, namespace string, seed uint64) (Result, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), defaultRunBudget)
	defer cancel()

	locks, err := newLockSession(ctx, database)
	if err != nil {
		return Result{}, err
	}
	defer locks.close()

	if err := reserveNamespaceFamily(ctx, locks, namespace); err != nil {
		return Result{}, err
	}

	support := repository.NewSyntheticSupportRepository(database.Queries)
	if err := checkNamespaceFree(ctx, support, namespace); err != nil {
		return Result{}, err
	}

	h, teardown, err := buildHarness(ctx, database, support, locks, namespace, seed)
	if err != nil {
		// The paths that built a harness and then tore it down already know
		// whether the namespace came out clean, and say so; pass those through
		// rather than re-deriving a worse answer here.
		var runErr *RunError
		if errors.As(err, &runErr) {
			return Result{}, err
		}
		// A pre-host constructor failure leaves nothing; a post-host one removes
		// the marker best-effort and wraps ErrConstructorResidue only when THAT
		// removal failed. Report the actual outcome.
		return Result{}, &RunError{
			Namespace: namespace,
			Cleaned:   !errors.Is(err, replay.ErrConstructorResidue),
			Err:       err,
		}
	}

	res, err := runEntities(ctx, h, support, d)
	if err != nil {
		return Result{}, &RunError{Namespace: h.Namespace(), Cleaned: driveTeardown(teardown), Err: err}
	}
	// Success: stop the client but LEAVE the rows. Quiesce never errors.
	_ = h.Quiesce(ctx)
	return res, nil
}

// teardownError drives the failure-path teardown for a run that HAS a harness
// and wraps the cause with the outcome the teardown actually produced. Deriving
// `cleaned` any other way at these sites would be a guess: the teardown's own
// contract is to LEAVE the rows when Gate B is uncleared, so only its return
// value knows.
func teardownError(teardown func(context.Context) error, namespace string, cause error) error {
	return &RunError{Namespace: namespace, Cleaned: driveTeardown(teardown), Err: cause}
}

// driveTeardown runs the failure-path teardown under its OWN deadline-bounded
// context (not the run's, which may already have expired) and reports whether
// the namespace came out clean. The teardown's documented contract is to LEAVE
// the rows when Gate B is uncleared, so a non-nil error means residue remains.
func driveTeardown(teardown func(context.Context) error) bool {
	if teardown == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTeardownBudget)
	defer cancel()
	return teardown(ctx) == nil
}

// reserveNamespaceFamily takes the run's namespace reservation.
//
// The reservation covers the whole FAMILY — the requested token plus every
// salted variant re-salting can mint — and that scope is load-bearing rather
// than cautious. Construction resolves the effective namespace INTERNALLY and
// materializes its host marker before returning, so a run that re-salts has
// already published a discoverable world under a token it was never asked
// about. Cleanup discovers exactly that way (requested → live -sN variants) and
// try-locks the EFFECTIVE token, so a reservation covering only the requested
// one would let a lost-response cleanup delete a still-running harness out from
// under it. Reserving the family before anything is materialized closes it:
// whatever construction picks is already ours.
//
// Order matters. The requested token goes FIRST and alone decides the winner of
// a same-namespace race — two concurrent runs contend on exactly one key, so
// exactly one proceeds. A variant key can then only be held by a cleanup of that
// variant, where reporting busy is the correct, retriable answer.
func reserveNamespaceFamily(ctx context.Context, locks *lockSession, namespace string) error {
	acquired, err := locks.tryLock(ctx, advisoryKey("declare:"+namespace))
	if err != nil {
		return fmt.Errorf("declare: reserve namespace %q: %w", namespace, err)
	}
	if !acquired {
		return fmt.Errorf("%w: %s", ErrNamespaceBusy, namespace)
	}

	variantKeys := make([]int64, 0, maxSaltAttempt)
	for _, variant := range saltVariants(namespace) {
		variantKeys = append(variantKeys, advisoryKey("declare:"+variant))
	}
	sort.Slice(variantKeys, func(i, j int) bool { return variantKeys[i] < variantKeys[j] })

	acquired, err = locks.tryLockAll(ctx, variantKeys)
	if err != nil {
		return fmt.Errorf("declare: reserve re-salt variants of %q: %w", namespace, err)
	}
	if !acquired {
		return fmt.Errorf("%w: a salted variant of %s is held (a cleanup of an earlier world under this token is in flight)",
			ErrNamespaceBusy, namespace)
	}
	return nil
}

// saltVariants are the namespaces re-salting can mint for this token, in order.
func saltVariants(namespace string) []string {
	out := make([]string, 0, maxSaltAttempt)
	for i := 1; i <= maxSaltAttempt; i++ {
		out = append(out, fmt.Sprintf("%s-s%d", namespace, i))
	}
	return out
}

// checkNamespaceFree runs the two occupancy checks, under the reservation lock.
//
//	direct       — any contact under 'synth-<ns>-' (LIVE OR TOMBSTONED, because a
//	               tombstone still owns the namespace's identifiers), or an exact
//	               'synth-<ns>-host' row — and the same host check for every
//	               salted variant the reservation covers. The contact sweep
//	               already reaches a salted world (its names carry the requested
//	               token as a prefix); the host check must be spelled out per
//	               variant because it is an EXACT match. Without it a HOST-ONLY
//	               residue from an earlier re-salted run is invisible here, and
//	               the next run could re-salt onto that same variant and
//	               materialize a second world on top of it.
//	descendant   — a live host for any namespace NESTED under this one. The
//	               direct check only closes the descendant direction for worlds
//	               that still have CONTACTS: a child carrying nothing but a host
//	               marker (constructor residue, or a child whose ladder failed
//	               before its marker) has no contact under this prefix and is not
//	               one of the exact tokens above, so nothing else here sees it.
//	               Admitting the parent then creates a world whose OWN cleanup
//	               refuses forever, because the descendant guard names that child
//	               and aborts every sweep.
//	hierarchical — a live host for any proper hyphen-boundary ANCESTOR of the
//	               namespace. This closes the other half of the foo/foo-bar hole:
//	               foo-bar's rows are invisible to foo's own occupancy check, but
//	               a later cleanup of foo would LIKE-sweep 'synth-foo-%' straight
//	               across them.
func checkNamespaceFree(ctx context.Context, support *repository.SyntheticSupportRepository, namespace string) error {
	prefix := factory.SyntheticSourcePrefix + namespace + "-"

	contactIDs, err := support.SelectContactIDsByFullNamePrefix(ctx, prefix)
	if err != nil {
		return fmt.Errorf("declare: occupancy check (contacts) for %q: %w", namespace, err)
	}
	if len(contactIDs) > 0 {
		return fmt.Errorf("%w: %q already holds %d contact rows", ErrNamespaceOccupied, namespace, len(contactIDs))
	}
	for _, token := range append([]string{namespace}, saltVariants(namespace)...) {
		hostname := factory.SyntheticSourcePrefix + token + "-host"
		if _, exists, err := support.SelectMacHostIDByHostname(ctx, hostname); err != nil {
			return fmt.Errorf("declare: occupancy check (host) for %q: %w", token, err)
		} else if exists {
			return fmt.Errorf("%w: %q already holds a synthetic host row", ErrNamespaceOccupied, token)
		}
	}

	// Descendants are found by the SAME query and the same salt-variant filter
	// the cleanup guard uses, so the two can never disagree about what counts as
	// a nested world — a seed admitted here whose cleanup would then refuse is
	// exactly the state that strands rows.
	descendants, err := liveDescendants(ctx, support, namespace, prefix)
	if err != nil {
		return fmt.Errorf("declare: descendant check for %q: %w", namespace, err)
	}
	if len(descendants) > 0 {
		return fmt.Errorf("%w: %q has %d live namespace(s) nested under it (%s)",
			ErrNamespaceOccupied, namespace, len(descendants), strings.Join(descendants, ", "))
	}

	for _, ancestor := range hyphenAncestors(namespace) {
		hostname := factory.SyntheticSourcePrefix + ancestor + "-host"
		if _, exists, err := support.SelectMacHostIDByHostname(ctx, hostname); err != nil {
			return fmt.Errorf("declare: hierarchy check for %q: %w", namespace, err)
		} else if exists {
			return fmt.Errorf("%w: %q is nested under live namespace %q", ErrNamespaceNested, namespace, ancestor)
		}
	}
	return nil
}

// hyphenAncestors returns every PROPER hyphen-boundary prefix of a namespace
// ("w3-1700-c1" → ["w3", "w3-1700"]).
func hyphenAncestors(namespace string) []string {
	var out []string
	for i := 1; i < len(namespace); i++ {
		if namespace[i] == '-' {
			out = append(out, namespace[:i])
		}
	}
	return out
}

// buildHarness claims the namespace's numeric bands, constructs the harness,
// and — when construction re-salted onto a different namespace — performs the
// release-acquire-REVALIDATE band swap before letting any declaration run.
//
// Why bands need locking at all: the generator buckets namespaces into only a
// few hundred phone area codes, so globally unique namespaces do NOT give
// unique bands. Two concurrent constructions could both pass the toolkit's
// pre-insert occupancy read and later mint IDENTICAL phone identifiers, which
// identity matching resolves DB-wide with no namespace scoping. Serializing the
// band is what makes the toolkit's existing detect-and-re-salt sound.
//
// Why the swap revalidates: releasing the requested bands before acquiring the
// effective ones is mandatory (holding one band set while blocking on another
// is a deadlock shape), and that gap is real — a third run initially mapped to
// the effective namespace can seed its band inside it. Executing on the stale
// pre-swap read would recreate exactly the collision this exists to prevent, so
// the occupancy read is repeated UNDER the new locks. Construction itself
// writes no band rows, so nothing leaked before the swap.
func buildHarness(
	ctx context.Context,
	database *db.Database,
	support *repository.SyntheticSupportRepository,
	locks *lockSession,
	namespace string,
	seed uint64,
) (*replay.Harness, func(context.Context) error, error) {
	var lastErr error
	for attempt := 0; attempt < bandClaimAttempts; attempt++ {
		requestedKeys := bandKeys(factory.NewGenerator(seed, namespace))
		if err := locks.lockAll(ctx, requestedKeys); err != nil {
			return nil, nil, fmt.Errorf("declare: claim numeric bands for %q: %w", namespace, err)
		}

		h, teardown, err := replay.NewHarnessWithDBForNamespace(ctx, database, namespace, seed)
		if err != nil {
			_ = locks.unlockAll(requestedKeys)
			return nil, nil, err
		}

		effective := h.Namespace()
		if effective == namespace {
			return h, teardown, nil
		}

		// Re-salted: swap the band claim onto the effective namespace. The
		// release comes FIRST and its outcome is decisive — going on to block
		// on the effective namespace's bands while still holding the requested
		// ones is hold-and-wait, the deadlock shape this ordering exists to
		// avoid. A release that errored (or reported the lock not held) leaves
		// the true state unknowable from here, so the run aborts and closes the
		// session immediately: destroying the connection is the only thing that
		// can settle a session lock we can no longer account for.
		if err := locks.unlockAll(requestedKeys); err != nil {
			locks.close()
			return nil, nil, teardownError(teardown, effective,
				fmt.Errorf("declare: release the requested bands of %q before the re-salt swap: %w", namespace, err))
		}
		effectiveGen := factory.NewGeneratorAt(seed, effective, h.Generator().Anchor())
		effectiveKeys := bandKeys(effectiveGen)
		if err := locks.lockAll(ctx, effectiveKeys); err != nil {
			return nil, nil, teardownError(teardown, effective,
				fmt.Errorf("declare: claim numeric bands for re-salted namespace %q: %w", effective, err))
		}

		free, err := bandsFree(ctx, support, effectiveGen)
		if err != nil {
			_ = locks.unlockAll(effectiveKeys)
			return nil, nil, teardownError(teardown, effective,
				fmt.Errorf("declare: revalidate bands for %q: %w", effective, err))
		}
		if free {
			return h, teardown, nil
		}

		// Somebody claimed the effective namespace's band during the swap gap.
		// Tear this harness down and retry construction from scratch — the
		// retry's own collision check sees their rows and salts onward. The
		// release is decisive for the same reason as the swap's: the next
		// attempt BLOCKS on the requested bands, so continuing with an
		// unaccountable hold would be hold-and-wait.
		releaseErr := locks.unlockAll(effectiveKeys)
		cleaned := driveTeardown(teardown)
		if releaseErr != nil {
			locks.close()
			return nil, nil, &RunError{
				Namespace: effective,
				Cleaned:   cleaned,
				Err:       fmt.Errorf("declare: release the re-salted bands of %q before retrying: %w", effective, releaseErr),
			}
		}
		lastErr = fmt.Errorf("numeric bands for re-salted namespace %q were claimed during the lock swap", effective)
	}
	return nil, nil, fmt.Errorf("declare: could not claim free numeric bands for %q after %d attempts: %w",
		namespace, bandClaimAttempts, lastErr)
}

// bandKeys are this namespace's two numeric-band advisory keys, sorted. Sorted
// acquisition order across all callers is what makes multi-key claims
// deadlock-free.
func bandKeys(gen *factory.Generator) []int64 {
	keys := []int64{
		advisoryKey(fmt.Sprintf("declare-band:phone:%d", gen.PhoneAreaCode())),
		advisoryKey(fmt.Sprintf("declare-band:peer:%d", gen.PeerBandStart())),
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// bandsFree repeats the toolkit's own pre-insert band occupancy reads. It is
// the revalidation step of the swap, so it must ask exactly what the toolkit
// asks — any narrower predicate would let a collision through.
func bandsFree(ctx context.Context, support *repository.SyntheticSupportRepository, gen *factory.Generator) (bool, error) {
	peers, err := support.CountTelegramMessagesInPeerBand(ctx, gen.PeerBandStart(), gen.PeerBandEnd())
	if err != nil {
		return false, fmt.Errorf("peer band: %w", err)
	}
	chatConfigs, err := support.CountTelegramChatConfigInChatIdBand(ctx, gen.PeerBandStart(), gen.PeerBandEnd())
	if err != nil {
		return false, fmt.Errorf("chat-config band: %w", err)
	}
	barePeers, err := support.CountTelegramBarePeerRowsInBand(ctx, gen.PeerBandStart(), gen.PeerBandEnd())
	if err != nil {
		return false, fmt.Errorf("bare-peer band: %w", err)
	}
	phonePrefix := gen.SyntheticPhonePrefix()
	methodPhones, err := support.CountContactMethodsByValueNormalizedPrefix(ctx, phonePrefix)
	if err != nil {
		return false, fmt.Errorf("phone band (contact_method): %w", err)
	}
	identityPhones, err := support.CountExternalIdentitiesByIdentifierPrefix(ctx, phonePrefix)
	if err != nil {
		return false, fmt.Errorf("phone band (external_identity): %w", err)
	}
	return peers == 0 && chatConfigs == 0 && barePeers == 0 && methodPhones == 0 && identityPhones == 0, nil
}

// runState carries what a running entity list already produced: the generated
// spec of every contact (so a later entity can copy its rendered name) and the
// manifest entry of every handle (so a later entity can act on its row), plus a
// single monotonic creation log in EXECUTION order.
type runState struct {
	specs  map[string]factory.ContactSpec
	seeded map[string]Seeded
	// order and orderHandles are the creation log and its parallel handle list,
	// both in EXECUTION order.
	order        []Seeded
	orderHandles []string
}

func newRunState(size int) *runState {
	return &runState{
		specs:  make(map[string]factory.ContactSpec, size),
		seeded: make(map[string]Seeded, size),
	}
}

func (st *runState) record(handle string, seeded Seeded) {
	st.seeded[handle] = seeded
	st.order = append(st.order, seeded)
	st.orderHandles = append(st.orderHandles, handle)
}

// contactID resolves an earlier CONTACT handle's id. Register-time validation
// guarantees the handle exists and is a contact, so a failure here is a
// programming error in the lowering rather than a bad declaration.
func (st *runState) contactID(handle string) (uuid.UUID, error) {
	seeded, ok := st.seeded[handle]
	if !ok {
		return uuid.Nil, fmt.Errorf("handle %q has not been created", handle)
	}
	id, err := uuid.Parse(seeded.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("handle %q: %w", handle, err)
	}
	return id, nil
}

// runEntities executes a declaration's entities in declared order, then drains
// Gate B. Success therefore means "quiescent", not merely "written".
func runEntities(
	ctx context.Context,
	h *replay.Harness,
	support *repository.SyntheticSupportRepository,
	d Declaration,
) (Result, error) {
	st := newRunState(len(d.Entities))
	if err := runEntityList(ctx, h, support, d.Entities, st); err != nil {
		return Result{}, err
	}
	if err := h.DrainGateB(ctx); err != nil {
		return Result{}, err
	}
	return Result{
		Namespace: h.Namespace(),
		Anchor:    h.Generator().Anchor(),
		Entities:  st.seeded,
	}, nil
}

// runEntityList executes entities in declared order, recording each outcome in
// st. It performs NO Gate-B drain: the drain is a whole-NAMESPACE predicate, so
// it belongs to the caller — once per declaration for Run, once per composed
// world for World.
func runEntityList(
	ctx context.Context,
	h *replay.Harness,
	support *repository.SyntheticSupportRepository,
	entities []Entity,
	st *runState,
) error {
	for i, e := range entities {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("declare: run budget expired before entity %q: %w", e.handle(), err)
		}
		seeded, err := runEntity(ctx, h, support, e, st)
		if err != nil {
			return fmt.Errorf("declare: entity %q: %w", e.handle(), err)
		}
		st.record(e.handle(), seeded)
		if i == 0 && currentFailpoint() == FailpointAfterFirstEntity {
			return fmt.Errorf("declare: failpoint %q fired after entity %q", FailpointAfterFirstEntity, e.handle())
		}
	}
	return nil
}

func runEntity(
	ctx context.Context,
	h *replay.Harness,
	support *repository.SyntheticSupportRepository,
	e Entity,
	st *runState,
) (Seeded, error) {
	switch p := e.(type) {
	case *contactPlan:
		return runContact(ctx, h, support, p, st)
	case *externalCandidatePlan:
		return runExternalCandidate(ctx, h, support, p, st)
	case *methodSuggestionPlan:
		return runMethodSuggestion(ctx, h, support, p, st)
	case *meetingNotePlan:
		return runMeetingNote(ctx, h, p)
	case *notePlan:
		return runNote(ctx, h, p, st)
	case *mergePlan:
		return runMerge(ctx, h, p, st)
	case *softDeletePlan:
		return runSoftDelete(ctx, h, p, st)
	default:
		return Seeded{}, fmt.Errorf("unsupported entity kind %q", e.kind())
	}
}

// runExternalCandidate lowers a declared import candidate onto the PRODUCTION
// writer its source actually has — the direct upsert, the mac-daemon ingest
// pipeline, or one of the two dedicated discovery writers (see candidateSources).
// Dispatching on the source is what keeps a declared candidate a row the sync path
// could have produced; a single writer for all seven would produce rows five of
// them cannot.
//
// Whatever the path, the row's namespace ownership is recorded by ID. The prefix
// sweep recovers only the sources whose production source_id is a
// namespace-prefixed string; telegram keys on a decimal peer id and anarlog_title
// on a SHA-256 digest, and prefixing either would forfeit exactly the fidelity
// using the real writer buys. The ownership record is keyed by the row id, which
// nothing in the application rewrites, and its id-set is unioned with the prefix
// sweep rather than replacing it.
func runExternalCandidate(
	ctx context.Context,
	h *replay.Harness,
	support *repository.SyntheticSupportRepository,
	p *externalCandidatePlan,
	st *runState,
) (Seeded, error) {
	var (
		id   uuid.UUID
		name string
		err  error
	)
	switch p.source {
	case SourceICloudContacts, SourceAnarlogHumans:
		id, name, err = seedIngestedCandidate(ctx, h, p)
	case SourceTelegram:
		id, name, err = seedTelegramCandidate(ctx, h, p)
	case SourceAnarlogTitle:
		id, name, err = seedTitleCandidate(ctx, h, p)
	default:
		id, name, err = seedUpsertCandidate(ctx, h, p, st)
	}
	if err != nil {
		return Seeded{}, err
	}
	if err := support.RecordNamespaceEntity(ctx, h.Namespace(), repository.EntityKindExternalContact, id); err != nil {
		return Seeded{}, fmt.Errorf("record namespace ownership: %w", err)
	}
	return Seeded{Kind: "external_contact", ID: id.String(), Name: name}, nil
}

// seedUpsertCandidate is the direct-upsert path (gcontacts, gcal_attendee,
// gmail_correspondence). SameNameAs overrides the generated display name with an
// earlier contact's rendered name, which is what makes the candidate collide with
// that contact in the matcher rather than merely resemble it; SameEmailAs does the
// same for the primary email, which is what lifts that collision to a
// high-confidence match.
func seedUpsertCandidate(
	ctx context.Context,
	h *replay.Harness,
	p *externalCandidatePlan,
	st *runState,
) (uuid.UUID, string, error) {
	emails := p.emails
	if emails < 1 {
		emails = 1
	}
	spec := h.Generator().ExternalContactCandidate(p.source, emails, p.phones)
	if p.sameNameAs != "" {
		twin, ok := st.specs[p.sameNameAs]
		if !ok {
			return uuid.Nil, "", fmt.Errorf("SameNameAs(%q): no such contact in this run", p.sameNameAs)
		}
		spec.DisplayName = twin.FullName
	}
	if p.sameEmailAs != "" {
		twin, ok := st.specs[p.sameEmailAs]
		if !ok {
			return uuid.Nil, "", fmt.Errorf("SameEmailAs(%q): no such contact in this run", p.sameEmailAs)
		}
		spec.Emails[0] = twin.Email
	}

	var evidence *replay.CorrespondenceEvidence
	if p.coOccurringWith != "" {
		co, ok := st.seeded[p.coOccurringWith]
		if !ok {
			return uuid.Nil, "", fmt.Errorf("CorrespondenceEvidence(%q): no such contact in this run", p.coOccurringWith)
		}
		evidence = &replay.CorrespondenceEvidence{
			MessageCount: p.messageCount,
			// The id is deliberately EMPTY: the discoverer degrades to id-only
			// evidence when its name lookup fails and writes the id as a JSON
			// string, so a fixture that supplied the real uuid would be seeding the
			// one shape the badge does not have to render.
			CoOccurringContactID: "",
			CoOccurringName:      co.Name,
		}
	}

	id, err := h.SeedExternalContactCandidate(ctx, spec, evidence)
	if err != nil {
		return uuid.Nil, "", err
	}
	return id, spec.DisplayName, nil
}

// seedIngestedCandidate is the mac-daemon INGEST path (icloud_contacts,
// anarlog_humans), the only writer the ingest registry admits for those two
// sources. It reuses the existing factory payload and replay adapter, then reads
// the row back so the manifest reports the display name the pipeline actually
// stored rather than one re-derived here.
func seedIngestedCandidate(
	ctx context.Context,
	h *replay.Harness,
	p *externalCandidatePlan,
) (uuid.UUID, string, error) {
	spec, err := h.Generator().MacContactForSource(factory.ContactSpec{}, factory.MatchUnknown, h.MacHostID(), p.source)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("build %s ingest payload: %w", p.source, err)
	}
	if _, err := h.ReplayMacContacts(ctx, uuid.Nil, spec); err != nil {
		return uuid.Nil, "", fmt.Errorf("ingest %s candidate: %w", p.source, err)
	}
	rows, err := h.ExternalContactRepo().FindBySourceAndSourceID(ctx, p.source, spec.EntityID)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("read back %s candidate: %w", p.source, err)
	}
	if len(rows) != 1 {
		return uuid.Nil, "", fmt.Errorf("read back %s candidate %q: want exactly 1 row, got %d", p.source, spec.EntityID, len(rows))
	}
	name := ""
	if rows[0].DisplayName != nil {
		name = *rows[0].DisplayName
	}
	return rows[0].ID, name, nil
}

// seedTelegramCandidate is the telegram discovery path. NoIdentity and a missing
// TelegramHandle SUBTRACT from a fully-identified peer, because both states are
// defined by what the discovery pass did not learn.
func seedTelegramCandidate(
	ctx context.Context,
	h *replay.Harness,
	p *externalCandidatePlan,
) (uuid.UUID, string, error) {
	spec := h.Generator().TelegramDiscoveryCandidate()
	if p.noIdentity {
		spec.DisplayName, spec.FirstName, spec.LastName = "", "", ""
	}
	if !p.telegramHandle {
		spec.Username = ""
	}
	return h.SeedTelegramDiscoveryCandidate(ctx, spec)
}

// seedTitleCandidate is the anarlog_title discovery path. The manifest reports the
// display token AS STORED, because the writer title-cases it.
func seedTitleCandidate(
	ctx context.Context,
	h *replay.Harness,
	p *externalCandidatePlan,
) (uuid.UUID, string, error) {
	return h.SeedTitleCandidate(ctx, h.Generator().AnarlogTitleCandidate(p.titleToken))
}

// runMethodSuggestion links an address-book row to an EARLIER contact and leaves
// one pending method suggestion on it. The row is an external_contact like any
// declared candidate, so it records namespace ownership the same way and rides the
// same cleanup steps.
func runMethodSuggestion(
	ctx context.Context,
	h *replay.Harness,
	support *repository.SyntheticSupportRepository,
	p *methodSuggestionPlan,
	st *runState,
) (Seeded, error) {
	contactID, err := st.contactID(p.contact)
	if err != nil {
		return Seeded{}, err
	}
	spec := h.Generator().ExternalContactCandidate(p.source, 1, 0)
	// The reconciler's address-book row is the SAME person as the contact it is
	// linked to, so it carries that contact's name — which is also the name the
	// suggestion card renders.
	id, pendingEmail, err := h.SeedMethodSuggestion(ctx, contactID, spec, st.seeded[p.contact].Name)
	if err != nil {
		return Seeded{}, err
	}
	if err := support.RecordNamespaceEntity(ctx, h.Namespace(), repository.EntityKindExternalContact, id); err != nil {
		return Seeded{}, fmt.Errorf("record namespace ownership: %w", err)
	}
	// Name is the PENDING METHOD, not the row's display name: the card is keyed on
	// the linked contact's name (already in the manifest under that handle) and the
	// value under review is what a test has no other way to learn.
	return Seeded{Kind: "method_suggestion", ID: id.String(), Name: pendingEmail}, nil
}

// runMeetingNote lowers a declared orphan meeting note onto the existing seeding
// primitive. The manifest ID is the SESSION uuid rather than the row id, because
// that is the value the ?session deep link carries.
func runMeetingNote(ctx context.Context, h *replay.Harness, p *meetingNotePlan) (Seeded, error) {
	gen := h.Generator()
	title := meetingNoteTitle(gen, p.name)
	summary := noteBody(gen)
	sessionID, err := h.SeedOrphanMeetingNote(ctx, title, summary)
	if err != nil {
		return Seeded{}, err
	}
	return Seeded{Kind: "meeting_note", ID: sessionID.String(), Name: title}, nil
}

// meetingNoteTitle is the namespace-prefixed session title. The handle is part of
// it so two orphans in one declaration render distinguishably, which is what lets a
// test resolve ONE of them and assert the sibling stayed.
func meetingNoteTitle(gen *factory.Generator, handle string) string {
	return gen.Prefix() + "Session " + handle
}

// runNote lowers a declared note onto the notepad seeding primitive. The body is
// generator-derived rather than a literal, so the world stays deterministic and
// carries no authored content.
func runNote(ctx context.Context, h *replay.Harness, p *notePlan, st *runState) (Seeded, error) {
	contactID, err := st.contactID(p.contact)
	if err != nil {
		return Seeded{}, err
	}
	body := noteBody(h.Generator())
	id, err := h.SeedNote(ctx, contactID, body)
	if err != nil {
		return Seeded{}, err
	}
	// SeedNote namespace-prefixes the body it writes; the manifest reports what
	// is actually stored so a test can assert on the value it will read back.
	return Seeded{Kind: "note", ID: id.String(), Name: h.Generator().Prefix() + body}, nil
}

// noteBody is a generator-derived notepad body. gen.Note already prefixes its
// own body and SeedNote prefixes again, so the prefix is stripped here and the
// single one SeedNote applies is what lands.
func noteBody(gen *factory.Generator) string {
	return strings.TrimPrefix(gen.Note().Body, gen.Prefix())
}

// runMerge merges one earlier contact into another through the production merge
// path. Both handles must already exist, which is what lets a chain reparent
// children across two hops.
func runMerge(ctx context.Context, h *replay.Harness, p *mergePlan, st *runState) (Seeded, error) {
	loserID, err := st.contactID(p.loser)
	if err != nil {
		return Seeded{}, err
	}
	winnerID, err := st.contactID(p.winner)
	if err != nil {
		return Seeded{}, err
	}
	if err := h.MergeSeededContacts(ctx, winnerID, loserID); err != nil {
		return Seeded{}, err
	}
	return Seeded{Kind: "merge", ID: winnerID.String(), Name: st.seeded[p.winner].Name}, nil
}

// runSoftDelete tombstones an earlier contact through the production delete
// path. Declaring it after that contact's children is what produces the
// soft-deleted-parent-with-live-children state.
func runSoftDelete(ctx context.Context, h *replay.Harness, p *softDeletePlan, st *runState) (Seeded, error) {
	contactID, err := st.contactID(p.target)
	if err != nil {
		return Seeded{}, err
	}
	if err := h.SoftDeleteSeededContact(ctx, contactID); err != nil {
		return Seeded{}, err
	}
	return Seeded{Kind: "soft_delete", ID: contactID.String(), Name: st.seeded[p.target].Name}, nil
}

// runContact lowers a declared contact onto the toolkit primitives.
//
// OverdueBy lowers to a REPLAYED inbound email, never a column write: creation
// never seeds last_contacted, and only a replayed inbound/mutual interaction
// moves it, so a fixture cannot manufacture a connection that never happened.
// The message is aged period(cadence) + amount, which puts last_contacted at
// least `amount` past its due date.
//
// It ALSO backdates creation, and that is load-bearing rather than cosmetic:
// the app's due date only ever moves FORWARD on an automated interaction. A
// contact created now already has a due date one period in the FUTURE, so a
// backdated inbound would move last_contacted and leave the due date untouched
// — the contact would carry ten days of history and still not be overdue.
// Creating it before the history it carries is both the honest shape (you
// cannot have heard from someone before you added them) and the thing that
// makes the derived due date land in the past.
//
// Honest caveat, worth knowing before reading a rendered day count: the source
// safety lag is additive with the requested age. Under CRM_ENV=testing — where
// the cadence table is compressed and a "day" is ~17s — that fixed lag
// dominates, so a contact declared overdue by 3 days renders a much larger day
// count. The declared semantics are a FLOOR ("overdue by AT LEAST the stated
// amount"), which holds in every environment; the exact rendered number is only
// faithful where a real day is long relative to the lag. A fixture that must
// pin an exact day count needs a non-email history source, which the vocabulary
// does not have yet.
func runContact(
	ctx context.Context,
	h *replay.Harness,
	support *repository.SyntheticSupportRepository,
	p *contactPlan,
	st *runState,
) (Seeded, error) {
	gen := h.Generator()

	var opts []factory.ContactOption
	if p.cadence != "" {
		opts = append(opts, factory.WithCadence(p.cadence))
	}
	if age, ok := creationAge(p); ok {
		opts = append(opts, factory.WithCreatedAge(age))
	}
	if p.noMethods {
		opts = append(opts, factory.WithNoMethods())
	}
	for _, kind := range p.methods {
		switch kind {
		case MethodEmail:
			opts = append(opts, factory.WithEmail())
		case MethodPhone:
			opts = append(opts, factory.WithPhone())
		case MethodTelegram:
			opts = append(opts, factory.WithTelegram())
		}
	}
	if p.nameEdge != "" {
		opts = append(opts, factory.WithNameEdge(factory.NameEdge(p.nameEdge)))
	}
	if p.explicitNameSet {
		opts = append(opts, factory.WithExplicitName(p.explicitGiven, p.explicitSurname))
	}
	if p.location != nil {
		opts = append(opts, factory.WithLocation(prefixedLabel(gen, *p.location)))
	}
	if p.sameNameAs != "" {
		twin, ok := st.specs[p.sameNameAs]
		if !ok {
			return Seeded{}, fmt.Errorf("SameNameAs(%q): no such contact in this run", p.sameNameAs)
		}
		opts = append(opts, factory.WithNameTwinOf(twin))
	}
	if p.birthday != nil {
		if p.birthday.placeholder {
			month, day := p.birthday.placeholderMonthDay(gen.Anchor())
			opts = append(opts, factory.WithBirthday1900Sentinel(month, day))
		} else {
			opts = append(opts, factory.WithBirthday(p.birthday.resolve(gen.Anchor())))
		}
	}

	spec := gen.Contact(opts...)
	st.specs[p.name] = spec
	contact, err := h.SeedContact(ctx, spec)
	if err != nil {
		return Seeded{}, err
	}
	// Record ownership by ID before anything else can happen to the row.
	// Cleanup runs in a LATER request and otherwise recovers contacts by their
	// generated full_name — which the contact API lets a test rewrite, taking
	// node.canonical_label with it. A renamed contact would then be invisible to
	// every name-derived sweep, so cleanup would skip it and its children,
	// delete the namespace's discovery marker, and report success over live
	// residue. This record is keyed by the contact id, which nothing rewrites.
	if err := support.RecordNamespaceEntity(ctx, h.Namespace(), repository.EntityKindContact, contact.ID); err != nil {
		return Seeded{}, fmt.Errorf("record namespace ownership: %w", err)
	}

	if p.overdueBy != nil {
		message := gen.GmailMessage(spec, factory.MatchSeeded, factory.WithMessageAge(overdueMessageAge(p)))
		if _, err := h.ReplayGmail(ctx, contact.ID, message); err != nil {
			return Seeded{}, fmt.Errorf("replay overdue history: %w", err)
		}
	}

	// History drives its messages through the BATCH adapter, which settles once
	// per dependency generation instead of once per message. n single replays
	// would be n full settles, which is the difference between a usable long
	// timeline and one nobody can afford to seed.
	if p.history != nil {
		n := *p.history
		items := make([]replay.GmailBatchItem, 0, n)
		for i := 0; i < n; i++ {
			// Oldest first — the chronological order the batch adapter requires.
			message := gen.GmailMessage(spec, factory.MatchSeeded, factory.WithMessageAge(historyMessageAge(i, n)))
			items = append(items, replay.GmailBatchItem{ContactID: contact.ID, Spec: message})
		}
		if _, err := h.ReplayGmailBatch(ctx, items); err != nil {
			return Seeded{}, fmt.Errorf("replay declared history: %w", err)
		}
	}

	return Seeded{Kind: "contact", ID: contact.ID.String(), Name: contact.FullName}, nil
}

// monthDay is the calendar month/day a declared birthday names: the anchor
// shifted by the declared offset, or the explicitly declared pair.
func (b *birthdayPlan) monthDay(anchor time.Time) (time.Month, int) {
	if b.inDays != nil {
		target := anchor.UTC().AddDate(0, 0, *b.inDays)
		return target.Month(), target.Day()
	}
	return b.month, b.day
}

// placeholderMonthDay is monthDay with the ONE substitution a placeholder-year
// birthday needs: 1900 is not a leap year, so February 29 has no
// placeholder-year representation at all and is clamped to February 28 rather
// than handed to a sentinel builder that must panic on it.
//
// The clamp is what keeps SEEDING safe on every calendar day — the composed
// world executes every declaration on every reseed, so a panic here would break
// seeding itself once every four years, not merely fail one assertion. It does
// NOT make the clamped contact's rendered "today" classification correct on the
// one day it fires: the app reads February 28 as already-celebrated on a
// February 29, which is a gap in the product's own year-unknown storage
// convention. It is applied identically in the lowering and in the derived
// postcondition, so the two cannot disagree about what was stored.
//
// BirthdayOn / BirthdayInDays need no such clamp: they resolve a REAL leap-safe
// year, which represents February 29 exactly.
func (b *birthdayPlan) placeholderMonthDay(anchor time.Time) (time.Month, int) {
	month, day := b.monthDay(anchor)
	if month == time.February && day == 29 {
		return time.February, 28
	}
	return month, day
}

// resolve turns a declared birthday into a stored date. Both non-placeholder
// forms land on a LEAP-SAFE birth year, never the year-unknown 1900 sentinel:
// 1900 is not a leap year, so a February 29 birthday stored against it silently
// becomes March 1.
func (b *birthdayPlan) resolve(anchor time.Time) time.Time {
	month, day := b.monthDay(anchor)
	return time.Date(factory.LeapSafeBirthYear(anchor), month, day, 0, 0, 0, 0, time.UTC)
}

// resolvePlaceholder turns a declared PLACEHOLDER birthday into the stored
// year-unknown sentinel date, clamp included.
func (b *birthdayPlan) resolvePlaceholder(anchor time.Time) time.Time {
	month, day := b.placeholderMonthDay(anchor)
	return time.Date(factory.SentinelBirthYear, month, day, 0, 0, 0, 0, time.UTC)
}

// prefixedLabel namespace-prefixes a caller-supplied label. The prefix is
// load-bearing rather than cosmetic: the place node the location write
// auto-creates carries this label, and the entity teardown's label-prefix sweep
// is the only thing that deletes it.
func prefixedLabel(gen *factory.Generator, s string) string {
	return gen.Prefix() + s
}

// overdueMessageAge is how far before the anchor the history message is dated:
// one full cadence period (which is what makes the contact due at all) plus the
// declared overdue amount. As a distance from the anchor this is exactly the
// backdate the bespoke overdue seeder computed —
// cadence_duration + days × (weekly / 7) — derived here from the locally stated
// facts rather than from the app's cadence math.
func overdueMessageAge(p *contactPlan) time.Duration {
	return p.overdueBy.resolve(p.cadence) + period(p.cadence)
}

// creationAge is how far before the anchor the contact is created, and whether
// creation should be backdated at all.
//
// Declared explicitly: the declaration's own amount.
// Derived (OverdueBy): the history's realized instant — the requested message
// age plus the source's fixed safety lag — plus one further period of margin.
// The margin is what makes the app's forward-only due-date update actually
// MOVE: the create-time due date sits one period after creation, so creation
// must precede the replayed instant by more than that or the update is a no-op
// and the contact is never overdue. One period is the smallest margin that
// guarantees a strict forward move for any declared amount.
func creationAge(p *contactPlan) (time.Duration, bool) {
	if p.createdAgo != nil {
		return p.createdAgo.resolve(p.cadence), true
	}
	if p.overdueBy != nil {
		return overdueMessageAge(p) + sourceHistoryLag + period(p.cadence), true
	}
	if p.history != nil {
		// Derived (History): the OLDEST message's realized instant — its
		// requested age plus the source's fixed safety lag — plus a strictly
		// positive margin, so creation precedes the first email rather than
		// coinciding with it.
		return historyMessageAge(0, *p.history) + sourceHistoryLag + historyCreationMargin(), true
	}
	return 0, false
}
