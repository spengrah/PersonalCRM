package replay

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	gmailapi "google.golang.org/api/gmail/v1"
)

// GmailResult is the settled outcome of a Gmail replay.
type GmailResult struct {
	ContactID  uuid.UUID
	ExternalID string
	Matched    bool // false for MatchUnknown (match-only)
}

// ReplayGmail feeds a synthetic Gmail message through the REAL GmailSyncProvider
// (fake fetcher + injected me-set, no OAuth) and settles the graph. For
// MatchSeeded the seeded contact's email matches → comms_message + email.* event
// → email_interaction_consumer → interaction. For MatchUnknown it is match-only
// (no contact, no interaction, no row).
//
// contactID is the seeded contact this message targets (for MatchSeeded). The
// caller seeds the contact first via Harness.SeedContact.
func (h *Harness) ReplayGmail(ctx context.Context, contactID uuid.UUID, spec factory.GmailMessageSpec) (GmailResult, error) {
	provider := google.NewGmailSyncProvider(nil, h.commsRepo, h.bus, h.database.Pool)
	provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(google.FakeGmailFetcherFuncs{
		ListMessageIDs: func(_ context.Context, _, pageToken string) ([]google.GmailMessageRefForTest, string, error) {
			if pageToken != "" {
				return nil, "", nil
			}
			return []google.GmailMessageRefForTest{{ID: spec.Message.Id, ThreadID: spec.Message.ThreadId}}, "", nil
		},
		GetMessage: func(_ context.Context, id string) (*gmailapi.Message, error) {
			if id == spec.Message.Id {
				return spec.Message, nil
			}
			return nil, fmt.Errorf("synthetic gmail: message %s not found", id)
		},
	}))
	provider.SetMeSetForTest(map[string]struct{}{spec.AccountID: {}})

	// email.* root events are published by the provider directly; track the
	// source so cleanup captures them.
	h.track(func(c *created) { c.addDirectSource(repository.InteractionSourceEmail) })

	state := &repository.SyncState{
		Source:    repository.InteractionSourceEmail,
		AccountID: &spec.AccountID,
		Metadata:  map[string]any{"backfill_since": gmailBackfillSince(h)},
	}
	if _, err := provider.Sync(ctx, state, nil); err != nil {
		return GmailResult{}, fmt.Errorf("gmail sync: %w", err)
	}

	if spec.Intent == factory.MatchUnknown {
		// Match-only: no interaction to wait on; Gate B has no email jobs for a
		// seeded contact. Settle with an immediately-true predicate scoped to
		// contactIDs (empty here, so Gate B is trivially 0).
		if err := h.Settle(ctx, func(context.Context) (bool, error) { return true, nil }, ""); err != nil {
			return GmailResult{}, err
		}
		return GmailResult{ExternalID: spec.ExternalID, Matched: false}, nil
	}

	// Gate A: the interaction with this email source landed for the contact.
	predicate := func(ctx context.Context) (bool, error) {
		return h.contactHasInteractionSource(ctx, contactID, repository.InteractionSourceEmail)
	}
	if err := h.Settle(ctx, predicate, ""); err != nil {
		return GmailResult{}, err
	}
	h.trackContactInteractions(ctx, contactID)
	return GmailResult{ContactID: contactID, ExternalID: spec.ExternalID, Matched: true}, nil
}
