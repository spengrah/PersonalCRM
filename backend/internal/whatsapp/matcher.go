package whatsapp

import (
	"context"
	"fmt"
	"strings"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
)

// identityMatcher is the slice of service.IdentityService the matcher uses.
type identityMatcher interface {
	MatchOrCreate(ctx context.Context, req service.MatchRequest) (*service.MatchResult, error)
}

// commsPeerStore is the slice of CommsMessageRepository the peer paths use.
type commsPeerStore interface {
	AttachUnmatchedByPeer(ctx context.Context, source string, peerNormalized, peerHandle *string, contactID uuid.UUID) (int64, int64, error)
	ListUnmatchedPeerCounts(ctx context.Context, source string, peerHandle *string, minMessages int) ([]repository.UnmatchedPeerCount, error)
}

// externalContactUpserter is the slice of ExternalContactRepository discovery
// needs.
type externalContactUpserter interface {
	UpsertDiscoveryCandidate(ctx context.Context, req repository.UpsertDiscoveryCandidateRequest) (*repository.ExternalContact, error)
	GetBySource(ctx context.Context, source, sourceID string, accountID *string) (*repository.ExternalContact, error)
	UpdateMatch(ctx context.Context, id uuid.UUID, crmContactID *uuid.UUID, status repository.MatchStatus) (*repository.ExternalContact, error)
}

// contactEnricher performs narrow method-only enrichment on a bound CRM
// contact. Satisfied by *service.EnrichmentService. May be nil in tests.
type contactEnricher interface {
	SyncMethodsFromExternal(ctx context.Context, crmContactID uuid.UUID, external *repository.ExternalContact) error
}

// PeerMatcher binds WhatsApp peers to CRM contacts and surfaces the frequent
// unknown ones as import candidates.
//
// There is no two-tier ladder, unlike Telegram: identity.IdentifierTypeWhatsApp
// already searches BOTH whatsapp and phone contact methods, so one
// MatchOrCreate call covers what Telegram needs two for.
type PeerMatcher struct {
	identityService     identityMatcher
	commsRepo           commsPeerStore
	externalContactRepo externalContactUpserter
	enricher            contactEnricher   // may be nil (tests)
	engine              contactAggregator // may be nil (tests)
	discoveryMinMsgs    int
}

// NewPeerMatcher creates a new peer matcher.
func NewPeerMatcher(
	identityService identityMatcher,
	commsRepo commsPeerStore,
	externalContactRepo externalContactUpserter,
	enricher contactEnricher,
	engine contactAggregator,
	discoveryMinMsgs int,
) *PeerMatcher {
	return &PeerMatcher{
		identityService:     identityService,
		commsRepo:           commsRepo,
		externalContactRepo: externalContactRepo,
		enricher:            enricher,
		engine:              engine,
		discoveryMinMsgs:    discoveryMinMsgs,
	}
}

// MatchPeer resolves a peer to a CRM contact. Returns (nil, nil) when the peer
// is unknown — which is a normal outcome, not a failure: the message still
// stages, unmatched, and the peer can reach a contact through the import queue.
//
// Matching is best-effort throughout: an identity error logs and yields nil
// rather than failing the message, mirroring Telegram's discipline.
func (m *PeerMatcher) MatchPeer(ctx context.Context, peerJID string, phoneE164, pushName *string) (*uuid.UUID, error) {
	// A peer whose phone number was never recovered has nothing to match on:
	// there is no identifier type for a WhatsApp-internal id, so there is
	// nothing worth minting.
	if phoneE164 == nil || *phoneE164 == "" {
		return nil, nil
	}

	result, err := m.identityService.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier: *phoneE164,
		Type:          identity.IdentifierTypeWhatsApp,
		Source:        syncSource,
		SourceID:      &peerJID,
		DisplayName:   nilIfEmptyPtr(pushName),
	})
	if err != nil {
		logger.Warn().Err(err).Msg("whatsapp: peer identity match failed")
		return nil, nil
	}
	if result == nil || result.ContactID == nil {
		return nil, nil
	}

	// Reconciliation is FIRST-MATCH work: marking the import candidate matched
	// and syncing the contact methods the peer implies each have to happen once,
	// not on every message this peer ever sends.
	//
	// The gate is Cached, which the identity service sets when the identity row
	// ALREADY existed and ALREADY pointed at a live contact — precisely "this
	// peer was reconciled by an earlier message". Gating on the candidate row
	// instead would not work: a peer whose phone matched a contact on its very
	// FIRST message never gets a candidate row at all (discovery runs only on
	// the unmatched branch), so the read would return nothing every time and the
	// method sync would run forever, inline on the library's serialized handler
	// goroutine. That is the dominant matched population, not an edge case.
	//
	// The cost of gating here rather than after the read: if the mark below
	// fails transiently, no later message retries it, so the candidate can stay
	// labelled unmatched while its contact is bound. That is cosmetic — the
	// contact link itself is on the identity row — and it is the price of not
	// paying a read per message forever.
	if !result.Cached {
		m.reconcileOnMatch(ctx, *result.ContactID, peerJID, phoneE164)
	}
	logger.Info().
		Str("contact_id", result.ContactID.String()).
		Str("match_type", string(result.MatchType)).
		Msg("whatsapp: peer matched")
	return result.ContactID, nil
}

// OnPeerLinked back-fills after an import or a manual link: it binds the
// identity and re-points every staged message for the peer onto the contact.
//
// phoneE164 is optional: a peer staged under a WhatsApp-internal id before its
// phone number resolved is attachable only by the number, and the attach query
// matches peer_handle OR peer_normalized. Passing nil narrows it to the handle.
func (m *PeerMatcher) OnPeerLinked(ctx context.Context, peerJID string, phoneE164 *string, contactID uuid.UUID) error {
	if phoneE164 != nil && *phoneE164 != "" {
		if _, err := m.identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier:  *phoneE164,
			Type:           identity.IdentifierTypeWhatsApp,
			Source:         syncSource,
			SourceID:       &peerJID,
			KnownContactID: &contactID,
		}); err != nil {
			// Non-fatal: the identity link is a convenience for future
			// matches, while the attach below is the user-visible effect.
			logger.Warn().Err(err).Msg("whatsapp: failed to link identity on import")
		}
	}

	attached, deduped, err := m.commsRepo.AttachUnmatchedByPeer(ctx, syncSource, nilIfEmptyPtr(phoneE164), &peerJID, contactID)
	if err != nil {
		return fmt.Errorf("attach whatsapp staged messages: %w", err)
	}

	logger.Info().
		Str("contact_id", contactID.String()).
		Int64("attached", attached).
		Int64("deduped", deduped).
		Msg("whatsapp: peer linked, staged messages attached")

	// Deriving the interactions HERE, rather than leaving them to the periodic
	// sweeper, is what makes the attach above visible: the user's evidence that
	// a link worked is the contact's activity, not a row count nobody sees. A
	// candidate carrying dozens of staged messages that binds instantly but
	// still shows zero interactions for up to five minutes (the sweeper's
	// interval) is indistinguishable, at the moment the user acts, from a link
	// that silently failed. Skipped when nothing was attached — an
	// already-linked peer's repeat call has nothing new to aggregate — and
	// guarded on a nil engine for the many test call sites that pass nil.
	//
	// A failure here is logged and swallowed rather than returned: the rows ARE
	// attached, so the link itself succeeded, and returning the error would
	// report a successful link as failed. The periodic sweeper is the retry for
	// whatever aggregation could not finish.
	//
	// This is the OPPOSITE of what the contact-method rematch path does with the
	// same error, and deliberately so: that path runs inside a River job, where
	// returning the error is what schedules the retry and makes the failure
	// visible as a failed job. A link is a synchronous user action with no job
	// behind it, so there is nothing for a returned error to retry — only a
	// success the user would be told had failed.
	if attached > 0 && m.engine != nil {
		if err := m.engine.AggregateForContactBatch(ctx, contactID); err != nil {
			logger.Warn().Err(err).
				Str("contact_id", contactID.String()).
				Msg("whatsapp: post-link aggregation failed; the periodic sweeper will retry")
		}
	}
	return nil
}

// UpdateDiscoveryCandidates upserts an import candidate for every unmatched
// peer at or above the discovery threshold. peerJID nil means every peer.
//
// The candidate ALWAYS carries a display name — push name, else resolved phone,
// else a "WhatsApp <user>" label — because the imports queue's unresolved-hiding
// predicates are Telegram-scoped, so a WhatsApp candidate is always visible and
// must therefore never be contentless. The ladder is monotone under the
// upsert's COALESCE preserve: a later push name always upgrades an earlier
// phone-or-JID label, because a non-nil incoming value wins.
func (m *PeerMatcher) UpdateDiscoveryCandidates(ctx context.Context, peerJID *string) error {
	counts, err := m.commsRepo.ListUnmatchedPeerCounts(ctx, syncSource, peerJID, m.discoveryMinMsgs)
	if err != nil {
		return fmt.Errorf("list unmatched whatsapp peer counts: %w", err)
	}

	now := accelerated.GetCurrentTime()
	created := 0
	for _, row := range counts {
		if row.PeerHandle == "" {
			continue
		}

		metadata := map[string]any{
			"message_count":  row.TotalCount,
			"outbound_count": row.OutboundCount,
			"inbound_count":  row.InboundCount,
			"peer_jid":       row.PeerHandle,
		}
		if !row.LastMessageAt.IsZero() {
			metadata["last_message_at"] = row.LastMessageAt.Format("2006-01-02T15:04:05Z")
		}
		if pushName := nilIfEmptyPtr(row.LastPushName); pushName != nil {
			metadata["push_name"] = *pushName
		}
		if phone := nilIfEmptyPtr(row.PeerNormalized); phone != nil {
			metadata["phone_e164"] = *phone
		}

		label := discoveryDisplayName(row)
		if _, err := m.externalContactRepo.UpsertDiscoveryCandidate(ctx, repository.UpsertDiscoveryCandidateRequest{
			Source:      syncSource,
			SourceID:    row.PeerHandle,
			DisplayName: &label,
			Metadata:    metadata,
			SyncedAt:    &now,
		}); err != nil {
			logger.Warn().Err(err).Msg("whatsapp: failed to upsert discovery candidate")
			continue
		}
		created++
	}

	if created > 0 {
		logger.Info().Int("candidates", created).Msg("whatsapp: discovery candidates updated")
	}
	return nil
}

// discoveryDisplayName is the never-nil label ladder. FirstName/LastName stay
// unset throughout: WhatsApp gives a push name, not a split name.
func discoveryDisplayName(row repository.UnmatchedPeerCount) string {
	if pushName := nilIfEmptyPtr(row.LastPushName); pushName != nil {
		return *pushName
	}
	if phone := nilIfEmptyPtr(row.PeerNormalized); phone != nil {
		return *phone
	}
	user := row.PeerHandle
	if at := strings.Index(user, "@"); at >= 0 {
		user = user[:at]
	}
	return "WhatsApp " + user
}

// reconcileOnMatch marks the peer's import candidate matched and syncs the
// contact methods it implies. Errors are logged at warn and never fail the
// match.
//
// The CALLER gates this on the first match for a peer, so a known peer's steady
// state costs nothing here at all — no read, no write, no method sync. When it
// does run it does ONE candidate read and reuses it for both steps; reading
// twice, once to mark and once to enrich, is what this shape replaced.
//
// The already-matched/imported short-circuit below is a second, narrower guard
// for the case the caller's gate cannot see: an identity that was bound by some
// other flow (an import, a rematch) and is reaching the live path uncached for
// the first time.
func (m *PeerMatcher) reconcileOnMatch(ctx context.Context, contactID uuid.UUID, peerJID string, phoneE164 *string) {
	external, err := m.externalContactRepo.GetBySource(ctx, syncSource, peerJID, nil)
	if err != nil {
		// Transient repository error — bail out rather than synthesize, which
		// would risk dropping persisted values and emitting a NULL
		// external_contact_id audit row when a real one exists.
		logger.Warn().Err(err).Msg("whatsapp: get external_contact for match reconcile failed")
		return
	}

	if external != nil {
		if external.MatchStatus == repository.MatchStatusMatched || external.MatchStatus == repository.MatchStatusImported {
			// Already reconciled by an earlier message from this peer.
			return
		}
		if _, err := m.externalContactRepo.UpdateMatch(ctx, external.ID, &contactID, repository.MatchStatusMatched); err != nil {
			logger.Warn().Err(err).Msg("whatsapp: failed to mark external contact as matched")
		}
	}

	if m.enricher == nil {
		return
	}
	if external == nil {
		// No candidate row: the peer matched before ever crossing the discovery
		// threshold. Synthesize the minimum the method builder reads.
		external = &repository.ExternalContact{
			Source:   syncSource,
			SourceID: peerJID,
			Metadata: map[string]any{},
		}
	} else if external.Metadata == nil {
		external.Metadata = map[string]any{}
	}
	if phoneE164 != nil && *phoneE164 != "" {
		// The current message is the freshest source of the peer's number.
		external.Metadata["phone_e164"] = *phoneE164
	}

	if err := m.enricher.SyncMethodsFromExternal(ctx, contactID, external); err != nil {
		logger.Warn().Err(err).
			Str("contact_id", contactID.String()).
			Msg("whatsapp: sync methods from external failed")
	}
}
