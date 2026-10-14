package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// Timeline item kinds. An item is an interaction exactly when its
// effective participation has an interaction node, and a message burst exactly
// when it has none; an upcoming event and a missed call have identities of their
// own. A kind never lists sources: what a source is decides nothing here.
const (
	TimelineKindInteraction   = "interaction"
	TimelineKindMessageBurst  = "message_burst"
	TimelineKindUpcomingEvent = "upcoming_event"
	TimelineKindMissedCall    = "missed_call"
)

// ErrUnknownTimelineKind is returned by the content read for a kind that is not
// one of the timeline kinds.
var ErrUnknownTimelineKind = errors.New("unknown timeline item kind")

// TimelineIdentity is the typed identity of a timeline item: an interaction's is
// its merge-root node, a message burst's its observation, an upcoming event's
// its calendar event and a missed call's its call record.
type TimelineIdentity struct {
	Kind string
	ID   uuid.UUID
}

// TimelineMember is one member of a timeline item: the item's representative
// observation of one interaction node, or the item itself when it has no other
// member. It carries what the observation's row carries in the base list.
type TimelineMember struct {
	Source     string
	SourceRef  *string
	Enrichment InteractionEnrichment
}

// TimelineItem is one item of a contact's interaction history, at the contact's
// effective participation. A missed call bears no cadence, which CadenceInert
// states.
type TimelineItem struct {
	Identity   TimelineIdentity
	Direction  string
	OccurredAt time.Time
	Members    []TimelineMember
}

func (i TimelineItem) CadenceInert() bool { return i.Identity.Kind == TimelineKindMissedCall }

// TimelinePage is one page of a contact's interaction history and the number of
// items the filters keep.
type TimelinePage struct {
	Items []TimelineItem
	Total int
}

// timelineEntry is a timeline item before its members are enriched: the
// members of an interaction or message burst, or the call of a missed call.
type timelineEntry struct {
	identity   TimelineIdentity
	direction  string
	occurredAt time.Time
	members    []observationMember
	call       *repository.PhoneCall
}

// observationMember is one member of a timeline item: its representative
// observation and every observation whose content the member shows, the
// representative first. A contact merge can leave the context contact several
// observations of one interaction node, each holding the content its own contact
// was sent; the member shows all of them, so what it shows does not depend on
// which one represents it.
type observationMember struct {
	representative repository.Interaction
	observations   []repository.Interaction
}

// singleMember is the member of one observation alone.
func singleMember(row repository.Interaction) observationMember {
	return observationMember{representative: row, observations: []repository.Interaction{row}}
}

// ids returns the ids of the member's observations.
func (m observationMember) ids() []uuid.UUID {
	ids := make([]uuid.UUID, len(m.observations))
	for i, row := range m.observations {
		ids[i] = row.ID
	}
	return ids
}

// ListTimeline returns one page of a contact's interaction history: the
// contact's effective participations and its missed calls, latest first, each
// enriched as EnrichPage enriches an observation.
func (s *InteractionContentService) ListTimeline(ctx context.Context, p repository.InteractionListFilterParams) (*TimelinePage, error) {
	entries, err := s.timelineEntries(ctx, p)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].occurredAt.Equal(entries[j].occurredAt) {
			return entries[i].occurredAt.After(entries[j].occurredAt)
		}
		return bytes.Compare(entries[i].identity.ID[:], entries[j].identity.ID[:]) > 0
	})
	total := len(entries)
	start := min(int(p.Offset), total)
	entries = entries[start:min(start+int(p.Limit), total)]

	var members []observationMember
	for _, entry := range entries {
		members = append(members, entry.members...)
	}
	enrichment, err := s.enrichMembers(ctx, members)
	if err != nil {
		return nil, err
	}
	items := make([]TimelineItem, len(entries))
	for i, entry := range entries {
		item := TimelineItem{Identity: entry.identity, Direction: entry.direction, OccurredAt: entry.occurredAt}
		if entry.call != nil {
			item.Members = []TimelineMember{missedCallMember(*entry.call)}
		}
		for _, member := range entry.members {
			row := member.representative
			item.Members = append(item.Members, TimelineMember{Source: row.Source, SourceRef: row.SourceRef, Enrichment: enrichment[row.ID]})
		}
		items[i] = item
	}
	return &TimelinePage{Items: items, Total: total}, nil
}

func missedCallMember(call repository.PhoneCall) TimelineMember {
	return TimelineMember{
		Source: repository.InteractionSourcePhoneCalls,
		Enrichment: InteractionEnrichment{
			Label: "Missed call", ContentKind: contentKind(repository.InteractionSourcePhoneCalls, 0),
			VenueTags: []VenueTag{}, Call: callContent(call),
		},
	}
}

// timelineEntries returns every item the filters keep, unordered: the contact's
// effective participations in the time range, kept by the venue filter when one
// is given, and, when none is, the contact's missed calls in the range.
func (s *InteractionContentService) timelineEntries(ctx context.Context, p repository.InteractionListFilterParams) ([]timelineEntry, error) {
	participations, err := s.interactions.ListEffectiveParticipation(ctx, p.ContactID)
	if err != nil {
		return nil, err
	}
	var roots, bursts []uuid.UUID
	inRange := participations[:0]
	for _, ep := range participations {
		if (p.From != nil && ep.OccurredAt.Before(*p.From)) || (p.To != nil && !ep.OccurredAt.Before(*p.To)) {
			continue
		}
		inRange = append(inRange, ep)
		if ep.InteractionNodeID != nil {
			roots = append(roots, *ep.InteractionNodeID)
		} else {
			bursts = append(bursts, ep.Members[0].ObservationID)
		}
	}
	trees, err := s.interactions.ListInteractionTreeObservations(ctx, roots)
	if err != nil {
		return nil, err
	}
	burstRows, err := s.interactions.ListInteractionsByIDs(ctx, bursts)
	if err != nil {
		return nil, err
	}
	entries := make([]timelineEntry, 0, len(inRange))
	for _, ep := range inRange {
		entry := timelineEntry{direction: ep.Direction, occurredAt: ep.OccurredAt}
		if ep.InteractionNodeID != nil {
			entry.identity = TimelineIdentity{Kind: TimelineKindInteraction, ID: *ep.InteractionNodeID}
			entry.members = representatives(trees[*ep.InteractionNodeID], p.ContactID)
		} else {
			id := ep.Members[0].ObservationID
			row, ok := burstRows[id]
			if !ok {
				return nil, fmt.Errorf("message burst %s left the page between reads", id)
			}
			entry.identity = TimelineIdentity{Kind: TimelineKindMessageBurst, ID: id}
			entry.members = []observationMember{singleMember(row)}
		}
		entries = append(entries, entry)
	}

	if p.VenueSource != nil && p.VenueContainer != nil {
		return s.entriesInVenue(ctx, entries, *p.VenueSource, *p.VenueContainer)
	}
	calls, err := s.phoneCalls.ListMissedCalls(ctx, repository.MissedCallFilter{ContactID: &p.ContactID, From: p.From, To: p.To})
	if err != nil {
		return nil, err
	}
	for i := range calls {
		call := calls[i]
		entries = append(entries, timelineEntry{
			identity:  TimelineIdentity{Kind: TimelineKindMissedCall, ID: call.ID},
			direction: call.Direction, occurredAt: call.StartedAt, call: &call,
		})
	}
	return entries, nil
}

// entriesInVenue keeps the entries that the base venue filter would keep for
// the row of any of their members' observations.
func (s *InteractionContentService) entriesInVenue(ctx context.Context, entries []timelineEntry, source, container string) ([]timelineEntry, error) {
	var ids []uuid.UUID
	for _, entry := range entries {
		for _, member := range entry.members {
			ids = append(ids, member.ids()...)
		}
	}
	inVenue, err := s.interactions.ListObservationsInVenue(ctx, ids, source, container)
	if err != nil {
		return nil, err
	}
	kept := entries[:0]
	for _, entry := range entries {
		if slices.ContainsFunc(entry.members, func(member observationMember) bool {
			return slices.ContainsFunc(member.observations, func(row repository.Interaction) bool { return inVenue[row.ID] })
		}) {
			kept = append(kept, entry)
		}
	}
	return kept, nil
}

// representatives returns the members of a merged interaction, from the live
// observations, of any contact, on the nodes of its merge-alias tree: one per
// node, earliest representative first, then by node. A node's representative is
// the context contact's own latest observation of it, and the member shows every
// one of the contact's own observations of it; a node the contact has no
// observation of is represented and shown by its earliest observation.
func representatives(observations []repository.Interaction, contactID uuid.UUID) []observationMember {
	byNode := map[uuid.UUID][]repository.Interaction{}
	for _, row := range observations {
		byNode[*row.InteractionNodeID] = append(byNode[*row.InteractionNodeID], row)
	}
	members := make([]observationMember, 0, len(byNode))
	for _, rows := range byNode {
		own := slices.DeleteFunc(slices.Clone(rows), func(row repository.Interaction) bool { return row.ContactID != contactID })
		if len(own) == 0 {
			members = append(members, singleMember(slices.MinFunc(rows, compareObservations)))
			continue
		}
		// Latest first, so the representative leads its observations.
		slices.SortFunc(own, func(a, b repository.Interaction) int { return compareObservations(b, a) })
		members = append(members, observationMember{representative: own[0], observations: own})
	}
	slices.SortFunc(members, func(a, b observationMember) int {
		if c := a.representative.OccurredAt.Compare(b.representative.OccurredAt); c != 0 {
			return c
		}
		return bytes.Compare(a.representative.InteractionNodeID[:], b.representative.InteractionNodeID[:])
	})
	return members
}

// compareObservations orders observations by time, then id.
func compareObservations(a, b repository.Interaction) int {
	if c := a.OccurredAt.Compare(b.OccurredAt); c != 0 {
		return c
	}
	return bytes.Compare(a.ID[:], b.ID[:])
}

// TimelineContentMember is the content of one member of a timeline item.
type TimelineContentMember struct {
	Source  string
	Content InteractionContent
}

// TimelineContent is the content of a timeline item, member by member.
type TimelineContent struct {
	Identity TimelineIdentity
	Members  []TimelineContentMember
}

// GetTimelineContent returns the content of the timeline item the typed identity
// names, with contactID the participation context. An interaction is read by the
// id of its root or of any node merged into it; the members are those ListTimeline
// lists for that identity and contact. Returns ErrUnknownTimelineKind for an
// unknown kind and db.ErrNotFound when no item of the kind has the id or
// contactID names no live contact.
func (s *InteractionContentService) GetTimelineContent(ctx context.Context, kind string, id, contactID uuid.UUID) (*TimelineContent, error) {
	if !slices.Contains([]string{TimelineKindInteraction, TimelineKindMessageBurst, TimelineKindUpcomingEvent, TimelineKindMissedCall}, kind) {
		return nil, ErrUnknownTimelineKind
	}
	if _, err := s.contacts.GetContact(ctx, contactID); err != nil {
		return nil, err
	}
	switch kind {
	case TimelineKindInteraction:
		return s.interactionContent(ctx, id, contactID)
	case TimelineKindMessageBurst:
		return s.burstContent(ctx, id)
	case TimelineKindMissedCall:
		return s.missedCallContent(ctx, id)
	default:
		return s.upcomingEventContent(ctx, id)
	}
}

func (s *InteractionContentService) interactionContent(ctx context.Context, nodeID, contactID uuid.UUID) (*TimelineContent, error) {
	tree, err := s.interactions.GetInteractionTree(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	trees, err := s.interactions.ListInteractionTreeObservations(ctx, []uuid.UUID{tree.Root})
	if err != nil {
		return nil, err
	}
	members := representatives(trees[tree.Root], contactID)
	if len(members) == 0 {
		return nil, db.ErrNotFound
	}
	content := &TimelineContent{Identity: TimelineIdentity{Kind: TimelineKindInteraction, ID: tree.Root}}
	for _, member := range members {
		memberContent, err := s.observationMemberContent(ctx, member)
		if err != nil {
			return nil, err
		}
		content.Members = append(content.Members, memberContent)
	}
	return content, nil
}

func (s *InteractionContentService) burstContent(ctx context.Context, observationID uuid.UUID) (*TimelineContent, error) {
	row, err := s.interactions.GetInteraction(ctx, observationID)
	if err != nil {
		return nil, err
	}
	if row.InteractionNodeID != nil {
		return nil, db.ErrNotFound
	}
	member, err := s.observationMemberContent(ctx, singleMember(*row))
	if err != nil {
		return nil, err
	}
	return &TimelineContent{Identity: TimelineIdentity{Kind: TimelineKindMessageBurst, ID: row.ID}, Members: []TimelineContentMember{member}}, nil
}

func (s *InteractionContentService) observationMemberContent(ctx context.Context, member observationMember) (TimelineContentMember, error) {
	content, err := s.contentOf(ctx, member)
	if err != nil {
		return TimelineContentMember{}, err
	}
	return TimelineContentMember{Source: member.representative.Source, Content: *content}, nil
}

func (s *InteractionContentService) missedCallContent(ctx context.Context, callID uuid.UUID) (*TimelineContent, error) {
	calls, err := s.phoneCalls.ListMissedCalls(ctx, repository.MissedCallFilter{CallID: &callID})
	if err != nil {
		return nil, err
	}
	if len(calls) == 0 {
		return nil, db.ErrNotFound
	}
	member := TimelineContentMember{Source: repository.InteractionSourcePhoneCalls, Content: InteractionContent{Kind: "call", Messages: []ContentMessage{}, MeetingNotes: []MeetingNoteContent{}}}
	return &TimelineContent{Identity: TimelineIdentity{Kind: TimelineKindMissedCall, ID: callID}, Members: []TimelineContentMember{member}}, nil
}

func (s *InteractionContentService) upcomingEventContent(ctx context.Context, eventID uuid.UUID) (*TimelineContent, error) {
	if _, err := s.calendarEvents.GetByID(ctx, eventID); err != nil {
		return nil, err
	}
	notes, err := s.eventNotes(ctx, eventID)
	if err != nil {
		return nil, err
	}
	content := InteractionContent{Kind: "none", Messages: []ContentMessage{}, MeetingNotes: notes}
	if len(notes) > 0 {
		content.Kind = "meeting_note"
	}
	member := TimelineContentMember{Source: repository.InteractionSourceGCal, Content: content}
	return &TimelineContent{Identity: TimelineIdentity{Kind: TimelineKindUpcomingEvent, ID: eventID}, Members: []TimelineContentMember{member}}, nil
}
