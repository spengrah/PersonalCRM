package scheduler

import (
	"context"

	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/service"

	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	cron        *cron.Cron
	syncService *service.SyncService
	syncEnabled bool
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

func NewScheduler(
	syncService *service.SyncService,
	syncEnabled bool,
) *Scheduler {
	// Create cron with second precision and structured logging
	c := cron.New(
		cron.WithSeconds(),
		cron.WithLogger(zerologCronAdapter{}),
	)

	return &Scheduler{
		cron:        c,
		syncService: syncService,
		syncEnabled: syncEnabled,
	}
}

func (s *Scheduler) Start() error {
	logger.Info().Msg("starting scheduler")

	// Schedule external sync check job (every 5 minutes)
	if s.syncEnabled && s.syncService != nil {
		syncCronSpec := GetExternalSyncCronSpec()
		_, err := s.cron.AddFunc(syncCronSpec, func() {
			ctx := context.Background()
			logger.Debug().Msg("checking for due external syncs")

			if err := s.syncService.RunDueSyncs(ctx); err != nil {
				logger.Error().Err(err).Msg("error running due syncs")
			}
		})
		if err != nil {
			return err
		}
		logger.Info().Str("cron_spec", syncCronSpec).Msg("external sync scheduler enabled")
	}

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

// GetScheduledJobs returns information about scheduled jobs
func (s *Scheduler) GetScheduledJobs() []cron.Entry {
	return s.cron.Entries()
}

// GetExternalSyncCronSpec returns the cron specification for external sync checks.
// Returns "0 */5 * * * *" (every 5 minutes) to support variable sync intervals.
// The fastest supported sync interval is 15 minutes (CalendarDefaultInterval),
// so checking every 5 minutes ensures syncs run within 5 minutes of being due.
func GetExternalSyncCronSpec() string {
	return "0 */5 * * * *"
}
