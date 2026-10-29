package service

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// InteractionTypeOption is an interaction type and the name the view gives it.
type InteractionTypeOption struct {
	Key   InteractionType
	Label string
}

// InteractionTypes are the interaction types in the order the view offers them.
var InteractionTypes = []InteractionTypeOption{
	{InteractionTypeMessage, "Messages"},
	{InteractionTypeEmail, "Email"},
	{InteractionTypeEvent, "Events"},
	{InteractionTypeCall, "Calls"},
	{InteractionTypeOther, "Other"},
}

// InteractionTypeOf is the type of an observation of the source: the one
// source-to-type mapping the Interactions tab's and the Threads tab's type
// filters read.
func InteractionTypeOf(source string) InteractionType {
	switch source {
	case repository.InteractionSourceTelegram, repository.InteractionSourceMessages,
		repository.InteractionSourceGChat, repository.InteractionSourceWhatsApp:
		return InteractionTypeMessage
	case repository.InteractionSourceEmail:
		return InteractionTypeEmail
	case repository.InteractionSourceGCal, repository.InteractionSourceAnarlogSessions:
		return InteractionTypeEvent
	case repository.InteractionSourcePhoneCalls:
		return InteractionTypeCall
	default:
		return InteractionTypeOther
	}
}

// RowParticipant is one participant contact of a row.
type RowParticipant struct {
	ContactID uuid.UUID
	Name      string
}

// RowPreview is the latest content a row shows: a message's sender and text,
// or a meeting note's text, which has no sender.
type RowPreview struct {
	Sender string
	Text   string
	Note   bool
}

// InteractionRow is one row of the all-interactions view: one interaction
// across contacts. An interaction several contacts take part in is one row,
// and so are the copies of a group chat's messages, which the store holds once
// per member. The row reads its content in the participation context of its
// first participant, whose participation gives it its identity, direction and
// time.
type InteractionRow struct {
	TimelineItem
	// Participants are the row's participant contacts, ordered by name.
	Participants []RowParticipant
	// Types are the interaction types of the row's members.
	Types []InteractionType
	// Threads are the venues the row's observations were recorded in.
	Threads []uuid.UUID
	// Observations are the row's observations, of every participant.
	Observations []uuid.UUID
	// Preview is nil for a row that shows no content.
	Preview *RowPreview
}

// InteractionsFilter selects rows of the all-interactions view. A nil field
// keeps every row; Rows keeps the rows whose identity it holds, so an empty
// non-nil set keeps none.
type InteractionsFilter struct {
	ContactID *uuid.UUID
	Type      *InteractionType
	Rows      map[TimelineIdentity]struct{}
	Limit     int32
	Offset    int32
}

// InteractionsPage is one page of rows and the number of rows the filter
// keeps.
type InteractionsPage struct {
	Rows  []InteractionRow
	Total int
}

// InteractionIndex is every row of the all-interactions view, before any
// filter or page. RowOf maps every observation of every row to its row's
// identity.
type InteractionIndex struct {
	candidates []candidateRow
	RowOf      map[uuid.UUID]TimelineIdentity
}

// candidateRow is a row before its page is read, with its first
// participant's participation, or the missed call it is.
type candidateRow struct {
	row  InteractionRow
	lead repository.EffectiveParticipation
	call *repository.PhoneCall
}

// IndexInteractions returns every interaction across contacts as rows: each
// contact's effective participations, merged into one row per interaction,
// and every missed call of a contact.
func (s *InteractionContentService) IndexInteractions(ctx context.Context) (*InteractionIndex, error) {
	candidates, err := s.candidateRows(ctx)
	if err != nil {
		return nil, err
	}
	rowOf := map[uuid.UUID]TimelineIdentity{}
	for _, c := range candidates {
		for _, id := range c.row.Observations {
			rowOf[id] = c.row.Identity
		}
	}
	return &InteractionIndex{candidates: candidates, RowOf: rowOf}, nil
}

// ListInteractions returns one page of the index's rows the filter keeps,
// newest first with the contact history's tie-breaker. Each row on the page is
// enriched as the contact history enriches it, in its context contact's
// participation.
func (s *InteractionContentService) ListInteractions(ctx context.Context, idx *InteractionIndex, f InteractionsFilter) (*InteractionsPage, error) {
	var kept []candidateRow
	for _, c := range idx.candidates {
		if keepRow(c.row, f) {
			kept = append(kept, c)
		}
	}
	slices.SortFunc(kept, func(a, b candidateRow) int {
		if c := b.row.OccurredAt.Compare(a.row.OccurredAt); c != 0 {
			return c
		}
		return bytes.Compare(b.row.Identity.ID[:], a.row.Identity.ID[:])
	})
	total := len(kept)
	start := min(int(f.Offset), total)
	page := kept[start:min(start+int(f.Limit), total)]

	rows, err := s.readPage(ctx, page)
	if err != nil {
		return nil, err
	}
	return &InteractionsPage{Rows: rows, Total: total}, nil
}

func keepRow(row InteractionRow, f InteractionsFilter) bool {
	if f.ContactID != nil && !slices.ContainsFunc(row.Participants, func(p RowParticipant) bool { return p.ContactID == *f.ContactID }) {
		return false
	}
	if f.Type != nil && !slices.Contains(row.Types, *f.Type) {
		return false
	}
	if f.Rows != nil {
		if _, ok := f.Rows[row.Identity]; !ok {
			return false
		}
	}
	return true
}

// candidateRows groups every live contact's effective participations into
// rows: the participations of one merged interaction are one row, and so are
// message bursts holding copies of the same group message. Every missed call
// of a live contact is a row of its own. The four whole-history reads are
// independent, so they run concurrently on the pool.
func (s *InteractionContentService) candidateRows(ctx context.Context) ([]candidateRow, error) {
	var (
		participations                               []repository.EffectiveParticipation
		calls                                        []repository.PhoneCall
		venues                                       map[uuid.UUID]uuid.UUID
		copies                                       []repository.MessageCopy
		participationErr, callErr, venueErr, copyErr error
		wg                                           sync.WaitGroup
	)
	wg.Go(func() { calls, callErr = s.phoneCalls.ListMissedCalls(ctx, repository.MissedCallFilter{}) })
	wg.Go(func() { venues, venueErr = s.interactions.ListLiveObservationVenues(ctx) })
	wg.Go(func() { copies, copyErr = s.interactions.ListGroupChatMessageCopies(ctx) })
	participations, participationErr = s.interactions.ListAllEffectiveParticipation(ctx)
	wg.Wait()
	if err := cmp.Or(participationErr, callErr, venueErr, copyErr); err != nil {
		return nil, err
	}
	contactIDs := make([]uuid.UUID, 0, len(participations)+len(calls))
	for _, ep := range participations {
		contactIDs = append(contactIDs, ep.ContactID)
	}
	for _, call := range calls {
		if call.MatchedContactID != nil {
			contactIDs = append(contactIDs, *call.MatchedContactID)
		}
	}
	names, err := s.contacts.ListContactNamesByIDs(ctx, uniqueUUIDs(contactIDs))
	if err != nil {
		return nil, err
	}
	groups := groupParticipations(participations, copies)

	candidates := make([]candidateRow, 0, len(groups)+len(calls))
	for _, group := range groups {
		var row InteractionRow
		for _, ep := range group {
			row.Participants = append(row.Participants, RowParticipant{ContactID: ep.ContactID, Name: names[ep.ContactID]})
			for _, member := range ep.Members {
				row.Observations = append(row.Observations, member.ObservationID)
				row.Types = append(row.Types, InteractionTypeOf(member.Source))
				if venue, ok := venues[member.ObservationID]; ok {
					row.Threads = append(row.Threads, venue)
				}
			}
		}
		slices.SortFunc(row.Participants, compareParticipants)
		slices.Sort(row.Types)
		row.Types = slices.Compact(row.Types)
		row.Threads = uniqueUUIDs(row.Threads)
		lead := group[slices.IndexFunc(group, func(ep repository.EffectiveParticipation) bool {
			return ep.ContactID == row.Participants[0].ContactID
		})]
		row.Direction, row.OccurredAt = lead.Direction, lead.OccurredAt
		if lead.InteractionNodeID != nil {
			row.Identity = TimelineIdentity{Kind: TimelineKindInteraction, ID: *lead.InteractionNodeID}
		} else {
			row.Identity = TimelineIdentity{Kind: TimelineKindMessageBurst, ID: lead.Members[0].ObservationID}
		}
		candidates = append(candidates, candidateRow{row: row, lead: lead})
	}
	for i := range calls {
		call := calls[i]
		if call.MatchedContactID == nil {
			continue
		}
		name, live := names[*call.MatchedContactID]
		if !live {
			continue
		}
		candidates = append(candidates, candidateRow{
			row: InteractionRow{
				TimelineItem: TimelineItem{
					Identity:  TimelineIdentity{Kind: TimelineKindMissedCall, ID: call.ID},
					Direction: call.Direction, OccurredAt: call.StartedAt,
				},
				Participants: []RowParticipant{{ContactID: *call.MatchedContactID, Name: name}},
				Types:        []InteractionType{InteractionTypeCall},
			},
			call: &call,
		})
	}
	return candidates, nil
}

func compareParticipants(a, b RowParticipant) int {
	return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), bytes.Compare(a.ContactID[:], b.ContactID[:]))
}

// groupParticipations groups participations into rows: by root interaction
// node, and for message bursts by the group messages their observations hold
// copies of.
func groupParticipations(participations []repository.EffectiveParticipation, copies []repository.MessageCopy) [][]repository.EffectiveParticipation {
	byRoot := map[uuid.UUID][]repository.EffectiveParticipation{}
	var roots []uuid.UUID
	bursts := map[uuid.UUID]repository.EffectiveParticipation{}
	parent := map[uuid.UUID]uuid.UUID{}
	for _, ep := range participations {
		if ep.InteractionNodeID != nil {
			if _, ok := byRoot[*ep.InteractionNodeID]; !ok {
				roots = append(roots, *ep.InteractionNodeID)
			}
			byRoot[*ep.InteractionNodeID] = append(byRoot[*ep.InteractionNodeID], ep)
			continue
		}
		id := ep.Members[0].ObservationID
		bursts[id], parent[id] = ep, id
	}

	// Bursts holding copies of one message are one row (union-find over the
	// group messages each burst holds).
	find := func(id uuid.UUID) uuid.UUID {
		for parent[id] != id {
			parent[id] = parent[parent[id]]
			id = parent[id]
		}
		return id
	}
	holder := map[string]uuid.UUID{}
	for _, c := range copies {
		if _, ok := bursts[c.ObservationID]; !ok {
			continue
		}
		first, seen := holder[c.MessageKey]
		if !seen {
			holder[c.MessageKey] = c.ObservationID
			continue
		}
		a, b := find(first), find(c.ObservationID)
		if a != b {
			if bytes.Compare(a[:], b[:]) > 0 {
				a, b = b, a
			}
			parent[b] = a
		}
	}

	groups := make([][]repository.EffectiveParticipation, 0, len(roots)+len(bursts))
	for _, root := range roots {
		groups = append(groups, byRoot[root])
	}
	byGroup := map[uuid.UUID][]repository.EffectiveParticipation{}
	var groupRoots []uuid.UUID
	for id, ep := range bursts {
		root := find(id)
		if _, ok := byGroup[root]; !ok {
			groupRoots = append(groupRoots, root)
		}
		byGroup[root] = append(byGroup[root], ep)
	}
	slices.SortFunc(groupRoots, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	for _, root := range groupRoots {
		groups = append(groups, byGroup[root])
	}
	return groups
}

// readPage reads the members, enrichment and preview of each row on a page,
// in its context contact's participation, as ListTimeline reads them.
func (s *InteractionContentService) readPage(ctx context.Context, page []candidateRow) ([]InteractionRow, error) {
	var roots, bursts []uuid.UUID
	for _, c := range page {
		switch c.row.Identity.Kind {
		case TimelineKindInteraction:
			roots = append(roots, c.row.Identity.ID)
		case TimelineKindMessageBurst:
			bursts = append(bursts, c.row.Identity.ID)
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
	members := make([][]observationMember, len(page))
	var all []observationMember
	for i, c := range page {
		switch c.row.Identity.Kind {
		case TimelineKindInteraction:
			members[i] = representatives(trees[c.row.Identity.ID], c.lead.ContactID)
		case TimelineKindMessageBurst:
			row, ok := burstRows[c.row.Identity.ID]
			if !ok {
				return nil, fmt.Errorf("message burst %s left the page between reads", c.row.Identity.ID)
			}
			members[i] = []observationMember{singleMember(row)}
		}
		all = append(all, members[i]...)
	}
	enrichment, err := s.enrichMembers(ctx, all)
	if err != nil {
		return nil, err
	}
	// Each row's preview is a few round trips of its own, so the rows read
	// theirs concurrently, at most previewReaders at a time to leave the pool
	// to other work.
	previews := make([]*RowPreview, len(page))
	previewErrs := make([]error, len(page))
	slots := make(chan struct{}, previewReaders)
	var wg sync.WaitGroup
	for i := range page {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			previews[i], previewErrs[i] = s.preview(ctx, members[i])
		})
	}
	wg.Wait()
	if err := cmp.Or(previewErrs...); err != nil {
		return nil, err
	}
	rows := make([]InteractionRow, len(page))
	for i, c := range page {
		row := c.row
		if c.call != nil {
			row.Members = []TimelineMember{missedCallMember(*c.call)}
		}
		for _, member := range members[i] {
			r := member.representative
			row.Members = append(row.Members, TimelineMember{Source: r.Source, SourceRef: r.SourceRef, Enrichment: enrichment[r.ID]})
		}
		row.Preview = previews[i]
		rows[i] = row
	}
	return rows, nil
}

// previewReaders is how many of a page's rows read their previews at once.
const previewReaders = 4

// preview is the latest message the members show, else their first meeting
// note's text; nil when they show neither.
func (s *InteractionContentService) preview(ctx context.Context, members []observationMember) (*RowPreview, error) {
	var latest *ContentMessage
	var note *MeetingNoteContent
	for _, member := range members {
		kind := contentKind(member.representative.Source, 1)
		if kind != "messages" && kind != "meeting_note" {
			continue
		}
		content, err := s.contentOf(ctx, member)
		if err != nil {
			return nil, err
		}
		for i := range content.Messages {
			m := &content.Messages[i]
			if latest == nil || !m.SentAt.Before(latest.SentAt) {
				latest = m
			}
		}
		if note == nil && len(content.MeetingNotes) > 0 {
			note = &content.MeetingNotes[0]
		}
	}
	switch {
	case latest != nil:
		return &RowPreview{Sender: latest.Sender, Text: latest.Body}, nil
	case note != nil:
		text := ""
		for _, part := range []*string{note.Summary, note.Memo, note.Title} {
			if part != nil && *part != "" {
				text = *part
				break
			}
		}
		return &RowPreview{Text: text, Note: true}, nil
	default:
		return nil, nil
	}
}
