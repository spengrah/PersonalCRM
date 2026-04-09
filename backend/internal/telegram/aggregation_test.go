package telegram

import (
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeMsg(id int32, chatID int64, outgoing bool, sentAt time.Time) repository.TelegramMessage {
	return repository.TelegramMessage{
		ID:                uuid.New(),
		TelegramMessageID: id,
		TelegramChatID:    chatID,
		IsOutgoing:        outgoing,
		SentAt:            sentAt,
	}
}

func TestGroupIntoBursts_SingleOutbound(t *testing.T) {
	e := &AggregationEngine{burstWindowHours: 2}
	base := accelerated.GetCurrentTime()
	msgs := []repository.TelegramMessage{
		makeMsg(1, 100, true, base),
		makeMsg(2, 100, true, base.Add(10*time.Minute)),
		makeMsg(3, 100, true, base.Add(30*time.Minute)),
	}

	bursts := e.groupIntoBursts(msgs, 100)
	require.Len(t, bursts, 1)
	assert.Equal(t, repository.InteractionDirectionOutbound, bursts[0].direction)
	assert.Len(t, bursts[0].messages, 3)
}

func TestGroupIntoBursts_SingleInbound(t *testing.T) {
	e := &AggregationEngine{burstWindowHours: 2}
	base := accelerated.GetCurrentTime()
	msgs := []repository.TelegramMessage{
		makeMsg(1, 100, false, base),
		makeMsg(2, 100, false, base.Add(5*time.Minute)),
	}

	bursts := e.groupIntoBursts(msgs, 100)
	require.Len(t, bursts, 1)
	assert.Equal(t, repository.InteractionDirectionInbound, bursts[0].direction)
}

func TestGroupIntoBursts_SplitByGap(t *testing.T) {
	e := &AggregationEngine{burstWindowHours: 2}
	base := accelerated.GetCurrentTime()
	msgs := []repository.TelegramMessage{
		makeMsg(1, 100, true, base),
		makeMsg(2, 100, true, base.Add(3*time.Hour)), // >2h gap
	}

	bursts := e.groupIntoBursts(msgs, 100)
	require.Len(t, bursts, 2)
}

func TestGroupIntoBursts_DirectionChangeSplits(t *testing.T) {
	e := &AggregationEngine{burstWindowHours: 2}
	base := accelerated.GetCurrentTime()
	msgs := []repository.TelegramMessage{
		makeMsg(1, 100, true, base),
		makeMsg(2, 100, false, base.Add(5*time.Minute)), // direction change
	}

	bursts := e.groupIntoBursts(msgs, 100)
	require.Len(t, bursts, 2)
	assert.Equal(t, repository.InteractionDirectionOutbound, bursts[0].direction)
	assert.Equal(t, repository.InteractionDirectionInbound, bursts[1].direction)
}

func TestResolveSessions_ReplyBridgeWithin48h(t *testing.T) {
	e := &AggregationEngine{burstWindowHours: 2, replyBridgeHours: 48}
	base := accelerated.GetCurrentTime()

	bursts := []burst{
		{
			direction: repository.InteractionDirectionOutbound,
			messages: []repository.TelegramMessage{
				makeMsg(1, 100, true, base),
				makeMsg(2, 100, true, base.Add(5*time.Minute)),
			},
			chatID: 100,
		},
		{
			direction: repository.InteractionDirectionInbound,
			messages: []repository.TelegramMessage{
				makeMsg(3, 100, false, base.Add(1*time.Hour)), // within 48h
			},
			chatID: 100,
		},
	}

	sessions := e.resolveSessions(bursts)
	require.Len(t, sessions, 1)
	assert.Equal(t, repository.InteractionDirectionMutual, sessions[0].direction)
	assert.Len(t, sessions[0].messages, 3)
}

func TestResolveSessions_ReplyBridgeExpired(t *testing.T) {
	e := &AggregationEngine{burstWindowHours: 2, replyBridgeHours: 48}
	base := accelerated.GetCurrentTime()

	bursts := []burst{
		{
			direction: repository.InteractionDirectionOutbound,
			messages: []repository.TelegramMessage{
				makeMsg(1, 100, true, base),
			},
			chatID: 100,
		},
		{
			direction: repository.InteractionDirectionInbound,
			messages: []repository.TelegramMessage{
				makeMsg(2, 100, false, base.Add(49*time.Hour)), // >48h gap
			},
			chatID: 100,
		},
	}

	sessions := e.resolveSessions(bursts)
	require.Len(t, sessions, 2) // not bridged
}

func TestResolveSessions_ExplicitReplyBridges(t *testing.T) {
	e := &AggregationEngine{burstWindowHours: 2, replyBridgeHours: 48}
	base := accelerated.GetCurrentTime()

	replyTo := int32(1)
	bursts := []burst{
		{
			direction: repository.InteractionDirectionOutbound,
			messages: []repository.TelegramMessage{
				makeMsg(1, 100, true, base),
			},
			chatID: 100,
		},
		{
			direction: repository.InteractionDirectionInbound,
			messages: []repository.TelegramMessage{
				{
					ID:                uuid.New(),
					TelegramMessageID: 2,
					TelegramChatID:    100,
					IsOutgoing:        false,
					SentAt:            base.Add(72 * time.Hour), // >48h gap
					ReplyToMsgID:      &replyTo,                 // explicit reply to msg 1
				},
			},
			chatID: 100,
		},
	}

	sessions := e.resolveSessions(bursts)
	require.Len(t, sessions, 1) // bridged via explicit reply
	assert.Equal(t, repository.InteractionDirectionMutual, sessions[0].direction)
}

func TestSessionKey_Stability(t *testing.T) {
	sess := msgSession{
		chatID:   12345,
		firstMsg: 50001,
	}
	assert.Equal(t, "tg:12345:50001", sess.sourceRef())
}

func TestResolveSessions_ChatScoped(t *testing.T) {
	e := &AggregationEngine{burstWindowHours: 2, replyBridgeHours: 48}
	base := accelerated.GetCurrentTime()

	// Messages from same contact in different chats should NOT merge
	bursts := []burst{
		{
			direction: repository.InteractionDirectionOutbound,
			messages:  []repository.TelegramMessage{makeMsg(1, 100, true, base)},
			chatID:    100,
		},
		{
			direction: repository.InteractionDirectionOutbound,
			messages:  []repository.TelegramMessage{makeMsg(2, 200, true, base.Add(5*time.Minute))},
			chatID:    200,
		},
	}

	sessions := e.resolveSessions(bursts)
	require.Len(t, sessions, 2) // separate chats = separate sessions
}

func TestResolveSessions_CrossChatNoBridge(t *testing.T) {
	e := &AggregationEngine{burstWindowHours: 2, replyBridgeHours: 48}
	base := accelerated.GetCurrentTime()

	// Outbound in chat 100, inbound in chat 200 — should NOT bridge even within 48h
	bursts := []burst{
		{
			direction: repository.InteractionDirectionOutbound,
			messages:  []repository.TelegramMessage{makeMsg(1, 100, true, base)},
			chatID:    100,
		},
		{
			direction: repository.InteractionDirectionInbound,
			messages:  []repository.TelegramMessage{makeMsg(2, 200, false, base.Add(1*time.Hour))},
			chatID:    200,
		},
	}

	sessions := e.resolveSessions(bursts)
	require.Len(t, sessions, 2) // different chats — no bridging
	assert.Equal(t, repository.InteractionDirectionOutbound, sessions[0].direction)
	assert.Equal(t, repository.InteractionDirectionInbound, sessions[1].direction)
}

func TestPartitionByChat(t *testing.T) {
	now := accelerated.GetCurrentTime()
	msgs := []repository.TelegramMessage{
		makeMsg(1, 100, true, now),
		makeMsg(2, 200, true, now),
		makeMsg(3, 100, false, now),
	}

	result := partitionByChat(msgs)
	assert.Len(t, result[100], 2)
	assert.Len(t, result[200], 1)
}

func TestMsgDirection(t *testing.T) {
	assert.Equal(t, repository.InteractionDirectionOutbound, msgDirection(repository.TelegramMessage{IsOutgoing: true}))
	assert.Equal(t, repository.InteractionDirectionInbound, msgDirection(repository.TelegramMessage{IsOutgoing: false}))
}
