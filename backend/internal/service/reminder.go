package service

import (
	"context"
	"log"
	"time"

	"personal-crm/backend/internal/reminder"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

type ReminderService struct {
	reminderRepo *repository.ReminderRepository
	contactRepo  *repository.ContactRepository
}

func NewReminderService(reminderRepo *repository.ReminderRepository, contactRepo *repository.ContactRepository) *ReminderService {
	return &ReminderService{
		reminderRepo: reminderRepo,
		contactRepo:  contactRepo,
	}
}

// GenerateRemindersForOverdueContacts creates reminders for contacts that are overdue based on their cadence
// This function is idempotent - it won't create duplicate reminders for the same day
func (s *ReminderService) GenerateRemindersForOverdueContacts(ctx context.Context) error {
	log.Println("Starting reminder generation job...")

	// Get all contacts with a cadence set
	contacts, err := s.contactRepo.ListContacts(ctx, repository.ListContactsParams{
		Limit:  1000, // Process in batches if needed
		Offset: 0,
	})
	if err != nil {
		log.Printf("Error fetching contacts: %v", err)
		return err
	}

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, now.Location()) // Set reminder for 9 AM

	remindersCreated := 0
	for _, contact := range contacts {
		if contact.Cadence == nil || *contact.Cadence == "" {
			continue // Skip contacts without cadence
		}

		cadenceType, err := reminder.ParseCadence(*contact.Cadence)
		if err != nil {
			log.Printf("Invalid cadence for contact %s: %v", contact.FullName, err)
			continue
		}

		// Check if contact is overdue using environment-aware cadences
		if !reminder.IsOverdueWithConfig(cadenceType, contact.LastContacted, contact.CreatedAt, now) {
			continue
		}

		// Check if we already have a reminder for today for this contact
		existingReminders, err := s.reminderRepo.ListRemindersByContact(ctx, contact.ID)
		if err != nil {
			log.Printf("Error checking existing reminders for contact %s: %v", contact.FullName, err)
			continue
		}

		// Check if there's already a reminder for today
		hasReminderForToday := false
		for _, existingReminder := range existingReminders {
			if !existingReminder.Completed &&
				existingReminder.DueDate.Year() == today.Year() &&
				existingReminder.DueDate.Month() == today.Month() &&
				existingReminder.DueDate.Day() == today.Day() {
				hasReminderForToday = true
				break
			}
		}

		if hasReminderForToday {
			continue // Skip if reminder already exists for today
		}

		// Calculate days since last contact for description
		daysSinceLastContact := 0
		if contact.LastContacted != nil {
			duration := now.Sub(*contact.LastContacted)
			daysSinceLastContact = int(duration.Hours() / 24)
		} else {
			duration := now.Sub(contact.CreatedAt)
			daysSinceLastContact = int(duration.Hours() / 24)
		}

		// Create reminder
		title := reminder.GenerateReminderTitle(contact.FullName, cadenceType)
		description := reminder.GenerateReminderDescription(contact.FullName, cadenceType, daysSinceLastContact)

		_, err = s.reminderRepo.CreateReminder(ctx, repository.CreateReminderRequest{
			ContactID:   &contact.ID,
			Title:       title,
			Description: &description,
			DueDate:     today,
		})

		if err != nil {
			log.Printf("Error creating reminder for contact %s: %v", contact.FullName, err)
			continue
		}

		remindersCreated++
		log.Printf("Created reminder for %s (overdue by %d days)", contact.FullName, reminder.GetOverdueDaysWithConfig(cadenceType, contact.LastContacted, contact.CreatedAt, now))
	}

	log.Printf("Reminder generation completed. Created %d new reminders.", remindersCreated)
	return nil
}

// GetDueReminders returns all reminders that are due by the specified time
func (s *ReminderService) GetDueReminders(ctx context.Context, dueBy time.Time) ([]repository.DueReminder, error) {
	return s.reminderRepo.ListDueReminders(ctx, dueBy)
}

// GetTodayReminders returns all reminders due today
func (s *ReminderService) GetTodayReminders(ctx context.Context) ([]repository.DueReminder, error) {
	now := time.Now()
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, now.Location())
	return s.GetDueReminders(ctx, endOfDay)
}

// CompleteReminder marks a reminder as completed
func (s *ReminderService) CompleteReminder(ctx context.Context, reminderID uuid.UUID) (*repository.Reminder, error) {
	return s.reminderRepo.CompleteReminder(ctx, reminderID)
}

// CreateReminder creates a new manual reminder
func (s *ReminderService) CreateReminder(ctx context.Context, req repository.CreateReminderRequest) (*repository.Reminder, error) {
	return s.reminderRepo.CreateReminder(ctx, req)
}

// GetAllReminders returns all reminders with pagination
func (s *ReminderService) GetAllReminders(ctx context.Context, params repository.ListRemindersParams) ([]repository.Reminder, error) {
	return s.reminderRepo.ListReminders(ctx, params)
}

// GetRemindersByContact returns all reminders for a specific contact
func (s *ReminderService) GetRemindersByContact(ctx context.Context, contactID uuid.UUID) ([]repository.Reminder, error) {
	return s.reminderRepo.ListRemindersByContact(ctx, contactID)
}

// DeleteReminder soft deletes a reminder
func (s *ReminderService) DeleteReminder(ctx context.Context, reminderID uuid.UUID) error {
	return s.reminderRepo.SoftDeleteReminder(ctx, reminderID)
}

// GetReminderStats returns statistics about reminders
func (s *ReminderService) GetReminderStats(ctx context.Context) (map[string]interface{}, error) {
	now := time.Now()
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, now.Location())

	totalReminders, err := s.reminderRepo.CountReminders(ctx)
	if err != nil {
		return nil, err
	}

	dueToday, err := s.reminderRepo.CountDueReminders(ctx, endOfDay)
	if err != nil {
		return nil, err
	}

	overdue, err := s.reminderRepo.CountDueReminders(ctx, now)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"total_reminders": totalReminders,
		"due_today":       dueToday,
		"overdue":         overdue,
	}, nil
}

