package judgment

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CloseAnsweredObligationsTx closes, with their tasks, the reply obligations a
// recorded interaction of the contact answered: on each thread the contact
// takes part in, the user's own message after an "I owe a reply" obligation's
// cut-off closes it, and the owing participant's own message after a "they
// owe a reply" obligation's cut-off closes that one. No message closes a
// commitment, and a message on one thread closes nothing on another. It runs
// inside the recording's transaction (consumer.ObligationCloser).
func (s *Store) CloseAnsweredObligationsTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) error {
	tasks, err := s.tasks.ListOpenReplyObligationsForContactTx(ctx, tx, contactID)
	if err != nil {
		return fmt.Errorf("list open reply obligations: %w", err)
	}
	contexts := map[uuid.UUID]threadctx.Context{}
	for i := range tasks {
		task := &tasks[i]
		thread := task.Obligation.ThreadID
		c, ok := contexts[thread]
		if !ok {
			if c, err = s.contexts.Build(ctx, thread, now()); err != nil {
				return fmt.Errorf("build thread %s: %w", thread, err)
			}
			contexts[thread] = c
		}
		if answered(c, task) {
			if err := s.closeTaskTx(ctx, tx, task); err != nil {
				return err
			}
		}
	}
	return nil
}

// answered reports whether the owing party wrote on the thread after the
// obligation's cut-off.
func answered(c threadctx.Context, task *repository.ContactTask) bool {
	for _, entry := range c.Entries {
		if !entry.SentAt.After(task.Obligation.CutOff) {
			continue
		}
		p := c.Participants[entry.Role]
		if task.Obligation.Debtor == repository.ObligationDebtorUser {
			if p.IsUser {
				return true
			}
		} else if p.ContactID != nil && *p.ContactID == task.ContactID {
			return true
		}
	}
	return false
}
