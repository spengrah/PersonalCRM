package service

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

type VenueTag struct {
	Key, Label, Kind string
	IsGroup          bool
}
type EventContent struct {
	Title, Location    *string
	AttendeeCount      int
	StartTime, EndTime time.Time
	HTMLLink           *string
}
type CallContent struct {
	Service         string
	Answered        *bool
	HasVoicemail    bool
	DurationSeconds int
}
type InteractionEnrichment struct {
	Label        string
	ContentKind  string
	MessageCount int
	IsGroup      bool
	VenueTags    []VenueTag
	Event        *EventContent
	Call         *CallContent
}
type ContentMessage struct {
	ID         uuid.UUID
	Sender     string
	IsOutgoing bool
	SentAt     time.Time
	Body       string
	VenueKey   string
}
type MeetingNoteContent struct{ Title, Summary, Memo *string }
type InteractionContent struct {
	Kind         string
	Messages     []ContentMessage
	MeetingNotes []MeetingNoteContent
}

type InteractionContentService struct {
	interactions   *repository.InteractionRepository
	comms          *repository.CommsMessageRepository
	telegram       *repository.TelegramMessageRepository
	messages       *repository.MessagesMessageRepository
	meetingNotes   *repository.MeetingNoteRepository
	calendarEvents *repository.CalendarEventRepository
	phoneCalls     *repository.PhoneCallRepository
}

func NewInteractionContentService(interactions *repository.InteractionRepository, comms *repository.CommsMessageRepository, telegram *repository.TelegramMessageRepository, messages *repository.MessagesMessageRepository, meetingNotes *repository.MeetingNoteRepository, calendarEvents *repository.CalendarEventRepository, phoneCalls *repository.PhoneCallRepository) *InteractionContentService {
	return &InteractionContentService{interactions: interactions, comms: comms, telegram: telegram, messages: messages, meetingNotes: meetingNotes, calendarEvents: calendarEvents, phoneCalls: phoneCalls}
}

func (s *InteractionContentService) EnrichPage(ctx context.Context, interactions []repository.Interaction) (map[uuid.UUID]InteractionEnrichment, error) {
	out := make(map[uuid.UUID]InteractionEnrichment, len(interactions))
	if len(interactions) == 0 {
		return out, nil
	}
	bySource := map[string][]uuid.UUID{}
	for _, row := range interactions {
		bySource[row.Source] = append(bySource[row.Source], row.ID)
	}
	commsIDs := append(append([]uuid.UUID{}, bySource[repository.InteractionSourceEmail]...), bySource[repository.InteractionSourceGChat]...)
	commsIDs = append(commsIDs, bySource[repository.InteractionSourceWhatsApp]...)
	commsSummary, err := s.comms.SummarizeByInteractionIDs(ctx, commsIDs)
	if err != nil {
		return nil, err
	}
	tgSummary, err := s.telegram.SummarizeByInteractionIDs(ctx, bySource[repository.InteractionSourceTelegram])
	if err != nil {
		return nil, err
	}
	msgSummary, err := s.messages.SummarizeByInteractionIDs(ctx, bySource[repository.InteractionSourceMessages])
	if err != nil {
		return nil, err
	}
	callRows, err := s.phoneCalls.ListByInteractionIDs(ctx, bySource[repository.InteractionSourcePhoneCalls])
	if err != nil {
		return nil, err
	}
	eventIDs, sessionIDs := []uuid.UUID{}, []uuid.UUID{}
	for _, row := range interactions {
		if row.Source == repository.InteractionSourceGCal {
			if id, ok := parseUUID(row.SourceRef); ok {
				eventIDs = append(eventIDs, id)
			}
		}
		if row.Source == repository.InteractionSourceAnarlogSessions {
			if id, ok := parseSessionUUID(row.SourceRef); ok {
				sessionIDs = append(sessionIDs, id)
			}
		}
	}
	eventIDs, sessionIDs = uniqueUUIDs(eventIDs), uniqueUUIDs(sessionIDs)
	events, err := s.calendarEvents.ListByIDs(ctx, eventIDs)
	if err != nil {
		return nil, err
	}
	gcalNotes, err := s.meetingNotes.ListByLinkedRefs(ctx, repository.LinkedKindEvent, eventIDs)
	if err != nil {
		return nil, err
	}
	anarlogNotes, err := s.meetingNotes.ListBySessionIDs(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	eventByID := map[uuid.UUID]repository.CalendarEvent{}
	for _, row := range events {
		eventByID[row.ID] = row
	}
	notesByEvent := map[uuid.UUID][]repository.MeetingNote{}
	for _, row := range gcalNotes {
		if row.LinkedID != nil {
			notesByEvent[*row.LinkedID] = append(notesByEvent[*row.LinkedID], row)
		}
	}
	notesBySession := map[uuid.UUID][]repository.MeetingNote{}
	for _, row := range anarlogNotes {
		notesBySession[row.AnarlogSessionID] = append(notesBySession[row.AnarlogSessionID], row)
	}
	commsByID, tgByID, msgByID := map[uuid.UUID][]repository.CommsInteractionSummary{}, map[uuid.UUID][]repository.TelegramInteractionSummary{}, map[uuid.UUID][]repository.MessagesInteractionSummary{}
	for _, row := range commsSummary {
		commsByID[row.InteractionID] = append(commsByID[row.InteractionID], row)
	}
	for _, row := range tgSummary {
		tgByID[row.InteractionID] = append(tgByID[row.InteractionID], row)
	}
	for _, row := range msgSummary {
		msgByID[row.InteractionID] = append(msgByID[row.InteractionID], row)
	}
	callsByID := map[uuid.UUID]repository.PhoneCall{}
	for _, row := range callRows {
		if row.InteractionID != nil {
			callsByID[*row.InteractionID] = row
		}
	}
	for _, row := range interactions {
		weighted, count := []weightedVenue{}, int64(0)
		switch row.Source {
		case repository.InteractionSourceEmail, repository.InteractionSourceGChat, repository.InteractionSourceWhatsApp:
			for _, item := range commsByID[row.ID] {
				count += item.MessageCount
				if item.ThreadID != nil {
					weighted = append(weighted, weightedVenue{deriveVenue(item.Source, *item.ThreadID, "", nil, item.LatestSubject, false), item.MessageCount})
				}
			}
		case repository.InteractionSourceTelegram:
			for _, item := range tgByID[row.ID] {
				count += item.MessageCount
				weighted = append(weighted, weightedVenue{deriveVenue("telegram", strconv.FormatInt(item.ChatID, 10), item.ChatType, item.ChatTitle, nil, false), item.MessageCount})
			}
		case repository.InteractionSourceMessages:
			for _, item := range msgByID[row.ID] {
				count += item.MessageCount
				weighted = append(weighted, weightedVenue{deriveVenue("messages", item.ChatGuid, "", nil, nil, item.IsGroupChat), item.MessageCount})
			}
		}
		tags := venueTags(weighted)
		var eventTitle *string
		var event *EventContent
		if row.Source == repository.InteractionSourceGCal {
			if id, ok := parseUUID(row.SourceRef); ok {
				if found, exists := eventByID[id]; exists {
					eventTitle, event = found.Title, eventContent(found)
				}
			}
		}
		noteCount := 0
		if row.Source == repository.InteractionSourceGCal {
			if id, ok := parseUUID(row.SourceRef); ok {
				noteCount = len(notesByEvent[id])
			}
		}
		if row.Source == repository.InteractionSourceAnarlogSessions {
			if id, ok := parseSessionUUID(row.SourceRef); ok {
				noteCount = len(notesBySession[id])
			}
		}
		var call *CallContent
		if found, ok := callsByID[row.ID]; ok {
			call = &CallContent{found.Service, found.Answered, found.HasVoicemail, int(found.DurationSeconds)}
		}
		out[row.ID] = InteractionEnrichment{Label: deriveLabel(row, eventTitle, weighted), ContentKind: contentKind(row.Source, noteCount), MessageCount: int(count), IsGroup: anyGroup(tags), VenueTags: tags, Event: event, Call: call}
	}
	return out, nil
}

func (s *InteractionContentService) GetContent(ctx context.Context, id uuid.UUID) (*InteractionContent, error) {
	interaction, err := s.interactions.GetInteraction(ctx, id)
	if err != nil {
		return nil, err
	}
	result := &InteractionContent{Messages: []ContentMessage{}, MeetingNotes: []MeetingNoteContent{}}
	switch interaction.Source {
	case repository.InteractionSourceGCal:
		parsed, ok := parseUUID(interaction.SourceRef)
		if !ok {
			result.Kind = "none"
			return result, nil
		}
		notes, err := s.meetingNotes.ListByLinkedRefs(ctx, repository.LinkedKindEvent, []uuid.UUID{parsed})
		if err != nil {
			return nil, err
		}
		result.Kind = "none"
		if len(notes) > 0 {
			result.Kind, result.MeetingNotes = "meeting_note", meetingNotesContent(notes)
		}
		return result, nil
	case repository.InteractionSourceAnarlogSessions:
		parsed, ok := parseSessionUUID(interaction.SourceRef)
		if !ok {
			result.Kind = "meeting_note"
			return result, nil
		}
		notes, err := s.meetingNotes.ListBySessionIDs(ctx, []uuid.UUID{parsed})
		if err != nil {
			return nil, err
		}
		result.Kind, result.MeetingNotes = "meeting_note", meetingNotesContent(notes)
		return result, nil
	case repository.InteractionSourcePhoneCalls:
		result.Kind = "call"
		return result, nil
	case repository.InteractionSourceManual:
		result.Kind = "none"
		return result, nil
	case repository.InteractionSourceTodoist:
		result.Kind = "none"
		return result, nil
	}
	result.Kind = "messages"
	switch interaction.Source {
	case repository.InteractionSourceEmail, repository.InteractionSourceGChat, repository.InteractionSourceWhatsApp:
		rows, err := s.comms.ListByInteractionIDs(ctx, []uuid.UUID{id})
		if err != nil {
			return nil, err
		}
		result.Messages, err = s.expandComms(ctx, id, rows)
		if err != nil {
			return nil, err
		}
	case repository.InteractionSourceTelegram:
		rows, err := s.telegram.ListByInteractionIDs(ctx, []uuid.UUID{id})
		if err != nil {
			return nil, err
		}
		result.Messages, err = s.expandTelegram(ctx, rows)
		if err != nil {
			return nil, err
		}
	case repository.InteractionSourceMessages:
		rows, err := s.messages.ListByInteractionIDs(ctx, []uuid.UUID{id})
		if err != nil {
			return nil, err
		}
		result.Messages, err = s.expandMessages(ctx, rows)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *InteractionContentService) ListContactVenues(ctx context.Context, contactID uuid.UUID) ([]VenueTag, error) {
	comms, err := s.comms.ListContainersForContact(ctx, contactID)
	if err != nil {
		return nil, err
	}
	tg, err := s.telegram.ListContainersForContact(ctx, contactID)
	if err != nil {
		return nil, err
	}
	msgs, err := s.messages.ListContainersForContact(ctx, contactID)
	if err != nil {
		return nil, err
	}
	tags := []VenueTag{}
	for _, row := range comms {
		tags = append(tags, deriveVenue(row.Source, row.ThreadID, "", nil, row.LatestSubject, false))
	}
	for _, row := range tg {
		tags = append(tags, deriveVenue("telegram", strconv.FormatInt(row.ChatID, 10), row.ChatType, row.ChatTitle, nil, false))
	}
	for _, row := range msgs {
		tags = append(tags, deriveVenue("messages", row.ChatGuid, "", nil, nil, row.IsGroupChat))
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Key < tags[j].Key })
	return tags, nil
}

type weightedVenue struct {
	tag   VenueTag
	count int64
}

func deriveLabel(interaction repository.Interaction, eventTitle *string, venues []weightedVenue) string {
	if interaction.Description != nil && *interaction.Description != "" {
		return *interaction.Description
	}
	if interaction.Source == repository.InteractionSourceGCal && eventTitle != nil && *eventTitle != "" {
		return *eventTitle
	}
	if len(venues) > 0 {
		best := venues[0]
		for _, candidate := range venues[1:] {
			if candidate.count > best.count || (candidate.count == best.count && candidate.tag.Key < best.tag.Key) {
				best = candidate
			}
		}
		return best.tag.Label
	}
	switch interaction.Source {
	case repository.InteractionSourcePhoneCalls:
		return "Phone call"
	case repository.InteractionSourceGCal, repository.InteractionSourceAnarlogSessions:
		return "Meeting"
	case repository.InteractionSourceManual:
		return "Logged interaction"
	case repository.InteractionSourceTodoist:
		return "Todoist task"
	default:
		return "Messages"
	}
}
func deriveVenue(source, container, chatType string, title, subject *string, isGroup bool) VenueTag {
	tag := VenueTag{Key: source + ":" + container}
	switch source {
	case repository.InteractionSourceEmail:
		tag.Kind, tag.Label = "email_thread", "Email thread"
		if subject != nil && *subject != "" {
			tag.Label = *subject
		}
	case repository.InteractionSourceGChat:
		tag.Kind, tag.Label, tag.IsGroup = "group_chat", "Group chat", true
	case repository.InteractionSourceWhatsApp:
		tag.IsGroup = strings.HasSuffix(container, "@g.us")
		if tag.IsGroup {
			tag.Kind, tag.Label = "group_chat", "Group chat"
		} else {
			tag.Kind, tag.Label = "dm", "DM"
		}
	case repository.InteractionSourceTelegram:
		tag.IsGroup = chatType != "private"
		if tag.IsGroup {
			tag.Kind, tag.Label = "group_chat", "Group chat"
			if title != nil && *title != "" {
				tag.Label = *title
			}
		} else {
			tag.Kind, tag.Label = "dm", "DM"
		}
	case repository.InteractionSourceMessages:
		tag.IsGroup = isGroup
		if isGroup {
			tag.Kind, tag.Label = "group_chat", "Group chat"
		} else {
			tag.Kind, tag.Label = "dm", "DM"
		}
	}
	return tag
}
func deriveSender(source string, outgoing bool, peer *string, metadata []byte, first, last, username *string) string {
	if outgoing {
		return "Me"
	}
	if source == repository.InteractionSourceTelegram {
		if name := joinNames(first, last); name != "" {
			return name
		}
		if username != nil && *username != "" {
			return *username
		}
		return "Unknown"
	}
	if source == repository.InteractionSourceWhatsApp {
		values := map[string]any{}
		if json.Unmarshal(metadata, &values) == nil {
			if name, ok := values["push_name"].(string); ok && name != "" {
				return name
			}
		}
	}
	if peer != nil && *peer != "" {
		return *peer
	}
	return "Unknown"
}

func (s *InteractionContentService) expandComms(ctx context.Context, interactionID uuid.UUID, linked []repository.CommsMessage) ([]ContentMessage, error) {
	type bounds struct {
		source, thread string
		from, to       time.Time
	}
	containers := map[string]bounds{}
	chosen := map[string]repository.CommsMessage{}
	for _, row := range linked {
		if row.ThreadID == nil {
			// A live FK'd row without a thread is still part of the
			// interaction's content. It has no window/container to expand,
			// but must be rendered directly and carries no venue key.
			key := row.Source + "\x00" + row.ExternalID
			old, exists := chosen[key]
			if !exists || (isExpanded(row, interactionID) && !isExpanded(old, interactionID)) || (isExpanded(row, interactionID) == isExpanded(old, interactionID) && bytes.Compare(row.ID[:], old.ID[:]) < 0) {
				chosen[key] = row
			}
			continue
		}
		key := row.Source + "\x00" + *row.ThreadID
		value, ok := containers[key]
		if !ok || row.SentAt.Before(value.from) {
			value.from = row.SentAt
		}
		if !ok || row.SentAt.After(value.to) {
			value.to = row.SentAt
		}
		value.source, value.thread = row.Source, *row.ThreadID
		containers[key] = value
	}
	for _, container := range containers {
		rows, err := s.comms.ListByThreadWindow(ctx, container.source, container.thread, container.from, container.to)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			key := row.Source + "\x00" + row.ExternalID
			old, exists := chosen[key]
			if !exists || (isExpanded(row, interactionID) && !isExpanded(old, interactionID)) || (isExpanded(row, interactionID) == isExpanded(old, interactionID) && bytes.Compare(row.ID[:], old.ID[:]) < 0) {
				chosen[key] = row
			}
		}
	}
	out := []ContentMessage{}
	for _, row := range chosen {
		venueKey := ""
		if row.ThreadID != nil {
			venueKey = row.Source + ":" + *row.ThreadID
		}
		out = append(out, ContentMessage{ID: row.ID, Sender: deriveSender(row.Source, row.Direction == repository.InteractionDirectionOutbound, row.PeerHandle, row.SourceMetadata, nil, nil, nil), IsOutgoing: row.Direction == repository.InteractionDirectionOutbound, SentAt: row.SentAt, Body: stringValue(row.Body), VenueKey: venueKey})
	}
	sortContentMessages(out)
	return out, nil
}
func (s *InteractionContentService) expandTelegram(ctx context.Context, linked []repository.TelegramMessage) ([]ContentMessage, error) {
	windows := map[int64][2]time.Time{}
	for _, row := range linked {
		value, ok := windows[row.TelegramChatID]
		if !ok || row.SentAt.Before(value[0]) {
			value[0] = row.SentAt
		}
		if !ok || row.SentAt.After(value[1]) {
			value[1] = row.SentAt
		}
		windows[row.TelegramChatID] = value
	}
	out := []ContentMessage{}
	for chat, window := range windows {
		rows, err := s.telegram.ListByChatWindow(ctx, chat, window[0], window[1])
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out = append(out, ContentMessage{ID: row.ID, Sender: deriveSender("telegram", row.IsOutgoing, nil, nil, row.PeerFirstName, row.PeerLastName, row.PeerUsername), IsOutgoing: row.IsOutgoing, SentAt: row.SentAt, Body: stringValue(row.MessageText), VenueKey: "telegram:" + strconv.FormatInt(row.TelegramChatID, 10)})
		}
	}
	sortContentMessages(out)
	return out, nil
}
func (s *InteractionContentService) expandMessages(ctx context.Context, linked []repository.MessagesMessage) ([]ContentMessage, error) {
	windows := map[string][2]time.Time{}
	for _, row := range linked {
		value, ok := windows[row.ChatGuid]
		if !ok || row.SentAt.Before(value[0]) {
			value[0] = row.SentAt
		}
		if !ok || row.SentAt.After(value[1]) {
			value[1] = row.SentAt
		}
		windows[row.ChatGuid] = value
	}
	out := []ContentMessage{}
	for chat, window := range windows {
		rows, err := s.messages.ListByChatWindow(ctx, chat, window[0], window[1])
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			peer := row.PeerHandle
			out = append(out, ContentMessage{ID: row.ID, Sender: deriveSender("messages", row.IsOutgoing, &peer, nil, nil, nil, nil), IsOutgoing: row.IsOutgoing, SentAt: row.SentAt, Body: stringValue(row.Text), VenueKey: "messages:" + row.ChatGuid})
		}
	}
	sortContentMessages(out)
	return out, nil
}

func eventContent(event repository.CalendarEvent) *EventContent {
	return &EventContent{Title: event.Title, Location: event.Location, AttendeeCount: len(event.Attendees), StartTime: event.StartTime, EndTime: event.EndTime, HTMLLink: event.HtmlLink}
}
func meetingNotesContent(notes []repository.MeetingNote) []MeetingNoteContent {
	out := make([]MeetingNoteContent, len(notes))
	for i, row := range notes {
		out[i] = MeetingNoteContent{Title: row.Title, Summary: row.Summary, Memo: row.Memo}
	}
	return out
}
func contentKind(source string, noteCount int) string {
	switch source {
	case repository.InteractionSourceGCal:
		if noteCount > 0 {
			return "meeting_note"
		}
		return "none"
	case repository.InteractionSourceAnarlogSessions:
		return "meeting_note"
	case repository.InteractionSourcePhoneCalls:
		return "call"
	case repository.InteractionSourceManual, repository.InteractionSourceTodoist:
		return "none"
	default:
		return "messages"
	}
}
func venueTags(weighted []weightedVenue) []VenueTag {
	seen := map[string]VenueTag{}
	for _, row := range weighted {
		seen[row.tag.Key] = row.tag
	}
	out := make([]VenueTag, 0, len(seen))
	for _, row := range seen {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
func anyGroup(tags []VenueTag) bool {
	for _, row := range tags {
		if row.IsGroup {
			return true
		}
	}
	return false
}
func parseUUID(value *string) (uuid.UUID, bool) {
	if value == nil {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(*value)
	return id, err == nil
}
func parseSessionUUID(value *string) (uuid.UUID, bool) {
	if value == nil {
		return uuid.Nil, false
	}
	fields := strings.Split(*value, ":")
	if len(fields) < 3 || fields[0] != "anarlog" || fields[2] == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(fields[1])
	if err != nil {
		return uuid.Nil, false
	}
	switch fields[2] {
	case "title", "walkin":
		if len(fields) != 4 || fields[3] == "" {
			return uuid.Nil, false
		}
		if _, err := uuid.Parse(fields[3]); err != nil {
			return uuid.Nil, false
		}
	default:
		if len(fields) != 3 {
			return uuid.Nil, false
		}
		if _, err := uuid.Parse(fields[2]); err != nil {
			return uuid.Nil, false
		}
	}
	return id, true
}
func uniqueUUIDs(ids []uuid.UUID) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	out := []uuid.UUID{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func joinNames(first, last *string) string {
	names := []string{}
	if first != nil && *first != "" {
		names = append(names, *first)
	}
	if last != nil && *last != "" {
		names = append(names, *last)
	}
	return strings.Join(names, " ")
}
func isExpanded(row repository.CommsMessage, id uuid.UUID) bool {
	return row.InteractionID != nil && *row.InteractionID == id
}
func sortContentMessages(rows []ContentMessage) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SentAt.Equal(rows[j].SentAt) {
			return bytes.Compare(rows[i].ID[:], rows[j].ID[:]) < 0
		}
		return rows[i].SentAt.Before(rows[j].SentAt)
	})
}
