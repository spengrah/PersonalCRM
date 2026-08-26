package declare

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
	chat "google.golang.org/api/chat/v1"
)

var messageInteractionSources = map[string]bool{
	"email": true, "gchat": true, "whatsapp": true, "telegram": true, "messages": true,
}

var loggedInteractionSources = map[string]bool{
	repository.InteractionSourceManual:          true,
	repository.InteractionSourceTodoist:         true,
	repository.InteractionSourceAnarlogSessions: true,
}

const maxSyncedInteractionAgoDays = 60

// maxLoggedInteractionAgoDays covers date-preset boundary fixtures for manual,
// Todoist, and Anarlog rows, which are inserted directly at their target without
// a provider backfill horizon.
const maxLoggedInteractionAgoDays = 100

type interactionPlanProps struct {
	agoDays       *int
	burst         *int
	speakerHandle string
	groupThread   bool
}

type InteractionProp func(*interactionPlanProps)

func AgoDays(n int) InteractionProp {
	return func(p *interactionPlanProps) { p.agoDays = &n }
}

func Burst(n int) InteractionProp {
	return func(p *interactionPlanProps) { p.burst = &n }
}

// GroupThread turns a gchat MessageInteraction into a three-message group
// burst with a second speaker. The subject speaks at target-2m and target,
// while the named speaker contact speaks at target-1m in the same space.
func GroupThread(speakerHandle string) InteractionProp {
	return func(p *interactionPlanProps) {
		p.speakerHandle = speakerHandle
		p.groupThread = true
	}
}

type messageInteractionPlan struct {
	name, contact, source string
	props                 interactionPlanProps
}

func (p *messageInteractionPlan) handle() string { return p.name }
func (p *messageInteractionPlan) kind() string   { return "interaction" }
func (p *messageInteractionPlan) refs() []string {
	refs := []string{p.contact}
	if p.props.speakerHandle != "" {
		refs = append(refs, p.props.speakerHandle)
	}
	return refs
}
func (p *messageInteractionPlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("message interaction handle must be non-empty")
	}
	if !messageInteractionSources[p.source] {
		return fmt.Errorf("message interaction %q: unknown source %q", p.name, p.source)
	}
	if err := validateInteractionProps(p.name, &p.props, maxSyncedInteractionAgoDays); err != nil {
		return err
	}
	if p.props.burst != nil && p.source != "messages" {
		return fmt.Errorf("message interaction %q: Burst is only valid for messages", p.name)
	}
	if p.props.groupThread && p.source != "gchat" {
		return fmt.Errorf("message interaction %q: GroupThread is only valid for gchat", p.name)
	}
	if p.props.groupThread && strings.TrimSpace(p.props.speakerHandle) == "" {
		return fmt.Errorf("message interaction %q: GroupThread speaker handle must be non-empty", p.name)
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
	if err := validateInteractionProps(p.name, &p.props, maxSyncedInteractionAgoDays); err != nil {
		return err
	}
	if p.props.burst != nil {
		return fmt.Errorf("phone call interaction %q: Burst is not valid", p.name)
	}
	if p.props.groupThread {
		return fmt.Errorf("phone call interaction %q: GroupThread is not valid", p.name)
	}
	return nil
}

type loggedInteractionPlan struct {
	name, contact, source string
	props                 interactionPlanProps
}

type linkedMeetingNotePlan struct {
	name, eventHandle string
}

func (p *linkedMeetingNotePlan) handle() string { return p.name }
func (p *linkedMeetingNotePlan) kind() string   { return "meeting_note" }
func (p *linkedMeetingNotePlan) refs() []string { return []string{p.eventHandle} }
func (p *linkedMeetingNotePlan) refKind(ref string) string {
	if ref == p.eventHandle {
		return "calendar_event"
	}
	return "contact"
}
func (p *linkedMeetingNotePlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("linked meeting note handle must be non-empty")
	}
	if strings.TrimSpace(p.eventHandle) == "" {
		return fmt.Errorf("linked meeting note %q: event handle must be non-empty", p.name)
	}
	return nil
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
	if err := validateInteractionProps(p.name, &p.props, maxLoggedInteractionAgoDays); err != nil {
		return err
	}
	if p.props.burst != nil {
		return fmt.Errorf("logged interaction %q: Burst is not valid", p.name)
	}
	if p.props.groupThread {
		return fmt.Errorf("logged interaction %q: GroupThread is not valid", p.name)
	}
	return nil
}

func validateInteractionProps(name string, props *interactionPlanProps, maxAgoDays int) error {
	if props.agoDays == nil {
		return fmt.Errorf("interaction %q: AgoDays is required", name)
	}
	if *props.agoDays < 1 || *props.agoDays > maxAgoDays {
		return fmt.Errorf("interaction %q: AgoDays(%d) is outside 1..%d", name, *props.agoDays, maxAgoDays)
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

// LinkedMeetingNote declares one live meeting note linked to an earlier
// CalendarEvent. It is lowered through the production meeting-note ingest path.
func LinkedMeetingNote(handle, eventHandle string) Entity {
	return &linkedMeetingNotePlan{name: handle, eventHandle: eventHandle}
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
		if p.props.groupThread {
			return runGroupThread(ctx, h, p, st, contactID, targetSpec, target)
		}
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

func runGroupThread(ctx context.Context, h *replay.Harness, p *messageInteractionPlan, st *runState, subjectID uuid.UUID, subjectSpec factory.ContactSpec, target time.Time) (Seeded, error) {
	speakerID, err := st.contactID(p.props.speakerHandle)
	if err != nil {
		return Seeded{}, err
	}
	speakerSpec, ok := st.specs[p.props.speakerHandle]
	if !ok {
		return Seeded{}, fmt.Errorf("group thread %q: no contact spec for %q", p.name, p.props.speakerHandle)
	}
	gen := h.Generator()
	age := interactionMessageAge(gen.Anchor(), target, "gchat")
	base := gen.GChatMessage(subjectSpec, factory.MatchSeeded, factory.WithMessageAge(age+2*time.Minute))
	speakerUser := base.Message.Sender.Name + "-speaker"
	base.Members = append(base.Members, &chat.Membership{State: "JOINED", Member: &chat.User{Name: speakerUser, Type: "HUMAN"}})
	base.EmailByUser[speakerUser] = speakerSpec.Email
	base.Message.Text = fmt.Sprintf("%sgroup-msg-1", gen.Prefix())
	base.Message.CreateTime = target.Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	clone2 := cloneDeclaredGChatMessage(base, "-t2", speakerUser, fmt.Sprintf("%sgroup-msg-2 <b>not-markup</b>", gen.Prefix()), target.Add(-time.Minute))
	clone3 := cloneDeclaredGChatMessage(base, "-t3", base.Message.Sender.Name, fmt.Sprintf("%sgroup-msg-3", gen.Prefix()), target)
	if _, err := h.ReplayGChatBatch(ctx, []replay.GChatBatchItem{
		{ContactID: subjectID, Spec: base},
		{ContactID: speakerID, Spec: clone2},
		{ContactID: subjectID, Spec: clone3},
	}); err != nil {
		return Seeded{}, fmt.Errorf("replay group thread %q: %w", p.name, err)
	}
	rows, err := h.InteractionRepo().ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: subjectID, Limit: 100})
	if err != nil {
		return Seeded{}, fmt.Errorf("read back group thread %q: %w", p.name, err)
	}
	min, max := target.Add(-2*time.Minute), target
	var matches []repository.Interaction
	for i := range rows {
		if rows[i].Source == repository.InteractionSourceGChat && !rows[i].OccurredAt.Before(min) && !rows[i].OccurredAt.After(max) {
			matches = append(matches, rows[i])
		}
	}
	if len(matches) != 1 {
		return Seeded{}, fmt.Errorf("group thread %q: expected exactly one gchat interaction in [%s,%s], found %d", p.name, min.Format(time.RFC3339), max.Format(time.RFC3339), len(matches))
	}
	return Seeded{Kind: "interaction", ID: matches[0].ID.String(), Name: p.name}, nil
}

func cloneDeclaredGChatMessage(base factory.GChatMessageSpec, suffix, sender, text string, createTime time.Time) factory.GChatMessageSpec {
	name := base.ExternalID + suffix
	clone := base
	clone.Message = &chat.Message{
		Name:       name,
		Sender:     &chat.User{Name: sender, Type: "HUMAN"},
		Text:       text,
		CreateTime: createTime.UTC().Format(time.RFC3339Nano),
	}
	clone.ExternalID = name
	return clone
}

func anarlogSessionUUID(factoryPrefix, handle string) uuid.UUID {
	var id uuid.UUID
	prefix := sha256.Sum256([]byte("anarlog:" + factoryPrefix))
	tail := sha256.Sum256([]byte("anarlog-tail:" + factoryPrefix + ":" + handle))
	copy(id[:12], prefix[:12])
	copy(id[12:], tail[:4])
	return id
}

func anarlogSessionEventPrefix(factoryPrefix string) string {
	hash := sha256.Sum256([]byte("anarlog:" + factoryPrefix))
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%04x", hash[0:4], hash[4:6], hash[6:8], hash[8:10], hash[10:12])
}

func runLinkedMeetingNote(ctx context.Context, h *replay.Harness, p *linkedMeetingNotePlan, st *runState) (Seeded, error) {
	seededEvent, ok := st.seeded[p.eventHandle]
	if !ok {
		return Seeded{}, fmt.Errorf("linked meeting note %q: event handle %q has not been created", p.name, p.eventHandle)
	}
	eventID, err := uuid.Parse(seededEvent.ID)
	if err != nil {
		return Seeded{}, fmt.Errorf("linked meeting note %q: parse event id: %w", p.name, err)
	}
	events, err := h.CalendarEventRepo().ListByIDs(ctx, []uuid.UUID{eventID})
	if err != nil {
		return Seeded{}, fmt.Errorf("linked meeting note %q: read event: %w", p.name, err)
	}
	if len(events) != 1 {
		return Seeded{}, fmt.Errorf("linked meeting note %q: expected one calendar event, found %d", p.name, len(events))
	}
	gen := h.Generator()
	prefix := gen.Prefix()
	title := strings.ToLower(fmt.Sprintf("%s%s note", prefix, p.name))
	summary := fmt.Sprintf("Summary for %s%s", prefix, p.name)
	memo := fmt.Sprintf("Memo for %s%s <i>not-markup</i>", prefix, p.name)
	sessionID := anarlogSessionUUID(prefix, p.name)
	if err := h.ReplayMeetingNoteRecorded(ctx, replay.MeetingNoteRecordedSpec{
		SessionID: sessionID, Title: &title, Summary: &summary, Memo: &memo,
		MeetingAt: events[0].StartTime,
	}); err != nil {
		return Seeded{}, fmt.Errorf("linked meeting note %q: %w", p.name, err)
	}
	notes, err := repository.NewMeetingNoteRepository(h.Database().Queries).ListBySessionIDs(ctx, []uuid.UUID{sessionID})
	if err != nil {
		return Seeded{}, fmt.Errorf("linked meeting note %q: read back note: %w", p.name, err)
	}
	if len(notes) != 1 {
		return Seeded{}, fmt.Errorf("linked meeting note %q: expected one note for session, found %d", p.name, len(notes))
	}
	note := notes[0]
	if note.LinkageState != repository.LinkageStateLinked || note.LinkedKind == nil || *note.LinkedKind != repository.LinkedKindEvent || note.LinkedID == nil || *note.LinkedID != eventID {
		return Seeded{}, fmt.Errorf("linked meeting note %q: unexpected linkage state=%q kind=%v id=%v", p.name, note.LinkageState, note.LinkedKind, note.LinkedID)
	}
	return Seeded{Kind: "meeting_note", ID: note.ID.String(), Name: title}, nil
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
