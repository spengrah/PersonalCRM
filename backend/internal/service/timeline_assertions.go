package service

import (
	"context"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// TimelineAssertionService reads the assertions beside a timeline item's
// content: those whose provenance cites a content item the item's drill-down
// displays. It is the one HTTP-facing read of the assertion store (KNW-038).
type TimelineAssertionService struct {
	content    *InteractionContentService
	assertions *repository.AssertionRepository
}

func NewTimelineAssertionService(content *InteractionContentService, assertions *repository.AssertionRepository) *TimelineAssertionService {
	return &TimelineAssertionService{content: content, assertions: assertions}
}

// ListTimelineAssertions returns the non-correction assertions whose provenance
// cites a content item GetTimelineContent displays for the typed identity and
// contactID, one per (content item, assertion), keyed by the item's content
// ref. It returns GetTimelineContent's errors for an unknown kind or item.
func (s *TimelineAssertionService) ListTimelineAssertions(ctx context.Context, kind string, id, contactID uuid.UUID) ([]repository.CitedAssertion, error) {
	content, err := s.content.GetTimelineContent(ctx, kind, id, contactID)
	if err != nil {
		return nil, err
	}
	var displayed []repository.ContentCitation
	for _, member := range content.Members {
		for _, message := range member.Content.Messages {
			displayed = append(displayed, repository.ContentCitation{SourceKind: message.RowKind, RowID: message.ID, ContentRef: message.ContentRef})
		}
		for _, note := range member.Content.MeetingNotes {
			displayed = append(displayed, repository.ContentCitation{SourceKind: repository.SourceKindMeetingNote, RowID: note.ID, ContentRef: note.ContentRef})
		}
	}
	return s.assertions.ListAssertionsCitingContent(ctx, displayed)
}
