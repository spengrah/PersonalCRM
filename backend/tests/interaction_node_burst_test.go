package tests

import (
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/require"
)

// A message burst is not a correctable interaction (IXN-015): the session an
// aggregation engine records, extends or promotes to mutual resolves to no
// interaction node. Each test runs one source through the shared aggregation
// engine: a created session, the session extended by a later message in the
// same direction, and the session promoted to mutual by a reply.

func requireNoNode(t *testing.T, rows []repository.Interaction, step string) {
	t.Helper()
	require.Len(t, rows, 1, step)
	require.Nil(t, rows[0].InteractionNodeID, "%s: a message burst resolves to no interaction node", step)
}

// spec: IXN-015.bursts-have-no-node
func TestInteractionNode_TelegramBurstSessionHasNoNode(t *testing.T) {
	t.Parallel()
	messageRepo, interactionRepo, _, _, engine, database := setupAggregationTest(t)
	ctx := t.Context()
	gen, _ := migrationGenerator(t)
	contact, cleanup := seedMigrationContact(ctx, t, database, gen)
	t.Cleanup(cleanup)

	base := accelerated.GetCurrentTime().Add(-3 * time.Hour).Truncate(time.Microsecond)
	const chatID, peerID = int64(9101), int64(79101)

	insertTestMessage(t, messageRepo, 89101, chatID, true, base, &contact.ID, peerID)
	require.NoError(t, engine.AggregateAll(ctx)) // the batch path
	requireNoNode(t, waitForInteractionCountExact(t, ctx, interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout), "created")

	insertTestMessage(t, messageRepo, 89102, chatID, true, base.Add(10*time.Minute), &contact.ID, peerID)
	require.NoError(t, engine.AggregateForContact(ctx, contact.ID, chatID))
	requireNoNode(t, waitForInteractionCountExact(t, ctx, interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout), "extended")

	insertTestMessage(t, messageRepo, 89103, chatID, false, base.Add(30*time.Minute), &contact.ID, peerID)
	require.NoError(t, engine.AggregateForContact(ctx, contact.ID, chatID))
	rows := waitForInteractionDirection(t, ctx, interactionRepo, contact.ID, repository.InteractionDirectionMutual, defaultInteractionWaitTimeout)
	requireNoNode(t, rows, "promoted to mutual")
}

// spec: IXN-015.bursts-have-no-node
func TestInteractionNode_WhatsAppBurstSessionHasNoNode(t *testing.T) {
	t.Parallel()
	e := setupWhatsAppEngineTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()
	contact := e.newWhatsAppContact(t, "WhatsApp Node "+suffix)
	chatJID := "1204555" + suffix + "@s.whatsapp.net"
	base := accelerated.GetCurrentTime().Add(-3 * time.Hour).Truncate(time.Microsecond)

	e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-node1-"+suffix, repository.InteractionDirectionOutbound, base)
	require.NoError(t, e.engine.AggregateAll(e.ctx)) // the batch path
	requireNoNode(t, waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout), "created")

	e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-node2-"+suffix, repository.InteractionDirectionOutbound, base.Add(10*time.Minute))
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, chatJID))
	requireNoNode(t, waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout), "extended")

	e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-node3-"+suffix, repository.InteractionDirectionInbound, base.Add(30*time.Minute))
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, chatJID))
	rows := waitForInteractionDirection(t, e.ctx, e.interactionRepo, contact.ID, repository.InteractionDirectionMutual, defaultInteractionWaitTimeout)
	requireNoNode(t, rows, "promoted to mutual")
}

// spec: IXN-015.bursts-have-no-node
func TestInteractionNode_GChatBurstSessionHasNoNode(t *testing.T) {
	t.Parallel()
	e := setupGChatEngineTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()
	contact := e.newGChatContact(t, "GChat Node "+suffix)
	space := "spaces/NODE-" + suffix
	base := accelerated.GetCurrentTime().Add(-3 * time.Hour).Truncate(time.Microsecond)

	e.seedGChatRow(t, contact.ID, space, "gchat-node1-"+suffix, repository.InteractionDirectionOutbound, base)
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, space))
	requireNoNode(t, waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout), "created")

	e.seedGChatRow(t, contact.ID, space, "gchat-node2-"+suffix, repository.InteractionDirectionOutbound, base.Add(10*time.Minute))
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, space))
	requireNoNode(t, waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout), "extended")

	e.seedGChatRow(t, contact.ID, space, "gchat-node3-"+suffix, repository.InteractionDirectionInbound, base.Add(30*time.Minute))
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, space))
	rows := waitForInteractionDirection(t, e.ctx, e.interactionRepo, contact.ID, repository.InteractionDirectionMutual, defaultInteractionWaitTimeout)
	requireNoNode(t, rows, "promoted to mutual")
}
