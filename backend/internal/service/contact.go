package service

import (
	"context"
	"errors"
	"sort"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/reminder"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ContactMethodInput struct {
	Type      string
	Value     string
	IsPrimary bool
}

type OverdueContact struct {
	Contact         repository.Contact
	DaysOverdue     int
	NextDueDate     time.Time
	SuggestedAction string
}

type ContactService struct {
	database          *db.Database
	contactRepo       *repository.ContactRepository
	contactMethodRepo *repository.ContactMethodRepository
	reminderRepo      *repository.ReminderRepository
}

func NewContactService(database *db.Database, contactRepo *repository.ContactRepository, contactMethodRepo *repository.ContactMethodRepository, reminderRepo *repository.ReminderRepository) *ContactService {
	return &ContactService{
		database:          database,
		contactRepo:       contactRepo,
		contactMethodRepo: contactMethodRepo,
		reminderRepo:      reminderRepo,
	}
}

func (s *ContactService) GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error) {
	contact, err := s.contactRepo.GetContact(ctx, id)
	if err != nil {
		return nil, err
	}

	if err := s.attachMethods(ctx, contact); err != nil {
		return nil, err
	}

	return contact, nil
}

// Deprecated: Use ListContactsPage when pagination metadata is needed.
func (s *ContactService) ListContacts(ctx context.Context, params repository.ListContactsParams) ([]repository.Contact, error) {
	contacts, err := s.contactRepo.ListContacts(ctx, params)
	if err != nil {
		return nil, err
	}

	if err := s.attachMethodsToContacts(ctx, contacts); err != nil {
		return nil, err
	}

	return contacts, nil
}

func (s *ContactService) ListContactsPage(ctx context.Context, params repository.ListContactsParams) ([]repository.Contact, int64, error) {
	contacts, err := s.contactRepo.ListContacts(ctx, params)
	if err != nil {
		return nil, 0, err
	}

	if err := s.attachMethodsToContacts(ctx, contacts); err != nil {
		return nil, 0, err
	}

	total, err := s.contactRepo.CountContacts(ctx)
	if err != nil {
		return nil, 0, err
	}

	return contacts, total, nil
}

// Deprecated: Use SearchContactsPage when pagination metadata is needed.
func (s *ContactService) SearchContacts(ctx context.Context, params repository.SearchContactsParams) ([]repository.Contact, error) {
	contacts, err := s.contactRepo.SearchContacts(ctx, params)
	if err != nil {
		return nil, err
	}

	if err := s.attachMethodsToContacts(ctx, contacts); err != nil {
		return nil, err
	}

	return contacts, nil
}

func (s *ContactService) SearchContactsPage(ctx context.Context, params repository.SearchContactsParams) ([]repository.Contact, int64, error) {
	contacts, err := s.contactRepo.SearchContacts(ctx, params)
	if err != nil {
		return nil, 0, err
	}

	if err := s.attachMethodsToContacts(ctx, contacts); err != nil {
		return nil, 0, err
	}

	total, err := s.contactRepo.CountSearchContacts(ctx, params.Query)
	if err != nil {
		return nil, 0, err
	}

	return contacts, total, nil
}

func (s *ContactService) CreateContact(ctx context.Context, req repository.CreateContactRequest, methods []ContactMethodInput) (contact *repository.Contact, err error) {
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			if err == nil {
				err = rollbackErr
			}
		}
	}()

	txQueries := db.New(tx)
	contactRepo := repository.NewContactRepository(txQueries)
	contactMethodRepo := repository.NewContactMethodRepository(txQueries)

	contact, err = contactRepo.CreateContact(ctx, req)
	if err != nil {
		return nil, err
	}

	createdMethods, err := createContactMethods(ctx, contactMethodRepo, contact.ID, methods)
	if err != nil {
		return nil, err
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}

	assignMethods(contact, createdMethods)
	return contact, nil
}

func (s *ContactService) UpdateContact(ctx context.Context, id uuid.UUID, req repository.UpdateContactRequest, methods []ContactMethodInput, replaceMethods bool) (contact *repository.Contact, err error) {
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			if err == nil {
				err = rollbackErr
			}
		}
	}()

	txQueries := db.New(tx)
	contactRepo := repository.NewContactRepository(txQueries)
	contactMethodRepo := repository.NewContactMethodRepository(txQueries)

	_, err = contactRepo.GetContact(ctx, id)
	if err != nil {
		return nil, err
	}

	contact, err = contactRepo.UpdateContact(ctx, id, req)
	if err != nil {
		return nil, err
	}

	var updatedMethods []repository.ContactMethod
	if replaceMethods {
		if err := contactMethodRepo.DeleteContactMethodsByContact(ctx, id); err != nil {
			return nil, err
		}

		updatedMethods, err = createContactMethods(ctx, contactMethodRepo, id, methods)
		if err != nil {
			return nil, err
		}
	} else {
		updatedMethods, err = contactMethodRepo.ListContactMethodsByContact(ctx, id)
		if err != nil {
			return nil, err
		}
		sortContactMethods(updatedMethods)
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}

	assignMethods(contact, updatedMethods)
	return contact, nil
}

func (s *ContactService) DeleteContact(ctx context.Context, id uuid.UUID) error {
	_, err := s.contactRepo.GetContact(ctx, id)
	if err != nil {
		return err
	}

	// Soft-delete all reminders for this contact before deleting the contact
	if err := s.reminderRepo.SoftDeleteRemindersForContact(ctx, id); err != nil {
		return err
	}

	return s.contactRepo.SoftDeleteContact(ctx, id)
}

func (s *ContactService) UpdateContactLastContacted(ctx context.Context, id uuid.UUID) (*repository.Contact, error) {
	_, err := s.contactRepo.GetContact(ctx, id)
	if err != nil {
		return nil, err
	}

	if err := s.contactRepo.UpdateContactLastContacted(ctx, id, accelerated.GetCurrentTime()); err != nil {
		return nil, err
	}

	// Complete auto-generated reminders for this contact when marked as contacted
	// Manual reminders are preserved since they may be unrelated to the contact cadence
	if err := s.reminderRepo.CompleteAutoRemindersForContact(ctx, id); err != nil {
		return nil, err
	}

	contact, err := s.contactRepo.GetContact(ctx, id)
	if err != nil {
		return nil, err
	}

	if err := s.attachMethods(ctx, contact); err != nil {
		return nil, err
	}

	return contact, nil
}

func (s *ContactService) ListOverdueContacts(ctx context.Context) ([]OverdueContact, error) {
	contacts, err := s.ListContacts(ctx, repository.ListContactsParams{
		Limit:  1000,
		Offset: 0,
	})
	if err != nil {
		return nil, err
	}

	now := accelerated.GetCurrentTime()
	var overdueContacts []OverdueContact

	for _, contact := range contacts {
		if contact.Cadence == nil || *contact.Cadence == "" {
			continue
		}

		cadence, err := reminder.ParseCadence(*contact.Cadence)
		if err != nil {
			continue
		}

		if reminder.IsOverdueWithConfig(cadence, contact.LastContacted, contact.CreatedAt, now) {
			daysOverdue := reminder.GetOverdueDaysWithConfig(cadence, contact.LastContacted, contact.CreatedAt, now)
			nextDue := reminder.CalculateNextDueDateWithConfig(cadence, contact.LastContacted, contact.CreatedAt)

			suggestedAction := suggestedActionForOverdueDays(daysOverdue)

			overdueContacts = append(overdueContacts, OverdueContact{
				Contact:         contact,
				DaysOverdue:     daysOverdue,
				NextDueDate:     nextDue,
				SuggestedAction: suggestedAction,
			})
		}
	}

	sort.Slice(overdueContacts, func(i, j int) bool {
		return overdueContacts[i].DaysOverdue > overdueContacts[j].DaysOverdue
	})

	return overdueContacts, nil
}

func (s *ContactService) attachMethods(ctx context.Context, contact *repository.Contact) error {
	methods, err := s.contactMethodRepo.ListContactMethodsByContact(ctx, contact.ID)
	if err != nil {
		return err
	}

	sortContactMethods(methods)
	assignMethods(contact, methods)
	return nil
}

func (s *ContactService) attachMethodsToContacts(ctx context.Context, contacts []repository.Contact) error {
	for i := range contacts {
		if err := s.attachMethods(ctx, &contacts[i]); err != nil {
			return err
		}
	}
	return nil
}

func createContactMethods(ctx context.Context, repo *repository.ContactMethodRepository, contactID uuid.UUID, methods []ContactMethodInput) ([]repository.ContactMethod, error) {
	created := make([]repository.ContactMethod, 0, len(methods))

	for _, method := range methods {
		createdMethod, err := repo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: contactID,
			Type:      method.Type,
			Value:     method.Value,
			IsPrimary: method.IsPrimary,
		})
		if err != nil {
			return nil, err
		}
		created = append(created, *createdMethod)
	}

	sortContactMethods(created)
	return created, nil
}

func assignMethods(contact *repository.Contact, methods []repository.ContactMethod) {
	contact.Methods = methods
	contact.PrimaryMethod = findPrimaryMethod(methods)
}

func findPrimaryMethod(methods []repository.ContactMethod) *repository.ContactMethod {
	for i := range methods {
		if methods[i].IsPrimary {
			return &methods[i]
		}
	}
	return nil
}

func sortContactMethods(methods []repository.ContactMethod) {
	sort.SliceStable(methods, func(i, j int) bool {
		if methods[i].IsPrimary != methods[j].IsPrimary {
			return methods[i].IsPrimary
		}

		priorityI := contactMethodPriority(methods[i].Type)
		priorityJ := contactMethodPriority(methods[j].Type)
		if priorityI != priorityJ {
			return priorityI < priorityJ
		}

		if methods[i].CreatedAt.IsZero() || methods[j].CreatedAt.IsZero() {
			return methods[i].CreatedAt.Before(methods[j].CreatedAt)
		}

		return methods[i].CreatedAt.Before(methods[j].CreatedAt)
	})
}

func contactMethodPriority(methodType string) int {
	switch methodType {
	case string(repository.ContactMethodEmailPersonal):
		return 1
	case string(repository.ContactMethodEmailWork):
		return 2
	case string(repository.ContactMethodPhone):
		return 3
	case string(repository.ContactMethodTelegram):
		return 4
	case string(repository.ContactMethodSignal):
		return 5
	case string(repository.ContactMethodDiscord):
		return 6
	case string(repository.ContactMethodTwitter):
		return 7
	case string(repository.ContactMethodGChat):
		return 8
	default:
		return 99
	}
}

func suggestedActionForOverdueDays(daysOverdue int) string {
	switch {
	case daysOverdue <= 2:
		return "Send a quick check-in message"
	case daysOverdue <= 7:
		return "Schedule a call or coffee"
	case daysOverdue <= 30:
		return "Send a meaningful update about your life"
	default:
		return "Reconnect with something specific and personal"
	}
}
