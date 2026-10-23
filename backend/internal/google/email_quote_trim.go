package google

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// quoteTrimBatchSize bounds each keyset page of the one-time quote trim.
const quoteTrimBatchSize = 500

// EmailQuoteTrimResult is the counts-only summary crm-admin --trim-email-quotes
// prints.
type EmailQuoteTrimResult struct {
	Scanned int
	Trimmed int
}

// quoteTrimCommsRepo is the comms_message surface the quote trim needs.
// Concrete: *repository.CommsMessageRepository.
type quoteTrimCommsRepo interface {
	ListEmailBodies(ctx context.Context, afterID uuid.UUID, batchSize int32) ([]repository.EmailBodyRow, error)
	UpdateText(ctx context.Context, id uuid.UUID, body, quotedHistory *string) (int64, error)
}

// EmailQuoteTrimService applies the ingest's text rule (storedText) once to
// email bodies stored before ingest applied it: a thread start's quoted mail
// moves to its quoted history, a forward's content is capped and a reply's
// quotes are dropped. It reads only what the row stores, so it makes no Gmail
// calls. It writes only a row whose body the rule changes, so a second run
// changes nothing.
type EmailQuoteTrimService struct {
	comms quoteTrimCommsRepo
}

// NewEmailQuoteTrimService builds the service.
func NewEmailQuoteTrimService(comms quoteTrimCommsRepo) *EmailQuoteTrimService {
	return &EmailQuoteTrimService{comms: comms}
}

// TrimStoredBodies pages every live email row by id and rewrites the body and
// quoted history of each row whose body the rule changes.
func (s *EmailQuoteTrimService) TrimStoredBodies(ctx context.Context) (EmailQuoteTrimResult, error) {
	var result EmailQuoteTrimResult
	afterID := uuid.Nil
	for {
		rows, err := s.comms.ListEmailBodies(ctx, afterID, quoteTrimBatchSize)
		if err != nil {
			return result, fmt.Errorf("list email bodies: %w", err)
		}
		if len(rows) == 0 {
			return result, nil
		}
		for _, row := range rows {
			result.Scanned++
			text := storedRowText(row)
			if text.body == row.Body {
				continue
			}
			if _, err := s.comms.UpdateText(ctx, row.ID, strPtrIfNotEmpty(text.body), strPtrIfNotEmpty(text.quoted)); err != nil {
				return result, fmt.Errorf("update text: %w", err)
			}
			result.Trimmed++
		}
		afterID = rows[len(rows)-1].ID
	}
}

// storedRowText re-derives a stored row's text with storedText. The row keeps
// no separate plain part: a body equal to its stripped HTML came from the HTML
// part, and any other body is the plain part.
func storedRowText(row repository.EmailBodyRow) emailText {
	plain := row.Body
	if row.HTML != "" && stripHTML(row.HTML) == row.Body {
		plain = ""
	}
	return storedText(plain, row.HTML, mailKindOf(row.GmailMessageID, row.ThreadID, row.Subject))
}
