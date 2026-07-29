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
	// Anchor is the generator anchor the world was built against. Postconditions
	// time against it, two-sided.
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

// maxNamespaceLen bounds a namespace token. The salt suffix and the
// 'synth-<ns>-' prefix are added on top, so this leaves ample room under any
// column width.
const maxNamespaceLen = 60

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

// ValidateRequestedNamespace is ValidateNamespaceToken plus the reserved salt
// grammar: a REQUESTED namespace may not end in -sN, because that is the suffix
// re-salting mints internally. Reserving it is what makes cleanup's
// requested → salted-variant expansion unambiguous.
func ValidateRequestedNamespace(namespace string) error {
	if err := ValidateNamespaceToken(namespace); err != nil {
		return err
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

// RunDeclarationForTest executes a declaration VALUE rather than a registry
// key. The satisfiability suite lives in backend/tests — an external package —
// and needs to exercise vocabulary combinations that are not (and must not be)
// registered under a spec behavior id. Same Result/RunError semantics as Run.
func RunDeclarationForTest(ctx context.Context, database *db.Database, d Declaration, namespace string, seed uint64) (Result, error) {
	requireTestEnv("declare.RunDeclarationForTest")
	for _, e := range d.Entities {
		if e == nil {
			return Result{}, errors.New("declare: declaration carries a nil entity")
		}
		if err := e.validate(); err != nil {
			return Result{}, fmt.Errorf("declare: %w", err)
		}
	}
	if len(d.Entities) == 0 {
		return Result{}, errors.New("declare: declaration carries no entities")
	}
	if err := ValidateRequestedNamespace(namespace); err != nil {
		return Result{}, err
	}
	return execute(ctx, database, d, namespace, seedOrDefault(seed))
}

func seedOrDefault(seed uint64) uint64 {
	if seed == 0 {
		return factory.DefaultSeed
	}
	return seed
}

// execute is the full protocol: reserve, claim bands, construct, run, drain.
func execute(parent context.Context, database *db.Database, d Declaration, namespace string, seed uint64) (Result, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), runBudget())
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

	res, err := runEntities(ctx, h, d)
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
	ctx, cancel := context.WithTimeout(context.Background(), teardownBudget())
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
//	hierarchical — a live host for any proper hyphen-boundary ANCESTOR of the
//	               namespace. This closes the foo/foo-bar hole: foo-bar's rows are
//	               invisible to foo's own occupancy check, but a later cleanup of
//	               foo would LIKE-sweep 'synth-foo-%' straight across them. The
//	               ancestor direction is already closed by the direct check.
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

		if hook := currentHook(HookAfterBandSwapBeforeRevalidate); hook != nil {
			if err := hook(ctx, h); err != nil {
				// Terminal path: the deferred close settles whatever is held.
				_ = locks.unlockAll(effectiveKeys)
				return nil, nil, teardownError(teardown, effective,
					fmt.Errorf("declare: %s hook: %w", HookAfterBandSwapBeforeRevalidate, err))
			}
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

// runEntities executes a declaration's entities in declared order, then drains
// Gate B. Success therefore means "quiescent", not merely "written".
func runEntities(ctx context.Context, h *replay.Harness, d Declaration) (Result, error) {
	res := Result{
		Namespace: h.Namespace(),
		Anchor:    h.Generator().Anchor(),
		Entities:  make(map[string]Seeded, len(d.Entities)),
	}
	for i, e := range d.Entities {
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("declare: run budget expired before entity %q: %w", e.handle(), err)
		}
		seeded, err := runEntity(ctx, h, e)
		if err != nil {
			return Result{}, fmt.Errorf("declare: entity %q: %w", e.handle(), err)
		}
		res.Entities[e.handle()] = seeded
		if i == 0 && currentFailpoint() == FailpointAfterFirstEntity {
			return Result{}, fmt.Errorf("declare: failpoint %q fired after entity %q", FailpointAfterFirstEntity, e.handle())
		}
	}

	if hook := currentHook(HookAfterReplayBeforeDrain); hook != nil {
		if err := hook(ctx, h); err != nil {
			return Result{}, fmt.Errorf("declare: %s hook: %w", HookAfterReplayBeforeDrain, err)
		}
	}
	if err := h.DrainGateB(ctx); err != nil {
		return Result{}, err
	}
	return res, nil
}

func runEntity(ctx context.Context, h *replay.Harness, e Entity) (Seeded, error) {
	switch p := e.(type) {
	case *contactPlan:
		return runContact(ctx, h, p)
	default:
		return Seeded{}, fmt.Errorf("unsupported entity kind %q", e.kind())
	}
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
func runContact(ctx context.Context, h *replay.Harness, p *contactPlan) (Seeded, error) {
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

	spec := gen.Contact(opts...)
	contact, err := h.SeedContact(ctx, spec)
	if err != nil {
		return Seeded{}, err
	}

	if p.overdueBy != nil {
		message := gen.GmailMessage(spec, factory.MatchSeeded, factory.WithMessageAge(overdueMessageAge(p)))
		if _, err := h.ReplayGmail(ctx, contact.ID, message); err != nil {
			return Seeded{}, fmt.Errorf("replay overdue history: %w", err)
		}
	}

	return Seeded{Kind: "contact", ID: contact.ID.String(), Name: contact.FullName}, nil
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
	return 0, false
}
