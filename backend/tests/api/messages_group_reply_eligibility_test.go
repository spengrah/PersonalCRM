package api

import (
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/require"
)

// spec: ING-042.no-cadence-or-event-effect
// spec: ING-043.promotion-order-independent
func TestGroupReplyEligibility_AnAddedUsersMessageLaterIngestedUnderTheSameGuidPromotesNoBurst(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	member := g.newMember(t)
	burst, _ := g.burst(t, chat, member, at(0), at(120))

	outbound := g.sent(chat, at(60))
	require.Equal(t, []string{"added"}, g.enrich(t, addItem(outbound.guid, chat, nil, "outbound", at(60), strPtr("late copy"))).outcomes())
	g.postAccepted(t, outbound)
	g.settle(t)

	after := g.interaction(t, burst.ID)
	require.Equal(t, repository.InteractionDirectionInbound, after.Direction, "an added row never credits a reply, whichever path stores the guid again")
	require.True(t, at(120).Equal(after.OccurredAt))
}

// spec: ING-042.no-cadence-or-event-effect
// spec: ING-042.keeps-identity-membership-and-state
func TestGroupReplyEligibility_AnUnprocessedOutboundRowClearedOfItsAttributionPromotesNoBurst(t *testing.T) {
	g := setupGroupIngest(t)
	chat := chatName(t)
	sender, member := g.newMember(t), g.newMember(t)
	burst, _ := g.burst(t, chat, member, at(0), at(120))
	legacy, err := g.messagesRepo.UpsertMessage(t.Context(), repository.UpsertMessagesMessageParams{
		Guid: g.newGuid(), ChatGuid: chat, PeerHandle: g.phones[sender.ID], PeerNormalized: strPtr(g.phones[sender.ID]),
		Text: strPtr("legacy outbound"), MessageType: "text", SentAt: at(60), IsOutgoing: true, IsGroupChat: true,
		MatchedContactID: &sender.ID, MacHostID: &g.pair.HostID,
	})
	require.NoError(t, err)
	require.Nil(t, legacy.ProcessedAt, "the row is still unprocessed")

	require.Equal(t, []string{"cleared"}, g.enrich(t, clearItem(legacy.Guid)).outcomes())
	g.postAccepted(t, g.received(chat, g.phones[member.ID], at(180)))
	g.settle(t)

	require.Equal(t, repository.InteractionDirectionInbound, g.interaction(t, burst.ID).Direction, "a cleared row is no reply, processed or not")
	cleared := g.row(t, legacy.Guid)
	require.Nil(t, cleared.ProcessedAt, "clearing keeps the processing state")
}
