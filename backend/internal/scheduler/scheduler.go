package scheduler

import (
	"context"
	"log"

	"personal-crm/backend/internal/service"

	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	cron            *cron.Cron
	reminderService *service.ReminderService
}

func NewScheduler(reminderService *service.ReminderService) *Scheduler {
	// Create cron with second precision and logging
	c := cron.New(
		cron.WithSeconds(),
		cron.WithLogger(cron.VerbosePrintfLogger(log.New(log.Writer(), "cron: ", log.LstdFlags))),
	)

	return &Scheduler{
		cron:            c,
		reminderService: reminderService,
	}
}

func (s *Scheduler) Start() error {
	log.Println("Starting scheduler...")

	// Schedule reminder generation job to run daily at 8:00 AM
	// Cron format: second minute hour day month weekday
	_, err := s.cron.AddFunc("0 0 8 * * *", func() {
		ctx := context.Background()
		log.Println("Running scheduled reminder generation job...")

		if err := s.reminderService.GenerateRemindersForOverdueContacts(ctx); err != nil {
			log.Printf("Error in scheduled reminder generation: %v", err)
		}
	})
	if err != nil {
		return err
	}

	// Optional: Schedule a cleanup job to run weekly on Sundays at 2:00 AM
	// This could clean up old completed reminders if needed
	_, err = s.cron.AddFunc("0 0 2 * * 0", func() {
		log.Println("Running weekly cleanup job...")
		// TODO: Implement cleanup logic if needed
	})
	if err != nil {
		return err
	}

	// Start the cron scheduler
	s.cron.Start()
	log.Println("Scheduler started successfully")

	return nil
}

func (s *Scheduler) Stop() {
	log.Println("Stopping scheduler...")
	s.cron.Stop()
	log.Println("Scheduler stopped")
}

// RunReminderGenerationNow triggers the reminder generation job immediately
// This is useful for testing or manual triggering
func (s *Scheduler) RunReminderGenerationNow() error {
	ctx := context.Background()
	log.Println("Running reminder generation job manually...")
	return s.reminderService.GenerateRemindersForOverdueContacts(ctx)
}

// GetScheduledJobs returns information about scheduled jobs
func (s *Scheduler) GetScheduledJobs() []cron.Entry {
	return s.cron.Entries()
}

