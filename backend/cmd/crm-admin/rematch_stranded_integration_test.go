//go:build integration_testdb

// End-to-end coverage for crm-admin --messages-rematch-stranded, driving the
// REAL run() dispatch through the production wiring (buildProductionDeps)
// against an isolated DB clone. It proves which stored messages the rematch
// may attribute to a contact and what its printed summary counts.
package main

import (
	"bytes"
	"personal-crm/backend/tests/testsupport"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// spec: ING-041.outbound-group-message-unattributed
// spec: ING-042.no-cadence-or-event-effect
func TestRematchStranded_Integration_NeverAttributesAGroupOutboundOrAnAddedMessageAndStillMatchesIngestedOnes(t *testing.T) {
	t.Parallel()
	testsupport.RequireDatabase(t)
	ctx := t.Context()
	database, cfg := newJobsTestDB(t, ctx)
	deps, cleanup, err := buildProductionDeps(ctx, cfg, database, runOptions{rematchStranded: true})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	out := &bytes.Buffer{}
	deps.stdout, deps.stderr = out, &bytes.Buffer{}

	messages := repository.NewMessagesMessageRepository(database.Queries)
	contacts := repository.NewContactRepository(database.Queries)
	contacts.SetPool(database.Pool)
	methods := repository.NewContactMethodRepository(database.Queries)
	interactions := repository.NewInteractionRepository(database.Queries)
	hostService := service.NewMacHostService(repository.NewMacHostRepository(database.Queries),
		repository.NewMacHostPairingTokenRepository(database.Queries),
		repository.NewSyncRepositoryWithPool(database.Queries, database.Pool), nil, nil, nil, database.Pool, 4)
	token, _, err := hostService.CreatePairingToken(ctx)
	require.NoError(t, err)
	paired, err := hostService.PairWithToken(ctx, token, "rematch-integration", "0.1.0", 1)
	require.NoError(t, err)

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const chat = "rematch-group-chat"
	store := func(guid, peer string, outgoing bool, at time.Time) *repository.MessagesMessage {
		row, err := messages.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
			Guid: guid, ChatGuid: chat, PeerHandle: peer, MessageType: "text", SentAt: at,
			IsOutgoing: outgoing, IsGroupChat: true, MacHostID: &paired.HostID,
		})
		require.NoError(t, err)
		return row
	}
	newContact := func(name, phone string) *repository.Contact {
		c, err := contacts.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
		require.NoError(t, err)
		_, err = methods.CreateContactMethod(ctx, repository.CreateContactMethodRequest{ContactID: c.ID, Type: "phone", Value: phone})
		require.NoError(t, err)
		return c
	}

	// A burst the enrichment can add a message to.
	anchor := newContact("Rematch anchor", "+15553000000")
	anchorRow := store("rematch-anchor", "+15553000000", false, base)
	burst, err := interactions.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID: anchor.ID, Source: repository.InteractionSourceMessages, SourceRef: strPtr("messages:" + chat + ":rematch-anchor"),
		OccurredAt: base.Add(time.Hour), Direction: repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)
	other := store("rematch-anchor-2", "+15553000000", false, base.Add(time.Hour))
	require.NoError(t, messages.MarkMessagesProcessed(ctx, []uuid.UUID{anchorRow.ID, other.ID}, burst.ID))

	// The three stranded candidates, stored before their senders are contacts.
	unattributed := store("rematch-unattributed-outbound", "", true, base.Add(10*time.Minute))
	added := newGuid("rematch-added")
	res, err := service.NewMessagesEnrichmentService(messages).ApplyItems(ctx, paired.HostID, []service.EnrichmentItem{{
		Op: service.EnrichmentOpAddGroupMessage, Guid: added, ChatGuid: chat, SentAt: base.Add(20 * time.Minute),
		Sender: strPtr("+15553000001"), Body: strPtr("added later"),
	}})
	require.NoError(t, err)
	require.Equal(t, service.EnrichmentAdded, res.Results[0].Outcome)
	ingested := store("rematch-ingested-non-contact", "+15553000002", false, base.Add(30*time.Minute))
	addedContact := newContact("Rematch added sender", "+15553000001")
	ingestedContact := newContact("Rematch ingested sender", "+15553000002")

	require.NoError(t, run(ctx, runOptions{rematchStranded: true}, deps))

	summary := out.String()
	require.Contains(t, summary, "  matched:        1\n", "only the ingested message is matched: %s", summary)
	require.Contains(t, summary, "  enqueued:       1\n")
	require.Contains(t, summary, "  errors:         0\n")
	row := func(guid string) *repository.MessagesMessage {
		r, err := messages.GetMessage(ctx, guid)
		require.NoError(t, err)
		return r
	}
	require.Nil(t, row(unattributed.Guid).MatchedContactID, "an unattributed outbound group row stays unmatched")
	require.Nil(t, row(added).MatchedContactID, "a message the enrichment added stays unmatched")
	require.Nil(t, row(added).InteractionID)
	matched := row(ingested.Guid)
	require.NotNil(t, matched.MatchedContactID)
	require.Equal(t, ingestedContact.ID, *matched.MatchedContactID)
	require.NotEqual(t, addedContact.ID, ingestedContact.ID)
}

func strPtr(s string) *string { return &s }

func newGuid(prefix string) string { return prefix + "-" + uuid.NewString() }
