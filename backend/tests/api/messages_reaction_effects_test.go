package api

import (
	"testing"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// reactionScenario stores a target message in a chat of its own, then a
// reaction to it, and settles the pipeline after each.
type reactionScenario struct {
	name string
	// target and reaction build the two messages for the member in the chat.
	target, reaction func(g *groupIngestEnv, chat string, member *repository.Contact) rawMsg
	// burst is the member's other messages, posted in one batch with the
	// target so they aggregate together.
	burst func(g *groupIngestEnv, chat string, member *repository.Contact) []rawMsg
}

func oneToOne(kind events.Kind, sentAt time.Time) func(g *groupIngestEnv, chat string, member *repository.Contact) rawMsg {
	return func(g *groupIngestEnv, chat string, member *repository.Contact) rawMsg {
		return rawMsg{kind: kind, guid: g.newGuid(), chat: chat, handle: g.phones[member.ID], sentAt: sentAt, text: "one-to-one " + uuid.NewString()}
	}
}

func inGroup(kind events.Kind, sentAt time.Time) func(g *groupIngestEnv, chat string, member *repository.Contact) rawMsg {
	return func(g *groupIngestEnv, chat string, member *repository.Contact) rawMsg {
		if kind == events.KindRawMessageSent {
			return g.sent(chat, sentAt)
		}
		return g.received(chat, g.phones[member.ID], sentAt)
	}
}

// storedEffect is what a stored reaction row says about its downstream
// effect, free of the identifiers two runs of one scenario cannot share.
type storedEffect struct {
	Text, MessageType                                        string
	IsOutgoing, IsGroupChat, CreditsReply                    bool
	RepliesToTarget, Matched, LinkedToInteraction, Processed bool
}

type interactionEffect struct {
	Direction      string
	OccurredAt     time.Time
	LinkedMessages int
}

type reactionEffects struct {
	Row          storedEffect
	Interactions []interactionEffect
	Cadence      cadenceFields
	Tasks        int
}

// run plays the scenario for a fresh member in a chat of its own, with the
// reaction's facts or without them, and records every effect the reaction has.
func (sc reactionScenario) run(t *testing.T, g *groupIngestEnv, facts *events.MessageReaction) reactionEffects {
	t.Helper()
	member := g.newMember(t)
	chat := chatName(t)
	target := sc.target(g, chat, member)
	batch := []rawMsg{target}
	if sc.burst != nil {
		batch = append(batch, sc.burst(g, chat, member)...)
	}
	g.postAccepted(t, batch...)
	g.settle(t)

	reaction := sc.reaction(g, chat, member)
	reaction.text = `Loved "see you at noon"`
	reaction.replyTo = &target.guid
	if facts != nil {
		f := *facts
		f.TargetKey = target.guid
		reaction.reaction = &f
	}
	g.postAccepted(t, reaction)
	g.settle(t)

	row := g.row(t, reaction.guid)
	if facts != nil {
		require.NotNil(t, row.Reaction, "the facts are stored")
	} else {
		require.Nil(t, row.Reaction)
	}
	out := reactionEffects{
		Row: storedEffect{
			Text: *row.Text, MessageType: row.MessageType,
			IsOutgoing: row.IsOutgoing, IsGroupChat: row.IsGroupChat, CreditsReply: row.CreditsReply,
			RepliesToTarget:     row.ReplyToGuid != nil && *row.ReplyToGuid == target.guid,
			Matched:             row.MatchedContactID != nil,
			LinkedToInteraction: row.InteractionID != nil,
			Processed:           row.ProcessedAt != nil,
		},
		Cadence: cadenceOf(g.contact(t, member.ID)),
	}
	for _, i := range g.interactions(t, member.ID) {
		out.Interactions = append(out.Interactions, interactionEffect{
			Direction: i.Direction, OccurredAt: i.OccurredAt.UTC(), LinkedMessages: len(g.linkedRowsOf(t, i.ID)),
		})
	}
	tasks, err := repository.NewContactTaskRepository(g.database.Queries).ListContactTasksByContact(t.Context(), member.ID)
	require.NoError(t, err)
	out.Tasks = len(tasks)
	return out
}

// spec: ING-048.reaction-effects-unchanged
func TestReactionFacts_LeaveTheReactionsInteractionReplyAndCadenceEffectsAsTheyWere(t *testing.T) {
	t.Parallel()
	g := setupGroupIngest(t)
	removal := false
	love := &events.MessageReaction{Kind: "love", Removal: &removal}

	scenarios := []reactionScenario{
		{
			name:     "inbound one-to-one",
			target:   oneToOne(events.KindRawMessageSent, at(0)),
			reaction: oneToOne(events.KindRawMessageReceived, at(60)),
		},
		{
			name:     "outbound one-to-one",
			target:   oneToOne(events.KindRawMessageReceived, at(0)),
			reaction: oneToOne(events.KindRawMessageSent, at(60)),
		},
		{
			name:     "inbound in a group",
			target:   inGroup(events.KindRawMessageReceived, at(0)),
			reaction: inGroup(events.KindRawMessageReceived, at(60)),
		},
		{
			name:   "outbound in a group",
			target: inGroup(events.KindRawMessageReceived, at(0)),
			burst: func(g *groupIngestEnv, chat string, member *repository.Contact) []rawMsg {
				return []rawMsg{g.received(chat, g.phones[member.ID], at(120))}
			},
			reaction: inGroup(events.KindRawMessageSent, at(60)),
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			without := sc.run(t, g, nil)
			with := sc.run(t, g, love)

			require.NotEmpty(t, without.Interactions, "the reaction's scenario records an interaction")
			require.Equal(t, without, with, "the facts change no effect of the reaction")
		})
	}
}
