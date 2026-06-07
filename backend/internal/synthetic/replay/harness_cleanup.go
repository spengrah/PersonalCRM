package replay

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"

	"github.com/riverqueue/river"
)

// teardownCleanupSource is the aggregation source the teardown's Gate-B check
// uses. Replays touch at most one aggregate source per harness in practice, but
// to be conservative the teardown checks BOTH messages and gchat aggregate jobs
// (an empty contact set short-circuits to 0).
const (
	teardownGateBBoundedWait = 30 * time.Second
	teardownPollInterval     = 100 * time.Millisecond
)

// teardown is the quiesce + conditional-cleanup closure registered on
// t.Cleanup / returned to non-test callers. It:
//  1. Stops THIS harness's River client (no further dispatch from this client).
//  2. Bounded-waits Gate B to reach zero (this replay's jobs finalize).
//  3. Gates the ENTIRE cleanup on Gate B == 0 — if unsettled, SKIP all deletes
//     and leave the namespaced (inert, obviously-synthetic) dataset intact.
//
// Rationale (D8): a retained unfinalized river_job, when a LATER shared
// default-queue River client picks it up, dereferences this replay's contact /
// comms_message / staging / calendar / event rows. Deleting ANY of them while a
// job is still live can fault that future worker. The only safe unsettled-path
// action is to leave the whole dataset; a follow-up DB reset reclaims it.
//
// Best-effort + logs (never fatal) so a teardown error never masks the real
// test failure. river_job rows are NEVER deleted here (D5a row 15).
func (h *Harness) teardown(stopCtx context.Context) error {
	if !h.stopped {
		stopC, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = h.client.Stop(stopC)
		cancel()
		h.stopped = true
	}

	// Bounded-wait Gate B to reach zero for both aggregate sources.
	if !h.waitTeardownGateB(stopCtx) {
		// Unsettled: skip ALL deletes; leave the namespaced dataset intact.
		return fmt.Errorf("synthetic teardown: Gate B did not clear within %s; skipping cleanup and leaving namespace %q intact",
			teardownGateBBoundedWait, h.namespace)
	}

	return h.cleanup(stopCtx)
}

// waitTeardownGateB bounded-waits until Gate B clears for BOTH aggregate sources
// (messages + gchat). Returns false on timeout.
func (h *Harness) waitTeardownGateB(ctx context.Context) bool {
	deadline := accelerated.GetCurrentTime().Add(teardownGateBBoundedWait)
	for accelerated.GetCurrentTime().Before(deadline) {
		if h.gateBClear(ctx, repository.InteractionSourceMessages) &&
			h.gateBClear(ctx, repository.InteractionSourceGChat) {
			return true
		}
		time.Sleep(teardownPollInterval)
	}
	// Final check (covers the boundary).
	return h.gateBClear(ctx, repository.InteractionSourceMessages) &&
		h.gateBClear(ctx, repository.InteractionSourceGChat)
}

// cleanup runs the D5a ID-tracked, FK-ordered teardown by tracked id (or
// ns-prefix for the genuinely prefixed columns). Best-effort: it logs and
// continues on per-table error so a single failure does not strand the rest.
// Callers MUST only run this after Gate B == 0 (see teardown).
func (h *Harness) cleanup(ctx context.Context) error {
	// Ensure the event-id union is captured even if Settle was never called
	// (e.g. an early test failure before Settle). Best-effort.
	if len(h.created.eventIDs) == 0 {
		_ = h.captureEventIDs(ctx)
	}

	h.createdMu.Lock()
	c := h.created
	prefix := h.gen.Prefix()
	h.createdMu.Unlock()

	var firstErr error
	step := func(label string, fn func() error) {
		if err := fn(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cleanup %s: %w", label, err)
		}
	}

	// 1. event_consumer_claim (by tracked event id).
	step("event_consumer_claim", func() error {
		_, err := h.support.DeleteEventConsumerClaimsByEventIds(ctx, c.eventIDs)
		return err
	})
	// 2. interaction (by tracked id).
	step("interaction", func() error {
		_, err := h.support.DeleteInteractionsByIds(ctx, c.interactionIDs)
		return err
	})
	// 3. event (by tracked id).
	step("event", func() error {
		_, err := h.support.DeleteEventsByIds(ctx, c.eventIDs)
		return err
	})
	// 4. comms_message (ns-prefixed external_id).
	step("comms_message", func() error {
		_, err := h.support.DeleteCommsMessagesByExternalIDPrefix(ctx, prefix)
		return err
	})
	// 5. messages_message (ns-prefixed guid).
	step("messages_message", func() error {
		_, err := h.support.DeleteMessagesMessageByGuidPrefix(ctx, prefix)
		return err
	})
	// 6. telegram_message (by this ns's peer ids).
	step("telegram_message", func() error {
		for _, peer := range c.telegramPeerIDs {
			if _, err := h.support.DeleteTelegramMessagesByPeerUserID(ctx, peer); err != nil {
				return err
			}
		}
		return nil
	})
	// 7. calendar_event (ns-prefixed gcal_event_id).
	step("calendar_event", func() error {
		_, err := h.support.DeleteCalendarEventsByGcalEventIDPrefix(ctx, prefix)
		return err
	})
	// 8. external_identity (by tracked id + ns-prefix backstop), BEFORE contact.
	step("external_identity_ids", func() error {
		_, err := h.support.DeleteExternalIdentitiesByIds(ctx, c.identityIDs)
		return err
	})
	step("external_identity_prefix", func() error {
		_, err := h.support.DeleteExternalIdentitiesBySourceIDPrefix(ctx, prefix)
		return err
	})
	// 9. external_contact (ns-prefixed source_id).
	step("external_contact", func() error {
		_, err := h.support.DeleteExternalContactsBySourceIDPrefix(ctx, prefix)
		return err
	})
	// 10. contact_task (by contact, hard delete — no deleted_at).
	step("contact_task", func() error {
		_, err := h.support.DeleteContactTasksByContactIds(ctx, c.contactIDs)
		return err
	})
	// 11. contact_method (by contact).
	step("contact_method", func() error {
		_, err := h.support.DeleteContactMethodsByContactIds(ctx, c.contactIDs)
		return err
	})
	// 12. note (by contact).
	step("note", func() error {
		_, err := h.support.DeleteNotesByContactIds(ctx, c.contactIDs)
		return err
	})
	// 13. contact (by tracked id; true DELETE so contact_enrichment cascades).
	step("contact", func() error {
		_, err := h.support.DeleteContactsByIds(ctx, c.contactIDs)
		return err
	})
	// 14. mac_host (the seeded revoked synthetic host by id).
	step("mac_host", func() error {
		_, err := h.support.DeleteMacHostByID(ctx, h.macHostID)
		return err
	})
	// 15. river_job: NEVER deleted here (D5a row 15) — finalized jobs are
	// reclaimed by River retention / DB reset.

	return firstErr
}

// ContactsRemaining counts surviving seeded contacts (test assertion that
// cleanup emptied the namespace).
func (h *Harness) ContactsRemaining(ctx context.Context) (int64, error) {
	return h.support.CountContactsByIds(ctx, h.snapshotContactIDs())
}

// --- deferred shim workers (bus needs client; real workers need bus) -------

type deferredRecorderWorker struct {
	river.WorkerDefaults[consumerjobs.InteractionRecorderJobArgs]
	real *consumer.InteractionRecorderWorker
}

func (w *deferredRecorderWorker) Work(ctx context.Context, j *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	if w.real == nil {
		return fmt.Errorf("deferredRecorderWorker invoked before real worker assignment")
	}
	return w.real.Work(ctx, j)
}

func (w *deferredRecorderWorker) Timeout(j *river.Job[consumerjobs.InteractionRecorderJobArgs]) time.Duration {
	if w.real == nil {
		return 30 * time.Second
	}
	return w.real.Timeout(j)
}

type deferredCadenceWorker struct {
	river.WorkerDefaults[consumerjobs.CadenceUpdaterJobArgs]
	real *consumer.CadenceUpdaterWorker
}

func (w *deferredCadenceWorker) Work(ctx context.Context, j *river.Job[consumerjobs.CadenceUpdaterJobArgs]) error {
	if w.real == nil {
		return fmt.Errorf("deferredCadenceWorker invoked before real worker assignment")
	}
	return w.real.Work(ctx, j)
}

func (w *deferredCadenceWorker) Timeout(j *river.Job[consumerjobs.CadenceUpdaterJobArgs]) time.Duration {
	if w.real == nil {
		return 30 * time.Second
	}
	return w.real.Timeout(j)
}

type deferredEmailWorker struct {
	river.WorkerDefaults[consumerjobs.EmailInteractionConsumerJobArgs]
	real *consumer.EmailInteractionConsumerWorker
}

func (w *deferredEmailWorker) Work(ctx context.Context, j *river.Job[consumerjobs.EmailInteractionConsumerJobArgs]) error {
	if w.real == nil {
		return fmt.Errorf("deferredEmailWorker invoked before real worker assignment")
	}
	return w.real.Work(ctx, j)
}

func (w *deferredEmailWorker) Timeout(j *river.Job[consumerjobs.EmailInteractionConsumerJobArgs]) time.Duration {
	if w.real == nil {
		return 30 * time.Second
	}
	return w.real.Timeout(j)
}

type deferredFollowUpWorker struct {
	river.WorkerDefaults[consumerjobs.FollowUpManagerJobArgs]
	real *consumer.FollowUpManagerWorker
}

func (w *deferredFollowUpWorker) Work(ctx context.Context, j *river.Job[consumerjobs.FollowUpManagerJobArgs]) error {
	if w.real == nil {
		return fmt.Errorf("deferredFollowUpWorker invoked before real worker assignment")
	}
	return w.real.Work(ctx, j)
}

func (w *deferredFollowUpWorker) Timeout(j *river.Job[consumerjobs.FollowUpManagerJobArgs]) time.Duration {
	if w.real == nil {
		return 30 * time.Second
	}
	return w.real.Timeout(j)
}
