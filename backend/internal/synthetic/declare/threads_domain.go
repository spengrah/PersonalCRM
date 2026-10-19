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
)

// Threads-tab resolutions (IXN-074 to IXN-077): the heading, the Review switch
// and the Threads list. A browser test records the judgments of this world's
// threads through the judgment store's testing-only route, naming each
// thread's messages by the keys the thread read returns.
func init() {
	RegisterNone("IXN-074", "its citing tests open /interactions and its tab addresses and assert the page's own chrome, the heading, the Review switch and the two tabs, which render the same over any data")
	RegisterNone("IXN-075", "its citing tests seed the IXN-076 world and record judgments of its threads through the testing route, then turn the switch and read the list it leaves")
	Register(Declaration{Behavior: "IXN-076", Entities: threadsWorld()})
	RegisterNone("IXN-077", "its citing tests seed the IXN-076 world and record runs of the compared methods through the testing route, naming the messages the thread read returns, to judge its threads")
}

// manyThreads is the number of threads one contact takes part in, enough to
// fill the list's first page and take a second.
const manyThreads = 21

// threadsWorld is a Telegram group chat of four contacts who speak in turn over
// twelve minutes, an email thread, a meeting with a note, a chat of a contact
// who is in no other thread, manyThreads group chats of one more contact, and a
// Google Chat chat of a contact who is in no other thread.
func threadsWorld() []Entity {
	family := []string{"ana", "ben", "cai", "dee"}
	var talk []GroupMessage
	for i := range 12 {
		talk = append(talk, Said(family[i%len(family)], i))
	}
	entities := []Entity{
		Contact("ana", Methods(MethodEmail, MethodTelegram)),
		Contact("ben", Methods(MethodEmail, MethodTelegram)),
		Contact("cai", Methods(MethodEmail, MethodTelegram)),
		Contact("dee", Methods(MethodEmail, MethodTelegram)),
		Contact("solo", Methods(MethodEmail, MethodTelegram)),
		Contact("many", Methods(MethodEmail, MethodTelegram)),
		Contact("host", Methods(MethodEmail)),
		TelegramGroupChat("family", family, AgoDays(4), talk...),
		MessageInteraction("mail", "ana", "email", AgoDays(6)),
		CalendarEvent("meeting", "host", StartedDaysAgo(8)),
		LinkedMeetingNote("meeting-note", "meeting"),
		TelegramGroupChat("solo-chat", []string{"solo"}, AgoDays(2), Said("solo", 0)),
	}
	for i := range manyThreads {
		entities = append(entities,
			TelegramGroupChat(fmt.Sprintf("many-%02d", i), []string{"many"}, AgoDays(10+i), Said("many", 0)))
	}
	return append(entities,
		Contact("chatter", Methods(MethodEmail, MethodGChat)),
		GroupChat("chatter-chat", []string{"chatter"}, AgoDays(3), Said("chatter", 0)),
	)
}

type telegramGroupChatPlan struct {
	name     string
	members  []string
	messages []GroupMessage
	props    interactionPlanProps
}

// TelegramGroupChat declares one Telegram group chat the member contacts speak
// in, and the messages said in it, oldest first, minutes from the chat's day.
// Every message is a member's; a Telegram group records no message of the user.
// The members must declare the Telegram method. The manifest names the
// interaction holding the chat's latest message for the first member who holds
// one.
func TelegramGroupChat(handle string, members []string, at InteractionProp, messages ...GroupMessage) Entity {
	p := &telegramGroupChatPlan{name: handle, members: members, messages: messages}
	at(&p.props)
	return p
}

func (p *telegramGroupChatPlan) handle() string { return p.name }
func (p *telegramGroupChatPlan) kind() string   { return "interaction" }
func (p *telegramGroupChatPlan) refs() []string { return p.members }
func (p *telegramGroupChatPlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("telegram group chat handle must be non-empty")
	}
	if len(p.members) == 0 {
		return fmt.Errorf("telegram group chat %q: no member contacts", p.name)
	}
	if err := validateInteractionProps(p.name, &p.props, maxSyncedInteractionAgoDays); err != nil {
		return err
	}
	if len(p.messages) == 0 {
		return fmt.Errorf("telegram group chat %q: no messages", p.name)
	}
	for i, m := range p.messages {
		if !slices.Contains(p.members, m.from) {
			return fmt.Errorf("telegram group chat %q: message %d is from %q, not a member", p.name, i, m.from)
		}
		if i > 0 && m.minutes <= p.messages[i-1].minutes {
			return fmt.Errorf("telegram group chat %q: messages must be said oldest first", p.name)
		}
	}
	return nil
}

func runTelegramGroupChat(ctx context.Context, h *replay.Harness, p *telegramGroupChatPlan, st *runState) (Seeded, error) {
	gen := h.Generator()
	target := interactionTarget(gen.Anchor(), *p.props.agoDays)
	// The first spec fixes the chat; each member then keeps one sender identity
	// across the messages they say, and every message a time and text of its own.
	chatID := gen.TelegramGroupMessage(st.specs[p.members[0]], factory.MatchSeeded, len(p.members)+1).ChatID
	identity := map[string]factory.TelegramGroupMessageSpec{}
	for _, handle := range p.members {
		spec, ok := st.specs[handle]
		if !ok {
			return Seeded{}, fmt.Errorf("telegram group chat %q: no contact spec for %q", p.name, handle)
		}
		identity[handle] = gen.TelegramGroupMessageInChat(spec, factory.MatchSeeded, len(p.members)+1, chatID)
	}
	title := gen.Prefix() + p.name
	bySpeaker := map[string][]factory.TelegramGroupMessageSpec{}
	for i, m := range p.messages {
		spec := identity[m.from]
		if len(bySpeaker[m.from]) > 0 {
			spec.TelegramMessageID = gen.TelegramGroupMessageInChat(st.specs[m.from], factory.MatchSeeded, len(p.members)+1, chatID).TelegramMessageID
		}
		spec.ChatTitle = title
		spec.Text = fmt.Sprintf("%s%s message %d", gen.Prefix(), p.name, i+1)
		spec.SentAt = target.Add(time.Duration(m.minutes) * time.Minute)
		bySpeaker[m.from] = append(bySpeaker[m.from], spec)
	}
	for _, handle := range p.members {
		if len(bySpeaker[handle]) == 0 {
			continue
		}
		contactID, err := st.contactID(handle)
		if err != nil {
			return Seeded{}, err
		}
		if _, err := h.ReplayTelegramGroupMessages(ctx, contactID, bySpeaker[handle]); err != nil {
			return Seeded{}, fmt.Errorf("replay telegram group chat %q: %w", p.name, err)
		}
	}

	first := target.Add(time.Duration(p.messages[0].minutes) * time.Minute)
	last := target.Add(time.Duration(p.messages[len(p.messages)-1].minutes) * time.Minute)
	for _, handle := range p.members {
		contactID, err := st.contactID(handle)
		if err != nil {
			return Seeded{}, err
		}
		rows, err := h.InteractionRepo().ListContactInteractions(ctx, contactID, 100, 0)
		if err != nil {
			return Seeded{}, fmt.Errorf("read back telegram group chat %q: %w", p.name, err)
		}
		var latest *repository.Interaction
		for i := range rows {
			r := &rows[i]
			if r.Source != repository.InteractionSourceTelegram || r.OccurredAt.Before(first) || r.OccurredAt.After(last) {
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
	return Seeded{}, fmt.Errorf("telegram group chat %q: no member holds an interaction of it", p.name)
}
