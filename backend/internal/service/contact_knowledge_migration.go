package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ContactKnowledgeMigrationConfidence is the confidence stamped on a migrated
// location/birthday/how_met assertion. These are existing user-entered profile
// values, so they land at full confidence.
const ContactKnowledgeMigrationConfidence = 100

// ContactKnowledgeMigrationResult is the counts-only summary of a
// --migrate-contact-knowledge-columns run.
type ContactKnowledgeMigrationResult struct {
	Contacts          int // non-deleted contacts with at least one knowledge column set
	LocationsMigrated int // lives_in edges asserted
	BirthdaysMigrated int // birthday facts asserted
	HowMetMigrated    int // how_met facts asserted
}

// ContactKnowledgeMigrationService mirrors the legacy contact.location /
// contact.birthday / contact.how_met cache columns into the graph: location
// becomes a lives_in edge to a place entity node, birthday/how_met become facts.
// Every assertion routes through AssertService (so proposition identity + dedup +
// event emission apply) with the contact's created_at as the knowledge time and a
// deterministic per-(contact, column) locator, which makes a re-run idempotent
// (corroborates rather than duplicates).
//
// A soft-deleted contact is skipped permanently: the source query filters
// deleted_at IS NULL, and the write API rejects a deleted subject node anyway.
type ContactKnowledgeMigrationService struct {
	pool        *pgxpool.Pool
	contactRepo *repository.ContactRepository
	assertSvc   *AssertService
}

// NewContactKnowledgeMigrationService wires the migration over the contact reads
// + the validated assertion write path.
func NewContactKnowledgeMigrationService(
	pool *pgxpool.Pool,
	contactRepo *repository.ContactRepository,
	assertSvc *AssertService,
) *ContactKnowledgeMigrationService {
	return &ContactKnowledgeMigrationService{
		pool:        pool,
		contactRepo: contactRepo,
		assertSvc:   assertSvc,
	}
}

// MigrateContactKnowledgeColumns runs the full location/birthday/how_met →
// assertion-store backfill. Idempotent: each assertion has a deterministic
// locator, so a re-run corroborates rather than duplicates.
func (s *ContactKnowledgeMigrationService) MigrateContactKnowledgeColumns(ctx context.Context) (ContactKnowledgeMigrationResult, error) {
	var result ContactKnowledgeMigrationResult

	rows, err := s.contactRepo.ListContactsWithKnowledgeColumns(ctx)
	if err != nil {
		return result, fmt.Errorf("list contacts with knowledge columns: %w", err)
	}
	result.Contacts = len(rows)

	for _, row := range rows {
		createdAt := row.CreatedAt
		if row.Location != nil && strings.TrimSpace(*row.Location) != "" {
			if err := s.migrateLocation(ctx, row.ContactID, *row.Location, createdAt); err != nil {
				return result, fmt.Errorf("migrate location (contact %s): %w", row.ContactID, err)
			}
			result.LocationsMigrated++
		}
		if row.Birthday != nil {
			if err := s.migrateBirthday(ctx, row.ContactID, *row.Birthday, createdAt); err != nil {
				return result, fmt.Errorf("migrate birthday (contact %s): %w", row.ContactID, err)
			}
			result.BirthdaysMigrated++
		}
		if row.HowMet != nil && strings.TrimSpace(*row.HowMet) != "" {
			if err := s.migrateHowMet(ctx, row.ContactID, *row.HowMet, createdAt); err != nil {
				return result, fmt.Errorf("migrate how_met (contact %s): %w", row.ContactID, err)
			}
			result.HowMetMigrated++
		}
	}

	return result, nil
}

// migrateLocation ensures the place entity node (own tx, idempotent), then
// asserts the lives_in edge with the contact's created_at as the knowledge time.
func (s *ContactKnowledgeMigrationService) migrateLocation(ctx context.Context, contactID uuid.UUID, label string, createdAt time.Time) error {
	placeID, err := s.ensurePlaceNode(ctx, label)
	if err != nil {
		return fmt.Errorf("ensure place node: %w", err)
	}
	knowledgeFrom := createdAt
	req := AssertRequest{
		SubjectNodeID:         contactID,
		PredicateKey:          repository.PredicateLivesIn,
		ObjectNodeID:          &placeID,
		Confidence:            ContactKnowledgeMigrationConfidence,
		KnowledgeFromOverride: &knowledgeFrom,
		Locators:              []ProvenanceLocator{s.locator(contactID, "location")},
	}
	_, err = s.assertSvc.Assert(ctx, req)
	return err
}

func (s *ContactKnowledgeMigrationService) migrateBirthday(ctx context.Context, contactID uuid.UUID, birthday time.Time, createdAt time.Time) error {
	knowledgeFrom := createdAt
	value := birthday
	req := AssertRequest{
		SubjectNodeID:         contactID,
		PredicateKey:          repository.PredicateBirthday,
		ValueDate:             &value,
		Confidence:            ContactKnowledgeMigrationConfidence,
		KnowledgeFromOverride: &knowledgeFrom,
		Locators:              []ProvenanceLocator{s.locator(contactID, "birthday")},
	}
	_, err := s.assertSvc.Assert(ctx, req)
	return err
}

func (s *ContactKnowledgeMigrationService) migrateHowMet(ctx context.Context, contactID uuid.UUID, howMet string, createdAt time.Time) error {
	knowledgeFrom := createdAt
	value := howMet
	req := AssertRequest{
		SubjectNodeID:         contactID,
		PredicateKey:          repository.PredicateHowMet,
		ValueText:             &value,
		Confidence:            ContactKnowledgeMigrationConfidence,
		KnowledgeFromOverride: &knowledgeFrom,
		Locators:              []ProvenanceLocator{s.locator(contactID, "how_met")},
	}
	_, err := s.assertSvc.Assert(ctx, req)
	return err
}

// ensurePlaceNode find-or-creates the place entity node for a location label in
// its own tx (find-or-create by (subtype='place', normalized_name)).
func (s *ContactKnowledgeMigrationService) ensurePlaceNode(ctx context.Context, label string) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	placeID, err := s.assertSvc.EnsurePlaceTx(ctx, tx, label)
	if err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit place node: %w", err)
	}
	return placeID, nil
}

// locator builds the deterministic user-provenance locator for a migrated field
// (source_id = "contact_col:<contactID>:<column>"), so a re-run hashes to the
// same locator and the provenance ON CONFLICT no-ops.
func (s *ContactKnowledgeMigrationService) locator(contactID uuid.UUID, column string) ProvenanceLocator {
	return ProvenanceLocator{
		SourceKind:   repository.SourceKindUser,
		SourceID:     fmt.Sprintf("contact_col:%s:%s", contactID, column),
		ProducerKind: repository.ProducerKindUser,
	}
}
