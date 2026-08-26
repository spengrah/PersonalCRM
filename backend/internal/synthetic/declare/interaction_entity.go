package declare

import (
	"context"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
)

var messageInteractionSources = map[string]bool{
	"email": true, "gchat": true, "whatsapp": true, "telegram": true, "messages": true,
}

var loggedInteractionSources = map[string]bool{
	repository.InteractionSourceManual:          true,
	repository.InteractionSourceTodoist:         true,
	repository.InteractionSourceAnarlogSessions: true,
}

type interactionPlanProps struct {
	agoDays *int
	burst   *int
}

type InteractionProp func(*interactionPlanProps)

func AgoDays(n int) InteractionProp {
	return func(p *interactionPlanProps) { p.agoDays = &n }
}

func Burst(n int) InteractionProp {
	return func(p *interactionPlanProps) { p.burst = &n }
}

type messageInteractionPlan struct {
	name, contact, source string
	props                 interactionPlanProps
}

func (p *messageInteractionPlan) handle() string { return p.name }
func (p *messageInteractionPlan) kind() string   { return "interaction" }
func (p *messageInteractionPlan) refs() []string { return []string{p.contact} }
func (p *messageInteractionPlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("message interaction handle must be non-empty")
	}
	if !messageInteractionSources[p.source] {
		return fmt.Errorf("message interaction %q: unknown source %q", p.name, p.source)
	}
	if err := validateInteractionProps(p.name, &p.props); err != nil {
		return err
	}
	if p.props.burst != nil && p.source != "messages" {
		return fmt.Errorf("message interaction %q: Burst is only valid for messages", p.name)
	}
	return nil
}

type phoneCallInteractionPlan struct {
	name, contact string
	props         interactionPlanProps
}

func (p *phoneCallInteractionPlan) handle() string { return p.name }
func (p *phoneCallInteractionPlan) kind() string   { return "interaction" }
func (p *phoneCallInteractionPlan) refs() []string { return []string{p.contact} }
func (p *phoneCallInteractionPlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("phone call interaction handle must be non-empty")
	}
	if err := validateInteractionProps(p.name, &p.props); err != nil {
		return err
	}
	if p.props.burst != nil {
		return fmt.Errorf("phone call interaction %q: Burst is not valid", p.name)
	}
	return nil
}

type loggedInteractionPlan struct {
	name, contact, source string
	props                 interactionPlanProps
}

func (p *loggedInteractionPlan) handle() string { return p.name }
func (p *loggedInteractionPlan) kind() string   { return "interaction" }
func (p *loggedInteractionPlan) refs() []string { return []string{p.contact} }
func (p *loggedInteractionPlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("logged interaction handle must be non-empty")
	}
	if !loggedInteractionSources[p.source] {
		return fmt.Errorf("logged interaction %q: unknown source %q", p.name, p.source)
	}
	if err := validateInteractionProps(p.name, &p.props); err != nil {
		return err
	}
	if p.props.burst != nil {
		return fmt.Errorf("logged interaction %q: Burst is not valid", p.name)
	}
	return nil
}

func validateInteractionProps(name string, props *interactionPlanProps) error {
	if props.agoDays == nil {
		return fmt.Errorf("interaction %q: AgoDays is required", name)
	}
	if *props.agoDays < 1 || *props.agoDays > 60 {
		return fmt.Errorf("interaction %q: AgoDays(%d) is outside 1..60", name, *props.agoDays)
	}
	if props.burst != nil && (*props.burst < 1 || *props.burst > 3) {
		return fmt.Errorf("interaction %q: Burst(%d) is outside 1..3", name, *props.burst)
	}
	return nil
}

func MessageInteraction(handle, contactHandle, source string, props ...InteractionProp) Entity {
	p := &messageInteractionPlan{name: handle, contact: contactHandle, source: source}
	for _, prop := range props {
		prop(&p.props)
	}
	return p
}

func PhoneCallInteraction(handle, contactHandle string, props ...InteractionProp) Entity {
	p := &phoneCallInteractionPlan{name: handle, contact: contactHandle}
	for _, prop := range props {
		prop(&p.props)
	}
	return p
}

func LoggedInteraction(handle, contactHandle, source string, props ...InteractionProp) Entity {
	p := &loggedInteractionPlan{name: handle, contact: contactHandle, source: source}
	for _, prop := range props {
		prop(&p.props)
	}
	return p
}

func interactionTarget(anchor time.Time, n int) time.Time {
	return anchor.Truncate(time.Second).Add(-time.Duration(n) * 24 * time.Hour).UTC()
}

func interactionMessageAge(anchor, target time.Time, source string) time.Duration {
	defaultOffset := time.Hour
	if source == "email" {
		defaultOffset = 2 * time.Hour
	}
	age := anchor.Sub(target) - defaultOffset
	if age < 0 {
		return 0
	}
	return age
}

func runMessageInteraction(ctx context.Context, h *replay.Harness, p *messageInteractionPlan, st *runState) (Seeded, error) {
	contactID, err := st.contactID(p.contact)
	if err != nil {
		return Seeded{}, err
	}
	targetSpec, ok := st.specs[p.contact]
	if !ok {
		return Seeded{}, fmt.Errorf("message interaction %q: no contact spec for %q", p.name, p.contact)
	}
	gen := h.Generator()
	target := interactionTarget(gen.Anchor(), *p.props.agoDays)
	age := interactionMessageAge(gen.Anchor(), target, p.source)
	count := 1
	if p.props.burst != nil {
		count = *p.props.burst
	}

	switch p.source {
	case "email":
		msg := gen.GmailMessage(targetSpec, factory.MatchSeeded, factory.WithMessageAge(age))
		if _, err := h.ReplayGmail(ctx, contactID, msg); err != nil {
			return Seeded{}, fmt.Errorf("replay email interaction %q: %w", p.name, err)
		}
	case "gchat":
		msg := gen.GChatMessage(targetSpec, factory.MatchSeeded, factory.WithMessageAge(age))
		if _, err := h.ReplayGChat(ctx, contactID, msg); err != nil {
			return Seeded{}, fmt.Errorf("replay gchat interaction %q: %w", p.name, err)
		}
	case "whatsapp":
		msg := gen.WhatsAppMessage(targetSpec, factory.MatchSeeded, factory.WithMessageAge(age))
		if _, err := h.ReplayWhatsApp(ctx, contactID, msg); err != nil {
			return Seeded{}, fmt.Errorf("replay whatsapp interaction %q: %w", p.name, err)
		}
	case "telegram":
		msg := gen.TelegramMessage(targetSpec, factory.MatchSeeded, factory.WithMessageAge(age))
		if _, err := h.ReplayTelegram(ctx, contactID, msg); err != nil {
			return Seeded{}, fmt.Errorf("replay telegram interaction %q: %w", p.name, err)
		}
	case "messages":
		items := make([]replay.IMessageBatchItem, 0, count)
		chatID := ""
		for i := count - 1; i >= 0; i-- {
			itemAge := age + time.Duration(i)*time.Minute
			msg, err := gen.IMessage(targetSpec, factory.MatchSeeded, h.MacHostID(), factory.WithMessageAge(itemAge))
			if err != nil {
				return Seeded{}, fmt.Errorf("build messages interaction %q: %w", p.name, err)
			}
			if chatID == "" {
				var payload events.RawMessageReceivedPayload
				if err := events.Unmarshal(msg.Envelope, &payload); err != nil {
					return Seeded{}, fmt.Errorf("decode messages interaction %q: %w", p.name, err)
				}
				chatID = payload.ChatID
			} else if err := setIMessageChatID(msg.Envelope, chatID); err != nil {
				return Seeded{}, fmt.Errorf("share messages burst chat %q: %w", p.name, err)
			}
			items = append(items, replay.IMessageBatchItem{ContactID: contactID, Spec: msg})
		}
		if _, err := h.ReplayIMessageBatch(ctx, items); err != nil {
			return Seeded{}, fmt.Errorf("replay messages interaction %q: %w", p.name, err)
		}
	}
	interaction, err := findInteractionAt(ctx, h, contactID, sourceForMessage(p.source), target)
	if err != nil {
		return Seeded{}, fmt.Errorf("read back message interaction %q: %w", p.name, err)
	}
	return Seeded{Kind: "interaction", ID: interaction.ID.String(), Name: p.name}, nil
}

func setIMessageChatID(envelope *events.Envelope, chatID string) error {
	var payload events.RawMessageReceivedPayload
	if err := events.Unmarshal(envelope, &payload); err != nil {
		return err
	}
	payload.ChatID = chatID
	raw, err := events.Marshal(envelope.Kind, payload)
	if err != nil {
		return err
	}
	envelope.Payload = raw
	return nil
}

func sourceForMessage(source string) string {
	if source == "messages" {
		return repository.InteractionSourceMessages
	}
	return source
}

func runLoggedInteraction(ctx context.Context, h *replay.Harness, p *loggedInteractionPlan, st *runState) (Seeded, error) {
	contactID, err := st.contactID(p.contact)
	if err != nil {
		return Seeded{}, err
	}
	target := interactionTarget(h.Generator().Anchor(), *p.props.agoDays)
	var sourceRef *string
	if p.source == repository.InteractionSourceAnarlogSessions {
		ref := fmt.Sprintf("anarlog:%s:%s", uuid.NewString(), contactID)
		sourceRef = &ref
	}
	row, err := h.InteractionRepo().TestInsertInteraction(ctx, uuid.New(), contactID, p.source, sourceRef, target, repository.InteractionDirectionOutbound)
	if err != nil {
		return Seeded{}, fmt.Errorf("insert logged interaction %q: %w", p.name, err)
	}
	return Seeded{Kind: "interaction", ID: row.ID.String(), Name: p.name}, nil
}

func runPhoneCallInteraction(ctx context.Context, h *replay.Harness, p *phoneCallInteractionPlan, st *runState) (Seeded, error) {
	contactID, err := st.contactID(p.contact)
	if err != nil {
		return Seeded{}, err
	}
	target := interactionTarget(h.Generator().Anchor(), *p.props.agoDays)
	peer := h.Generator().Prefix() + "phone-peer"
	if spec, ok := st.specs[p.contact]; ok && spec.Phone != "" {
		peer = spec.Phone
	}
	answered := true
	callRepo := repository.NewPhoneCallRepository(h.Database().Queries)
	call, err := callRepo.UpsertCall(ctx, repository.UpsertPhoneCallParams{
		CallUniqueID: h.Generator().Prefix() + "call-" + p.name,
		PeerHandle:   peer, PeerNormalized: peer,
		Service: repository.PhoneCallServiceVoice, Direction: repository.PhoneCallDirectionInbound,
		Answered: &answered, HasVoicemail: false, DurationSeconds: 372, StartedAt: target,
		MatchedContactID: &contactID, MacHostID: func() *uuid.UUID { id := h.MacHostID(); return &id }(),
	})
	if err != nil {
		return Seeded{}, fmt.Errorf("insert phone call %q: %w", p.name, err)
	}
	interaction, err := h.InteractionRepo().TestInsertInteraction(ctx, uuid.New(), contactID, repository.InteractionSourcePhoneCalls, nil, target, repository.InteractionDirectionInbound)
	if err != nil {
		return Seeded{}, fmt.Errorf("insert phone interaction %q: %w", p.name, err)
	}
	if err := callRepo.MarkProcessed(ctx, repository.MarkProcessedParams{ID: call.ID, InteractionID: &interaction.ID}); err != nil {
		return Seeded{}, fmt.Errorf("link phone interaction %q: %w", p.name, err)
	}
	return Seeded{Kind: "interaction", ID: interaction.ID.String(), Name: p.name}, nil
}

func findInteractionAt(ctx context.Context, h *replay.Harness, contactID uuid.UUID, source string, target time.Time) (*repository.Interaction, error) {
	rows, err := h.InteractionRepo().ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: contactID, Limit: 100})
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Source == source && rows[i].OccurredAt.Equal(target) {
			return &rows[i], nil
		}
	}
	return nil, fmt.Errorf("no %s interaction at %s", source, target.Format(time.RFC3339))
}
