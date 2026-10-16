package declare

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	chat "google.golang.org/api/chat/v1"
)

// GroupMessage is one message said in a GroupChat, minutes from the chat's
// day: a member's, or the user's own when from is empty.
type GroupMessage struct {
	from    string
	minutes int
}

// Said is a member contact's message, minutes from the chat's day.
func Said(member string, minutes int) GroupMessage {
	return GroupMessage{from: member, minutes: minutes}
}

// ISaid is the user's own message, minutes from the chat's day. Google Chat
// fans it out to every member contact who spoke before it, so the store holds
// it once per such member.
func ISaid(minutes int) GroupMessage {
	return GroupMessage{minutes: minutes}
}

type groupChatPlan struct {
	name     string
	members  []string
	messages []GroupMessage
	props    interactionPlanProps
}

// GroupChat declares one Google Chat space the user and the member contacts
// belong to (a one-to-one chat when there is one member), and the messages
// said in it, oldest first. Each member's message is recorded against that
// member; the user's is recorded against every member who spoke before it.
// The manifest names the interaction holding the chat's latest message for the
// first member who holds one.
func GroupChat(handle string, members []string, at InteractionProp, messages ...GroupMessage) Entity {
	p := &groupChatPlan{name: handle, members: members, messages: messages}
	at(&p.props)
	return p
}

func (p *groupChatPlan) handle() string { return p.name }
func (p *groupChatPlan) kind() string   { return "interaction" }
func (p *groupChatPlan) refs() []string { return p.members }
func (p *groupChatPlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("group chat handle must be non-empty")
	}
	if len(p.members) == 0 {
		return fmt.Errorf("group chat %q: no member contacts", p.name)
	}
	if err := validateInteractionProps(p.name, &p.props, maxSyncedInteractionAgoDays); err != nil {
		return err
	}
	if len(p.messages) == 0 {
		return fmt.Errorf("group chat %q: no messages", p.name)
	}
	for i, m := range p.messages {
		if m.from != "" && !slices.Contains(p.members, m.from) {
			return fmt.Errorf("group chat %q: message %d is from %q, not a member", p.name, i, m.from)
		}
		if i > 0 && m.minutes <= p.messages[i-1].minutes {
			return fmt.Errorf("group chat %q: messages must be said oldest first", p.name)
		}
	}
	return nil
}

func runGroupChat(ctx context.Context, h *replay.Harness, p *groupChatPlan, st *runState) (Seeded, error) {
	gen := h.Generator()
	target := interactionTarget(gen.Anchor(), *p.props.agoDays)
	first, ok := st.specs[p.members[0]]
	if !ok {
		return Seeded{}, fmt.Errorf("group chat %q: no contact spec for %q", p.name, p.members[0])
	}
	base := gen.GChatMessage(first, factory.MatchSeeded, factory.WithMessageAge(interactionMessageAge(gen.Anchor(), target, "gchat")))
	// The factory's space holds the first member and the user; every other
	// member joins under a user name of its own.
	firstUser := base.Message.Sender.Name
	users := map[string]string{p.members[0]: firstUser}
	var meUser string
	for _, m := range base.Members {
		if m.Member.Name != firstUser {
			meUser = m.Member.Name
		}
	}
	members := []*chat.Membership{{State: "JOINED", Member: &chat.User{Name: firstUser, Type: "HUMAN"}}}
	for i, handle := range p.members[1:] {
		spec, ok := st.specs[handle]
		if !ok {
			return Seeded{}, fmt.Errorf("group chat %q: no contact spec for %q", p.name, handle)
		}
		user := fmt.Sprintf("%s-member-%d", firstUser, i+2)
		users[handle] = user
		base.EmailByUser[user] = spec.Email
		members = append(members, &chat.Membership{State: "JOINED", Member: &chat.User{Name: user, Type: "HUMAN"}})
	}
	base.Members = append(members, &chat.Membership{State: "JOINED", Member: &chat.User{Name: meUser, Type: "HUMAN"}})

	items := make([]replay.GChatBatchItem, len(p.messages))
	for i, m := range p.messages {
		sender, owner := meUser, p.members[0]
		if m.from != "" {
			sender, owner = users[m.from], m.from
		}
		contactID, err := st.contactID(owner)
		if err != nil {
			return Seeded{}, err
		}
		text := fmt.Sprintf("%s%s message %d", gen.Prefix(), p.name, i+1)
		at := target.Add(time.Duration(m.minutes) * time.Minute)
		items[i] = replay.GChatBatchItem{ContactID: contactID, Spec: cloneDeclaredGChatMessage(base, fmt.Sprintf("-g%d", i+1), sender, text, at)}
	}
	if _, err := h.ReplayGChatBatch(ctx, items); err != nil {
		return Seeded{}, fmt.Errorf("replay group chat %q: %w", p.name, err)
	}

	last := target.Add(time.Duration(p.messages[len(p.messages)-1].minutes) * time.Minute)
	earliest := target.Add(time.Duration(p.messages[0].minutes) * time.Minute)
	for _, handle := range p.members {
		contactID, err := st.contactID(handle)
		if err != nil {
			return Seeded{}, err
		}
		rows, err := h.InteractionRepo().ListContactInteractions(ctx, contactID, 100, 0)
		if err != nil {
			return Seeded{}, fmt.Errorf("read back group chat %q: %w", p.name, err)
		}
		var latest *repository.Interaction
		for i := range rows {
			r := &rows[i]
			if r.Source != repository.InteractionSourceGChat || r.OccurredAt.Before(earliest) || r.OccurredAt.After(last) {
				continue
			}
			if latest == nil || r.OccurredAt.After(latest.OccurredAt) {
				latest = r
			}
		}
		if latest != nil {
			return Seeded{Kind: "interaction", ID: latest.ID.String(), Name: p.name}, nil
		}
	}
	return Seeded{}, fmt.Errorf("group chat %q: no member holds an interaction of it", p.name)
}
