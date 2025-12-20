package scheduler

import (
	"context"

	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/reminder"
	"personal-crm/backend/internal/service"

	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	cron            *cron.Cron
	reminderService *service.ReminderService
}

// zerologCronAdapter adapts zerolog to cron.Logger interface
type zerologCronAdapter struct{}

func (z zerologCronAdapter) Info(msg string, keysAndValues ...interface{}) {
	event := logger.Debug().Str("component", "cron")
	for i := 0; i < len(keysAndValues)-1; i += 2 {
		if key, ok := keysAndValues[i].(string); ok {
			event = event.Interface(key, keysAndValues[i+1])
		}
	}
	event.Msg(msg)
}

func (z zerologCronAdapter) Error(err error, msg string, keysAndValues ...interface{}) {
	event := logger.Error().Err(err).Str("component", "cron")
	for i := 0; i < len(keysAndValues)-1; i += 2 {
		if key, ok := keysAndValues[i].(string); ok {
			event = event.Interface(key, keysAndValues[i+1])
		}
	}
	event.Msg(msg)
}

func NewScheduler(reminderService *service.ReminderService) *Scheduler {
	// Create cron with second precision and structured logging
	c := cron.New(
		cron.WithSeconds(),
		cron.WithLogger(zerologCronAdapter{}),
	)

	return &Scheduler{
		cron:            c,
		reminderService: reminderService,
	}
}

func (s *Scheduler) Start() error {
	logger.Info().Msg("starting scheduler")

	// Get environment-aware cron specification
	cronSpec := reminder.GetSchedulerCronSpec()
	logger.Info().Str("cron_spec", cronSpec).Msg("using scheduler cron spec")

	// Schedule reminder generation job with environment-aware timing
	_, err := s.cron.AddFunc(cronSpec, func() {
		ctx := context.Background()
		logger.Info().Msg("running scheduled reminder generation job")

		if err := s.reminderService.GenerateRemindersForOverdueContacts(ctx); err != nil {
			logger.Error().Err(err).Msg("error in scheduled reminder generation")
		}
	})
	if err != nil {
		return err
	}

	// Optional: Schedule a cleanup job (only in production to avoid noise in testing)
	// Skip cleanup job in testing environments
	// In testing mode, we want to see all activity and avoid confusion

	// Start the cron scheduler
	s.cron.Start()
	logger.Info().Msg("scheduler started successfully")

	return nil
}

func (s *Scheduler) Stop() {
	logger.Info().Msg("stopping scheduler")
	s.cron.Stop()
	logger.Info().Msg("scheduler stopped")
}

// RunReminderGenerationNow triggers the reminder generation job immediately
// This is useful for testing or manual triggering
func (s *Scheduler) RunReminderGenerationNow() error {
	ctx := context.Background()
	logger.Info().Msg("running reminder generation job manually")
	return s.reminderService.GenerateRemindersForOverdueContacts(ctx)
}

// GetScheduledJobs returns information about scheduled jobs
func (s *Scheduler) GetScheduledJobs() []cron.Entry {
	return s.cron.Entries()
}
