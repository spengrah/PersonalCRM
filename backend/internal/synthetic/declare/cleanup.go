package declare

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
)

// Per-namespace cleanup outcomes. Counts plus a single error cannot encode
// partial success, so each namespace reports its own status.
const (
	// StatusCleaned: the ladder ran to completion; Deleted carries the counts.
	StatusCleaned = "cleaned"
	// StatusBusy: an in-flight run holds the namespace reservation. Nothing was
	// deleted; retriable.
	StatusBusy = "busy"
	// StatusPending: unfinalized River jobs still reference the namespace's
	// rows. Nothing was deleted; retriable once they finalize.
	StatusPending = "pending"
	// StatusError: the sweep refused or failed. Nothing was deleted, or the
	// ladder stopped part way with the namespace's marker deliberately retained.
	StatusError = "error"
)

// NamespaceCleanup is one effective namespace's outcome.
type NamespaceCleanup struct {
	Status      string           `json:"status"`
	Deleted     map[string]int64 `json:"deleted,omitempty"`
	Descendants []string         `json:"descendants,omitempty"`
	Err         string           `json:"error,omitempty"`
}

// CleanupResult reports what each REQUESTED token expanded to and what happened
// to each EFFECTIVE namespace.
type CleanupResult struct {
	Expansions map[string][]string         `json:"expansions"`
	Results    map[string]NamespaceCleanup `json:"results"`
}

// maxCleanupNamespaces bounds one cleanup request so a repeated call cannot
// multiply DB work.
const maxCleanupNamespaces = 32

// maxSaltAttempt is the highest salt suffix re-salting can mint. The toolkit
// budgets 8 attempts but never uses attempt 8's candidate, so -s7 is the
// ceiling; expansion probes exactly that range.
const maxSaltAttempt = 7

var saltedNamespace = regexp.MustCompile(`-s\d+$`)

// CleanupNamespaces removes the worlds a set of REQUESTED namespace tokens
// resolved to.
//
// Statelessness is the whole point: the seed happened in an earlier request and
// took its in-memory id ledger with it, so every id set here is rebuilt from
// the namespace's own generator-derived tokens.
//
// The protocol mirrors the seed side:
//
//	expand + canonicalize — a requested token may have been re-salted into
//	   <ns>-sN, and the client may never have seen the response that said so, so
//	   each token expands to itself plus its live salted variants. The union is
//	   deduplicated into EFFECTIVE namespaces, so a client sending both the
//	   requested and effective values for one world gets one lock, one ladder,
//	   and one result — never a double acquisition that strands a counted lock.
//	reserve — the same non-blocking advisory lock the seed takes. A held lock
//	   means a seed is in flight: report busy, delete nothing.
//	descendant guard — refuse rather than LIKE-sweep across a live nested world.
//	pending gate — refuse while any unfinalized River job still dereferences
//	   the namespace's rows.
//	ladder — FK-ordered deletes, with the host marker deleted LAST and only on a
//	   clean run, so a partial failure leaves the namespace discoverable and
//	   occupied instead of stranding unreachable residue.
func CleanupNamespaces(ctx context.Context, database *db.Database, namespaces []string, seed uint64) (CleanupResult, error) {
	res := CleanupResult{Expansions: map[string][]string{}, Results: map[string]NamespaceCleanup{}}
	if err := ValidateCleanupNamespaces(namespaces); err != nil {
		return res, err
	}

	support := repository.NewSyntheticSupportRepository(database.Queries)
	effectiveSet := map[string]bool{}
	for _, ns := range namespaces {
		expanded, err := expandNamespace(ctx, support, ns)
		if err != nil {
			return res, err
		}
		res.Expansions[ns] = expanded
		for _, e := range expanded {
			effectiveSet[e] = true
		}
	}

	effective := make([]string, 0, len(effectiveSet))
	for ns := range effectiveSet {
		effective = append(effective, ns)
	}
	sort.Strings(effective)

	for _, ns := range effective {
		res.Results[ns] = cleanNamespace(ctx, database, support, ns, seedOrDefault(seed))
	}
	return res, nil
}

// ValidateCleanupNamespaces checks the request bounds WITHOUT touching the
// database, so a caller can reject at its own boundary. Repeated cleanup must
// not multiply DB work, hence the size cap and the duplicate rejection; the
// token grammar is the laxer one, because cleanup legitimately receives
// EFFECTIVE namespaces that carry a -sN suffix.
func ValidateCleanupNamespaces(namespaces []string) error {
	if len(namespaces) == 0 {
		return fmt.Errorf("%w: no namespaces given", ErrInvalidNamespace)
	}
	if len(namespaces) > maxCleanupNamespaces {
		return fmt.Errorf("%w: %d namespaces exceeds the limit of %d", ErrInvalidNamespace, len(namespaces), maxCleanupNamespaces)
	}
	seen := map[string]bool{}
	for _, ns := range namespaces {
		if err := ValidateNamespaceToken(ns); err != nil {
			return err
		}
		if seen[ns] {
			return fmt.Errorf("%w: %q appears more than once", ErrInvalidNamespace, ns)
		}
		seen[ns] = true
	}
	return nil
}

// expandNamespace maps a REQUESTED token to the EFFECTIVE namespaces it names —
// the worlds that actually exist under it.
//
// A world is identified by its host marker, so the requested token is an
// effective namespace only when it carries one of its own. When it re-salted
// away, the salted variants ARE the effective namespaces and the requested token
// is not a second world: the client's normal [requested, effective] pair must
// canonicalize to ONE entry, one lock, one ladder run.
//
// The exception is the empty case: a token that resolved to nothing at all still
// expands to itself, so a lost-response cleanup gets a real (no-op) sweep and a
// reportable outcome instead of silence — and so an in-flight seed that has not
// published its marker yet is still reported busy rather than missing. That
// sweep is also the backstop for marker-less residue: its 'synth-<ns>-' prefix
// is a superset of every salted variant's, so rows whose marker was lost are
// still reachable by the requested token.
func expandNamespace(ctx context.Context, support *repository.SyntheticSupportRepository, namespace string) ([]string, error) {
	// A token that is ALREADY a salted variant cannot have salted children:
	// re-salting appends to the ORIGINAL token (foo → foo-s1, foo-s2), never to
	// a salted one. Probing would be harmless but pointless.
	if saltedNamespace.MatchString(namespace) {
		return []string{namespace}, nil
	}

	hasMarker := func(token string) (bool, error) {
		_, exists, err := support.SelectMacHostIDByHostname(ctx, factory.SyntheticSourcePrefix+token+"-host")
		if err != nil {
			return false, fmt.Errorf("declare: expand namespace %q: %w", namespace, err)
		}
		return exists, nil
	}

	var variants []string
	for i := 1; i <= maxSaltAttempt; i++ {
		candidate := fmt.Sprintf("%s-s%d", namespace, i)
		exists, err := hasMarker(candidate)
		if err != nil {
			return nil, err
		}
		if exists {
			variants = append(variants, candidate)
		}
	}

	self, err := hasMarker(namespace)
	if err != nil {
		return nil, err
	}
	if self || len(variants) == 0 {
		return append([]string{namespace}, variants...), nil
	}
	return variants, nil
}

// cleanNamespace runs the guards and the ladder for ONE effective namespace.
func cleanNamespace(
	ctx context.Context,
	database *db.Database,
	support *repository.SyntheticSupportRepository,
	namespace string,
	seed uint64,
) NamespaceCleanup {
	locks, err := newLockSession(ctx, database)
	if err != nil {
		return NamespaceCleanup{Status: StatusError, Err: err.Error()}
	}
	defer locks.close()

	acquired, err := locks.tryLock(ctx, advisoryKey("declare:"+namespace))
	if err != nil {
		return NamespaceCleanup{Status: StatusError, Err: fmt.Sprintf("reserve namespace: %v", err)}
	}
	if !acquired {
		return NamespaceCleanup{Status: StatusBusy}
	}

	gen := factory.NewGenerator(seed, namespace)
	prefix := gen.Prefix()

	// Descendant guard. A namespace's sweep is prefix-scoped, so a LIVE nested
	// world under it would be destroyed as collateral. Salt variants of this
	// namespace are expansion members, not descendants, and are excluded.
	descendants, err := liveDescendants(ctx, support, namespace, prefix)
	if err != nil {
		return NamespaceCleanup{Status: StatusError, Err: err.Error()}
	}
	if len(descendants) > 0 {
		return NamespaceCleanup{
			Status:      StatusError,
			Descendants: descendants,
			Err:         fmt.Sprintf("refusing to sweep %q: %d live namespace(s) are nested under it", namespace, len(descendants)),
		}
	}

	contactIDs, err := support.SelectContactIDsByFullNamePrefix(ctx, prefix)
	if err != nil {
		return NamespaceCleanup{Status: StatusError, Err: fmt.Sprintf("select contacts: %v", err)}
	}
	eventIDs, err := namespaceEventIDs(ctx, support, contactIDs, prefix)
	if err != nil {
		return NamespaceCleanup{Status: StatusError, Err: err.Error()}
	}

	// Pending gate, at Gate-B parity. Deleting rows an unfinalized job still
	// dereferences can fault whichever worker later picks that job up.
	pending, err := support.CountPendingJobsForNamespaceCleanup(ctx, eventIDs, contactIDs)
	if err != nil {
		return NamespaceCleanup{Status: StatusError, Err: fmt.Sprintf("pending-job check: %v", err)}
	}
	if pending > 0 {
		return NamespaceCleanup{Status: StatusPending}
	}

	return runLadder(ctx, support, gen, namespace, contactIDs, eventIDs)
}

// liveDescendants returns the hostnames of namespaces nested under this one,
// with this namespace's own salt variants filtered out.
func liveDescendants(ctx context.Context, support *repository.SyntheticSupportRepository, namespace, prefix string) ([]string, error) {
	hostnames, err := support.SelectLiveDescendantHostnames(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("descendant guard for %q: %w", namespace, err)
	}
	var out []string
	for _, hostname := range hostnames {
		token := strings.TrimSuffix(strings.TrimPrefix(hostname, factory.SyntheticSourcePrefix), "-host")
		if token == namespace || isSaltVariantOf(token, namespace) {
			continue // this namespace itself, or one of its expansion members
		}
		out = append(out, hostname)
	}
	return out, nil
}

// isSaltVariantOf reports whether token is exactly "<namespace>-sN" — a variant
// re-salting minted for this namespace, which cleanup expands to and therefore
// owns. Anything else under the prefix (including "<namespace>-s1-more") is a
// genuine descendant.
func isSaltVariantOf(token, namespace string) bool {
	rest, ok := strings.CutPrefix(token, namespace+"-")
	return ok && saltSuffix.MatchString(rest)
}

// namespaceEventIDs is the UNION of the two event classes a declaration can
// produce: cascade events referencing one of its contacts, and adapter-direct
// ROOT events keyed by the namespace-prefixed source_id (an inbound email root
// carries no contact id, so the contact-scoped query alone would miss it and
// both leak the row and break idempotency for a later same-namespace replay).
func namespaceEventIDs(
	ctx context.Context,
	support *repository.SyntheticSupportRepository,
	contactIDs []uuid.UUID,
	prefix string,
) ([]uuid.UUID, error) {
	seen := map[uuid.UUID]bool{}
	var union []uuid.UUID
	add := func(ids []uuid.UUID) {
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			union = append(union, id)
		}
	}

	cascade, err := support.ListEventIdsForContacts(ctx, contactIDs)
	if err != nil {
		return nil, fmt.Errorf("capture cascade event ids: %w", err)
	}
	add(cascade)

	for _, source := range declaredRootEventSources {
		roots, err := support.ListEventIdsBySourceAndSourceIDPrefix(ctx, source, prefix)
		if err != nil {
			return nil, fmt.Errorf("capture root event ids for source %q: %w", source, err)
		}
		add(roots)
	}
	return union, nil
}

// declaredRootEventSources are the sources whose adapters publish ns-prefixed
// ROOT events for the entity kinds the vocabulary can currently declare. It
// grows with the vocabulary: every declarable class owes a namespace-recoverable
// cleanup step in the PR that makes it declarable.
var declaredRootEventSources = []string{repository.InteractionSourceEmail}

// runLadder executes the FK-ordered deletes and returns per-class counts.
//
// Best-effort per step (record and continue, first error wins), mirroring the
// harness's own teardown: one failing table must not strand the rest.
//
// MARKER RETENTION: step "mac_host" — and the meeting notes hanging off it —
// runs ONLY when every prior step succeeded. The host row is how expansion and
// the descendant guard DISCOVER this namespace, so deleting it after a partial
// sweep would make the residue unreachable by token forever. Keeping it leaves
// the namespace both discoverable and occupied, so a retry is possible and a
// re-seed is refused.
func runLadder(
	ctx context.Context,
	support *repository.SyntheticSupportRepository,
	gen *factory.Generator,
	namespace string,
	contactIDs []uuid.UUID,
	eventIDs []uuid.UUID,
) NamespaceCleanup {
	prefix := gen.Prefix()
	deleted := map[string]int64{"mac_hosts": 0, "meeting_notes": 0}
	var firstErr error

	failStep := currentCleanupFailStep()
	step := func(label string, fn func() (int64, error)) {
		if failStep == label {
			if firstErr == nil {
				firstErr = fmt.Errorf("cleanup %s: injected failure", label)
			}
			return
		}
		n, err := fn()
		deleted[label] += n
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cleanup %s: %w", label, err)
		}
	}

	// Captured BEFORE the interaction delete removes the only link to them.
	venueIDs, err := support.SelectVenueNodeIDsForContacts(ctx, contactIDs)
	if err != nil {
		return NamespaceCleanup{Status: StatusError, Err: fmt.Sprintf("select venue nodes: %v", err)}
	}

	step("event_consumer_claims", func() (int64, error) {
		return support.DeleteEventConsumerClaimsByEventIds(ctx, eventIDs)
	})
	step("interactions", func() (int64, error) {
		return support.DeleteInteractionsByContactIds(ctx, contactIDs)
	})
	step("events", func() (int64, error) {
		return support.DeleteEventsByIds(ctx, eventIDs)
	})
	step("comms_messages", func() (int64, error) {
		return support.DeleteCommsMessagesByExternalIDPrefix(ctx, prefix)
	})
	step("messages_messages", func() (int64, error) {
		return support.DeleteMessagesMessageByGuidPrefix(ctx, prefix)
	})
	step("calendar_events", func() (int64, error) {
		return support.DeleteCalendarEventsByGcalEventIDPrefix(ctx, prefix)
	})
	// external_identity survives a contact delete via ON DELETE SET NULL, so it
	// must be cleared FIRST or it pollutes future matching. Three sweeps: the
	// string identifiers, the namespace's phone digits, and the source_id backstop.
	step("external_identities", func() (int64, error) {
		return support.DeleteExternalIdentitiesByIdentifierPrefix(ctx, prefix)
	})
	step("external_identities", func() (int64, error) {
		return support.DeleteExternalIdentitiesByIdentifierPrefix(ctx, gen.SyntheticPhonePrefix())
	})
	step("external_identities", func() (int64, error) {
		return support.DeleteExternalIdentitiesBySourceIDPrefix(ctx, prefix)
	})
	step("external_contacts", func() (int64, error) {
		return support.DeleteExternalContactsBySourceIDPrefix(ctx, prefix)
	})
	step("contact_tasks", func() (int64, error) {
		return support.DeleteContactTasksByContactIds(ctx, contactIDs)
	})
	step("contact_methods", func() (int64, error) {
		return support.DeleteContactMethodsByContactIds(ctx, contactIDs)
	})
	step("notes", func() (int64, error) {
		return support.DeleteNotesByContactIds(ctx, contactIDs)
	})
	step("contacts", func() (int64, error) {
		return support.DeleteContactsByIds(ctx, contactIDs)
	})
	// The assertion→node FK is RESTRICT, so assertions go before the nodes.
	step("assertions", func() (int64, error) {
		var total int64
		for _, contactID := range contactIDs {
			n, err := support.DeleteAssertionsForNode(ctx, contactID)
			total += n
			if err != nil {
				return total, err
			}
		}
		return total, nil
	})
	step("nodes", func() (int64, error) {
		return support.DeleteNodesByIds(ctx, contactIDs)
	})
	step("nodes", func() (int64, error) {
		return support.DeleteNodesByLabelPrefix(ctx, prefix)
	})
	// Venue nodes carry an empty canonical_label, so neither node sweep above
	// finds them. The delete keeps its NOT-EXISTS-interaction guard.
	step("venue_nodes", func() (int64, error) {
		return support.DeleteVenueNodesByIds(ctx, venueIDs)
	})

	hostID, hostExists, err := support.SelectMacHostIDByHostname(ctx, prefix+"host")
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("cleanup host lookup: %w", err)
	}

	if firstErr != nil {
		return NamespaceCleanup{
			Status: StatusError,
			Err: fmt.Sprintf("%v (namespace %q left discoverable and occupied so a retry can finish it)",
				firstErr, namespace),
		}
	}

	if hostExists {
		if err := support.DeleteMeetingNotesByHostID(ctx, hostID); err != nil {
			return NamespaceCleanup{
				Status: StatusError,
				Err:    fmt.Sprintf("cleanup meeting_notes: %v (namespace %q left discoverable)", err, namespace),
			}
		}
		n, err := support.DeleteMacHostByID(ctx, hostID)
		if err != nil {
			return NamespaceCleanup{
				Status: StatusError,
				Err:    fmt.Sprintf("cleanup mac_host: %v (namespace %q left discoverable)", err, namespace),
			}
		}
		deleted["mac_hosts"] = n
	}

	return NamespaceCleanup{Status: StatusCleaned, Deleted: deleted}
}
