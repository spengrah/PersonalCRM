package service

import (
	"context"
	"sort"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// daemonFamily is the single descriptor for one daemon-emitted event
// family (raw_message.*, external_contact.*, meeting_note.*, call.*).
// One entry per family in daemonFamilies. The table is the source of
// truth for the host-auth allowlist, the inline-dispatch routing, the
// per-family invariant verifier, and the per-family allowed envelope
// source set. Pieces in other packages (events.AllKinds,
// mac.AllowedPushSources, the interaction.source SQL CHECK, the provider
// stubs) are cross-checked against this table by the agreement tests
// rather than driven from it — the import graph forbids inverting those
// dependencies into this package.
type daemonFamily struct {
	// name is a human label, e.g. "raw_message".
	name string
	// kinds are the host-only kinds in this family. raw_message.*,
	// external_contact.*, meeting_note.*, and call.* each have 2 kinds.
	kinds []events.Kind
	// allowedSources is the set of envelope.Source values this family
	// accepts. external_contact.* accepts two sources (icloud_contacts,
	// anarlog_humans); the other three families accept exactly one. The
	// per-family verify function reads this set instead of a package-var
	// or an inline literal.
	allowedSources map[string]struct{}
	// providerStubSources is the subset of allowedSources that MUST have
	// a push ProviderRegistry stub registered (messages, icloud_contacts,
	// phone_calls). The anarlog sources are deliberately absent — they
	// have no stub by design. The agreement test cross-checks this subset
	// against the live registry built by push.RegisterPushProviders().
	providerStubSources map[string]struct{}
	// writesInteractions is true iff this family produces interaction
	// rows (raw_message / meeting_note / call); false for
	// external_contact. Paired with interactionSource:
	// writesInteractions == true REQUIRES a non-empty interactionSource
	// and vice versa.
	writesInteractions bool
	// interactionSource is the repository.InteractionSource* value this
	// family's interactions carry; "" when writesInteractions is false.
	interactionSource string
	// verify runs the family's cross-field invariant checks before the
	// per-event savepoint opens. Returns a non-nil rejection (caller fills
	// in Index) on any violation.
	verify func(env *events.Envelope, hostID uuid.UUID) *IngestPerEventRejection
	// dispatch runs the family's inline handler inside the per-event
	// savepoint. The return tuple is the union of what the four families
	// produce; each family populates only its relevant fields:
	//   - contactID: raw_message aggregator enqueue key; nil otherwise.
	//   - needsAttention: meeting_note.recorded only; nil otherwise.
	//   - postCommits: call + meeting_note follow-up closures; nil
	//     otherwise.
	//   - rejection: non-nil on a domain refusal (caller rolls back the
	//     savepoint).
	dispatch func(s *IngestService, ctx context.Context, sp pgx.Tx, env *events.Envelope, hostID uuid.UUID) (
		contactID *uuid.UUID,
		needsAttention *NeedsAttentionItem,
		postCommits []func(context.Context),
		rejection *IngestPerEventRejection,
	)
}

// Per-family allowed-source sets. Defined as standalone vars (rather
// than inline in the family literals) so the verify functions can read
// them directly without creating a package-init cycle: a verify function
// references its source set, and the family literal references the same
// set AND the verify function — referencing the set var directly keeps
// the function out of the family's init dependency chain. The family's
// allowedSources field points at the same map, so there is exactly one
// source-of-truth set per family.
var (
	rawMessageAllowedSources      = map[string]struct{}{"messages": {}}
	externalContactAllowedSources = map[string]struct{}{"icloud_contacts": {}, "anarlog_humans": {}}
	meetingNoteAllowedSources     = map[string]struct{}{"anarlog_sessions": {}}
	callAllowedSources            = map[string]struct{}{repository.InteractionSourcePhoneCalls: {}}
)

// rawMessageFamily descriptor — raw_message.received / raw_message.sent.
// Source "messages" (the only raw_message source today). Interactions
// are written asynchronously by the aggregator using
// InteractionSourceMessages, so the family writes interactions even
// though handleRawMessage itself only stages.
var rawMessageFamily = daemonFamily{
	name:                "raw_message",
	kinds:               []events.Kind{events.KindRawMessageReceived, events.KindRawMessageSent},
	allowedSources:      rawMessageAllowedSources,
	providerStubSources: map[string]struct{}{"messages": {}},
	writesInteractions:  true,
	interactionSource:   repository.InteractionSourceMessages,
	verify:              verifyRawMessageInvariants,
	dispatch: func(s *IngestService, ctx context.Context, sp pgx.Tx, env *events.Envelope, hostID uuid.UUID) (
		*uuid.UUID, *NeedsAttentionItem, []func(context.Context), *IngestPerEventRejection,
	) {
		contactID, rejection := s.handleRawMessage(ctx, sp, env, hostID)
		return contactID, nil, nil, rejection
	},
}

// externalContactFamily descriptor — external_contact.upserted /
// external_contact.deleted. Accepts two sources (icloud_contacts has a
// provider stub; anarlog_humans does not). Produces no interaction rows.
var externalContactFamily = daemonFamily{
	name:                "external_contact",
	kinds:               []events.Kind{events.KindExternalContactUpserted, events.KindExternalContactDeleted},
	allowedSources:      externalContactAllowedSources,
	providerStubSources: map[string]struct{}{"icloud_contacts": {}},
	writesInteractions:  false,
	interactionSource:   "",
	verify:              verifyExternalContactInvariants,
	dispatch: func(s *IngestService, ctx context.Context, sp pgx.Tx, env *events.Envelope, hostID uuid.UUID) (
		*uuid.UUID, *NeedsAttentionItem, []func(context.Context), *IngestPerEventRejection,
	) {
		switch env.Kind {
		case events.KindExternalContactUpserted:
			return nil, nil, nil, s.handleExternalContactUpserted(ctx, sp, env, hostID)
		case events.KindExternalContactDeleted:
			return nil, nil, nil, s.handleExternalContactDeleted(ctx, sp, env, hostID)
		default:
			return nil, nil, nil, &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvariant,
				Message: "external_contact dispatch: unexpected kind",
			}
		}
	},
}

// meetingNoteFamily descriptor — meeting_note.recorded /
// meeting_note.deleted. Source "anarlog_sessions" has NO provider stub by
// design, so providerStubSources is empty. recorded writes session-
// attributed interactions (InteractionSourceAnarlogSessions).
var meetingNoteFamily = daemonFamily{
	name:                "meeting_note",
	kinds:               []events.Kind{events.KindMeetingNoteRecorded, events.KindMeetingNoteDeleted},
	allowedSources:      meetingNoteAllowedSources,
	providerStubSources: map[string]struct{}{},
	writesInteractions:  true,
	interactionSource:   repository.InteractionSourceAnarlogSessions,
	verify:              verifyMeetingNoteInvariants,
	dispatch: func(s *IngestService, ctx context.Context, sp pgx.Tx, env *events.Envelope, hostID uuid.UUID) (
		*uuid.UUID, *NeedsAttentionItem, []func(context.Context), *IngestPerEventRejection,
	) {
		switch env.Kind {
		case events.KindMeetingNoteRecorded:
			naItem, followUps, rejection := s.handleMeetingNoteRecorded(ctx, sp, env, hostID)
			return nil, naItem, followUps, rejection
		case events.KindMeetingNoteDeleted:
			return nil, nil, nil, s.handleMeetingNoteDeleted(ctx, sp, env, hostID)
		default:
			return nil, nil, nil, &IngestPerEventRejection{
				Code:    ingestRejectPayloadInvariant,
				Message: "meeting_note dispatch: unexpected kind",
			}
		}
	},
}

// callFamily descriptor — call.received / call.sent. Source "phone_calls"
// has a provider stub. Writes interactions (InteractionSourcePhoneCalls).
// The kind→isOutbound decision (KindCallSent) lives entirely inside this
// dispatch wrapper so IngestBatch carries no kind reference.
var callFamily = daemonFamily{
	name:                "call",
	kinds:               []events.Kind{events.KindCallReceived, events.KindCallSent},
	allowedSources:      callAllowedSources,
	providerStubSources: map[string]struct{}{repository.InteractionSourcePhoneCalls: {}},
	writesInteractions:  true,
	interactionSource:   repository.InteractionSourcePhoneCalls,
	verify:              verifyCallInvariants,
	dispatch: func(s *IngestService, ctx context.Context, sp pgx.Tx, env *events.Envelope, hostID uuid.UUID) (
		*uuid.UUID, *NeedsAttentionItem, []func(context.Context), *IngestPerEventRejection,
	) {
		isOutbound := env.Kind == events.KindCallSent
		postCommit, rejection := s.handleCall(ctx, sp, env, hostID, isOutbound)
		if rejection != nil {
			return nil, nil, nil, rejection
		}
		if postCommit != nil {
			return nil, nil, []func(context.Context){postCommit}, nil
		}
		return nil, nil, nil, nil
	},
}

// daemonFamilies is the single source of truth for the daemon-push event
// families. Built once at package init.
var daemonFamilies = []daemonFamily{
	rawMessageFamily,
	externalContactFamily,
	meetingNoteFamily,
	callFamily,
}

// kindToFamily indexes every family kind → its descriptor. Built from
// daemonFamilies at package init. A kind appears in at most one family
// (enforced by the agreement test's partition check).
var kindToFamily = buildKindToFamilyIndex(daemonFamilies)

// buildKindToFamilyIndex flattens daemonFamilies into a kind→descriptor
// lookup. Panics on a duplicate kind across families — a programming
// error in the table definition that must fail loudly at startup.
func buildKindToFamilyIndex(families []daemonFamily) map[events.Kind]*daemonFamily {
	index := make(map[events.Kind]*daemonFamily)
	for i := range families {
		fam := &families[i]
		for _, k := range fam.kinds {
			if _, dup := index[k]; dup {
				panic("daemonFamilies: kind " + string(k) + " appears in more than one family")
			}
			index[k] = fam
		}
	}
	return index
}

// DaemonFamilyView is a read-only, copy-safe projection of one daemon
// event family descriptor, exported solely for cross-package agreement
// tests and read-only diagnostics. It must NOT be used as production
// routing input outside the service package — routing goes through the
// internal kindToFamily table. All slices are defensive copies and
// deterministically sorted (the internal allowedSources /
// providerStubSources are Go maps with randomized iteration order, so
// the accessor sorts every copied slice — AllowedSources /
// ProviderStubSources by string, Kinds by string) so callers can
// deep-equal against a sorted expected literal without flakiness.
type DaemonFamilyView struct {
	Name                string
	Kinds               []events.Kind // sorted
	AllowedSources      []string      // sorted
	ProviderStubSources []string      // sorted
	WritesInteractions  bool
	InteractionSource   string
}

// DaemonFamilyViews returns a defensive, deterministically-sorted copy of
// the descriptor table (families ordered by Name). The internal
// daemonFamily struct (with the verify/dispatch func fields) stays
// unexported; only this data-shaped view is exported so both the
// service_test agreement test and the backend/tests integration test can
// read the table without seeing routing internals.
func DaemonFamilyViews() []DaemonFamilyView {
	views := make([]DaemonFamilyView, 0, len(daemonFamilies))
	for i := range daemonFamilies {
		fam := &daemonFamilies[i]

		kinds := make([]events.Kind, len(fam.kinds))
		copy(kinds, fam.kinds)
		sort.Slice(kinds, func(a, b int) bool { return kinds[a] < kinds[b] })

		views = append(views, DaemonFamilyView{
			Name:                fam.name,
			Kinds:               kinds,
			AllowedSources:      sortedKeys(fam.allowedSources),
			ProviderStubSources: sortedKeys(fam.providerStubSources),
			WritesInteractions:  fam.writesInteractions,
			InteractionSource:   fam.interactionSource,
		})
	}
	sort.Slice(views, func(a, b int) bool { return views[a].Name < views[b].Name })
	return views
}

// sortedKeys returns the keys of a set map as a sorted slice. Returns an
// empty (non-nil) slice for an empty map so DeepEqual comparisons against
// an empty expected literal are stable.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
