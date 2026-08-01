package replay

import (
	"context"
	"fmt"
	"time"

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
	h.stopClient()

	// Bounded-wait Gate B to reach zero for both aggregate sources.
	if !h.waitTeardownGateB(stopCtx) {
		// Unsettled: skip ALL deletes; leave the namespaced dataset intact.
		return fmt.Errorf("synthetic teardown: Gate B did not clear within %s; skipping cleanup and leaving namespace %q intact",
			teardownGateBBoundedWait, h.namespace)
	}

	return h.cleanup(stopCtx)
}

// stopClient stops THIS harness's River client exactly once (idempotent). It is
// teardown's step 1, shared with Quiesce.
func (h *Harness) stopClient() {
	if h.stopped {
		return
	}
	stopC, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = h.client.Stop(stopC)
	cancel()
	h.stopped = true
}

// Quiesce is the "seed-and-leave" exit for non-test entrypoints (crm-admin
// --seed / --reset-and-seed). It runs teardown's step 1+2 — stop THIS harness's
// River client and bounded-wait Gate B so this run's jobs finalize — but SKIPS
// step 3 (the cleanup deletes), so the seeded namespace PERSISTS after the
// entrypoint exits.
//
// Use Quiesce on the SUCCESSFUL seed path only. On the error path the entrypoint
// must call the full teardown closure instead, which stops the client AND cleans
// up the partial namespace (so a failed seed is never a leave-behind). Either
// way the River client is always stopped, never leaked.
//
// It is NOT an error for Gate B to remain unsettled here: unlike teardown, there
// is nothing to gate (no deletes follow), and the client is already stopped, so
// the seeded rows are inert. The wait is best-effort settling for a tidier world.
func (h *Harness) Quiesce(ctx context.Context) error {
	h.stopClient()
	_ = h.waitTeardownGateB(ctx)
	return nil
}

// DrainGateB bounded-waits Gate B (this run's River jobs finalizing) to zero
// and errors loudly if it does not clear. It is teardown's step 2 exposed on
// its own for the "seed and leave" callers that must be able to PROMISE a
// quiescent namespace: a successful declared seed guarantees no job it created
// still references its rows, which is what makes a later stateless, id-set
// based cleanup safe. Unlike Quiesce it does NOT stop the client, so a caller
// can drain, act, and drain again.
func (h *Harness) DrainGateB(ctx context.Context) error {
	if h.waitTeardownGateB(ctx) {
		return nil
	}
	return fmt.Errorf("synthetic: Gate B did not clear within %s for namespace %q", teardownGateBBoundedWait, h.namespace)
}

// waitTeardownGateB bounded-waits until Gate B clears for BOTH aggregate sources
// (messages + gchat). Returns false on timeout. The budget is REAL wall-clock
// (see defaultSettleTimeout) so it does not collapse under TIME_ACCELERATION.
func (h *Harness) waitTeardownGateB(ctx context.Context) bool {
	open, cancel := realTimeBudget(teardownGateBBoundedWait)
	defer cancel()
	for open() {
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
	// telegram_chat_config (by tracked group chat id). It has no namespace column
	// (keyed only by telegram_chat_id), so a group replay's config rows are
	// deleted by the exact chat ids the ledger tracked. No FK to contact, so it
	// sits with the telegram_message step. Without it a group replay would leak
	// config rows on the shared DB and risk future cross-namespace chat-id
	// collisions.
	step("telegram_chat_config", func() error {
		_, err := h.support.DeleteTelegramChatConfigsByChatIds(ctx, c.telegramChatIDs)
		return err
	})
	// 7. calendar_event (ns-prefixed gcal_event_id).
	step("calendar_event", func() error {
		_, err := h.support.DeleteCalendarEventsByGcalEventIDPrefix(ctx, prefix)
		return err
	})
	// 8. external_identity (by ns-prefixed identifier + ns-scoped phone-digit
	// prefix + source_id backstop), BEFORE contact. The identifier-prefix delete
	// catches the source_id-NULL identities GCal/external_contact matching creates
	// (keyed by the synthetic email/handle); the phone-prefix delete catches phone
	// identities (normalized form +1<area>55501...), which are now ns-scoped via
	// the per-namespace area code so they no longer leak. external_identity
	// survives contact delete via ON DELETE SET NULL, so all must be cleared first
	// to avoid polluting future matching.
	step("external_identity_identifier", func() error {
		_, err := h.support.DeleteExternalIdentitiesByIdentifierPrefix(ctx, prefix)
		return err
	})
	step("external_identity_phone", func() error {
		_, err := h.support.DeleteExternalIdentitiesByIdentifierPrefix(ctx, h.gen.SyntheticPhonePrefix())
		return err
	})
	step("external_identity_source_id", func() error {
		_, err := h.support.DeleteExternalIdentitiesBySourceIDPrefix(ctx, prefix)
		return err
	})
	// external_identity for unmatched telegram peers. MatchPeer keys the identity
	// by source='telegram', source_id = the BARE peer id, and the synthetic handle
	// normalizes to 'synth_<ns>_<n>' (underscores) — neither matches the ns-prefix
	// ('synth-<ns>-') deletes above, so clear these by the tracked peer ids before
	// the contact delete.
	step("external_identity_telegram_peer", func() error {
		_, err := h.support.DeleteTelegramExternalIdentitiesByPeerIds(ctx, c.telegramPeerIDs)
		return err
	})
	// 9. external_contact (ns-prefixed source_id).
	step("external_contact", func() error {
		_, err := h.support.DeleteExternalContactsBySourceIDPrefix(ctx, prefix)
		return err
	})
	// external_contact telegram discovery candidates, keyed by the bare peer id
	// (source='telegram'), which the ns-prefix delete above misses.
	step("external_contact_telegram_peer", func() error {
		_, err := h.support.DeleteTelegramExternalContactsByPeerIds(ctx, c.telegramPeerIDs)
		return err
	})
	// external_contact rows the Seed* primitives created, by tracked id. The two
	// deletes above cannot reach all of them: a telegram DISCOVERY candidate's peer
	// id is only in the ledger when a MESSAGE replay tracked it, and an
	// anarlog_title row's source_id is a SHA-256 digest with no prefix to match at
	// all. This is the failure path of the declared seed, which reports the
	// namespace CLEAN when it returns nil — so a row it could not find would make
	// that report false.
	step("external_contact_tracked", func() error {
		_, err := h.support.DeleteExternalContactsByIds(ctx, c.externalContactIDs)
		return err
	})
	// 10. contact_task (by contact, hard delete — no deleted_at), plus the
	// Todoist-reconcile delta rows tracked by id (which may attach to
	// cadence-bearing contacts this run did not seed).
	step("contact_task", func() error {
		_, err := h.support.DeleteContactTasksByContactIds(ctx, c.contactIDs)
		return err
	})
	step("contact_task_todoist_delta", func() error {
		_, err := h.support.DeleteContactTasksByIds(ctx, c.contactTaskIDs)
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
	// 13a-pre2. relationship_signal on the seeded nodes: relationship_signal has no
	// deleted_at and its subject_node_id → node FK is NO ACTION, so any signal a
	// profile seeded MUST be deleted BEFORE the person/entity node deletes below.
	// Keyed by the tracked subject node ids (the seed only writes signals on person
	// nodes). No-op when no profile seeded any.
	step("relationship_signal", func() error {
		_, err := h.support.DeleteRelationshipSignalsForNodes(ctx, c.signalNodeIDs)
		return err
	})
	// 13a-pre. assertions on the seeded contact nodes: the assertion→node FK is
	// RESTRICT, so any assertion ReplayAssertion seeded on a person node MUST be
	// deleted BEFORE the person-node delete below. Provenance cascades from the
	// assertion. No-op when no profile/test seeded any (DeleteAssertionsForNode is
	// per-node; the loop is bounded by the seeded-contact count).
	step("assertions", func() error {
		for _, contactID := range c.contactIDs {
			if _, err := h.support.DeleteAssertionsForNode(ctx, contactID); err != nil {
				return err
			}
		}
		return nil
	})
	// 13a. person node (node.id == contact.id): SeedContact dual-writes a node
	// via the real service path, so teardown must remove it too or the shared
	// DB leaks an orphan node per seeded contact. There is no FK from
	// node→contact, so order relative to the contact delete is free. The
	// assertion→node FK is RESTRICT, so the assertion step above runs first.
	step("person_node", func() error {
		_, err := h.support.DeleteNodesByIds(ctx, c.contactIDs)
		return err
	})
	// 13a-entity. entity nodes (place/org/topic/tag): SeedEntity mints org/topic/tag
	// nodes and the contact-create authority flip (WithLocation) mints place nodes;
	// both carry an ns-prefixed canonical_label, so one label-prefix sweep removes
	// them (the entity subtype rows cascade on the node delete). Runs AFTER the
	// assertions step (the assertion→node FK is RESTRICT, and person→entity edges
	// reference these nodes as the OBJECT) so no edge still points at them. It
	// re-matches the already-deleted ns-prefixed person nodes harmlessly (0 rows) and
	// misses the empty-label venue nodes (cleaned by the by-id venue step below). A
	// surviving place→place `within` edge — which EnsurePlaceTx does NOT mint today
	// (flat place nodes) — would FK-block this delete loudly; the coverage test
	// asserts zero such edges so the regression is caught before this point.
	step("graph_entity_nodes", func() error {
		_, err := h.support.DeleteNodesByLabelPrefix(ctx, prefix)
		return err
	})
	// 13b. venue node (interaction.venue_id → node): the real recorders mint a
	// venue node per distinct container on the replay path. They are not contacts
	// (so person_node misses them) and have an empty canonical_label (so the
	// ns-prefix node delete misses them), so cleanup deletes them by the tracked
	// id set. Ordered AFTER the interaction delete (step 2) so the
	// interaction→venue restrict FK is already clear; the delete is additionally
	// guarded NOT EXISTS any remaining interaction, so a venue shared with an
	// un-cleaned interaction is left intact rather than FK-violating. The venue
	// subtype row cascades on the node delete.
	step("venue_node", func() error {
		_, err := h.support.DeleteVenueNodesByIds(ctx, c.venueNodeIDs)
		return err
	})
	// meeting_note scoped to the seeded synthetic host, BEFORE the mac_host
	// delete. A profile may seed orphan_needs_review meeting_note rows against
	// this host; the mac_host FK is ON DELETE SET NULL, so deleting the host
	// alone would leave them orphaned on the shared DB. No-op when no profile
	// seeded any.
	step("meeting_note", func() error {
		return h.support.DeleteMeetingNotesByHostID(ctx, h.macHostID)
	})
	// 13c. the CONSUMED pairing token each LIVE paired host was created from,
	// BEFORE the host delete. consumed_host_id is ON DELETE SET NULL and is the
	// token's only recovery key — CreatePairingToken returns the plaintext and the
	// expiry, never the row id, and only a hash is stored — so a host deleted
	// first strands the row permanently. The production janitor cannot reclaim it
	// either: it sweeps only tokens that were never consumed.
	tokensSwept := true
	step("mac_host_pairing_token", func() error {
		for _, id := range c.pairedMacHostIDs {
			if _, err := h.support.DeletePairingTokensByConsumedHostID(ctx, id); err != nil {
				tokensSwept = false
				return err
			}
		}
		return nil
	})
	// 13d. the LIVE paired hosts SeedPairedMacHost created. They are NOT reachable
	// by the marker delete below (which is keyed on this harness's single revoked
	// host id), and a survivor holds the database-wide singleton slot against
	// every later world that pairs one. Runs after the external_contact steps: the
	// external_contact.host_id FK is ON DELETE SET NULL, so a host deleted first
	// would orphan its ingest candidates from every host-scoped route.
	//
	// GATED on the token step, because this ladder CONTINUES past a failed step.
	// consumed_host_id is ON DELETE SET NULL and is the consumed token's only
	// recovery key, so deleting the host after the token delete failed converts a
	// retriable leftover into a permanently unreachable row — the janitor sweeps
	// only UNCONSUMED tokens. Leaving the host standing keeps both rows recoverable
	// (and the world discoverable, see the marker step below); the run already
	// reports the failure through firstErr.
	pairedHostsSwept := true
	if tokensSwept {
		step("mac_host_paired", func() error {
			for _, id := range c.pairedMacHostIDs {
				if _, err := h.support.DeleteMacHostByID(ctx, id); err != nil {
					pairedHostsSwept = false
					return err
				}
			}
			return nil
		})
	} else {
		pairedHostsSwept = len(c.pairedMacHostIDs) == 0
	}
	// 14. mac_host (the seeded revoked synthetic host by id).
	//
	// GATED for the same reason, one level out: the marker is what makes a
	// namespace DISCOVERABLE to a later cleanup. Deleting it while a live paired
	// host survives would strand that host under a token nothing can look up —
	// holding the database-wide singleton slot against every later world that
	// pairs one. Retaining the marker keeps the whole namespace recoverable, which
	// is the same invariant the cross-request ladder gets for free from its single
	// transaction.
	if pairedHostsSwept {
		step("mac_host", func() error {
			_, err := h.support.DeleteMacHostByID(ctx, h.macHostID)
			return err
		})
	}
	// 14a. namespace ownership records. Only the DECLARED seed path writes them
	// (keyed by this namespace), but this teardown is the failure path that same
	// path drives — and it reports whether the namespace came out CLEAN, so
	// leaving bookkeeping behind would make that report false.
	step("synthetic_namespace_entity", func() error {
		_, err := h.support.DeleteNamespaceEntities(ctx, h.namespace)
		return err
	})
	// 14b. river_queue: the row this harness's own producer upserted for its
	// PRIVATE queue when the client started. The client is stopped by step 1, so
	// nothing will ever fetch that queue again and the row is pure residue — one
	// per harness, accumulating on the shared database because a reset preserves
	// River's internal tables. It also has to go here specifically: the declared
	// seed's FAILURE path drives this teardown and reports the namespace as
	// cleaned when it returns nil, which would be false while the row survived.
	step("river_queue", func() error {
		_, err := h.support.DeleteRiverQueue(ctx, SyntheticQueueName(h.namespace))
		return err
	})
	// 15. river_job: NEVER deleted here (D5a row 15) — finalized jobs are
	// reclaimed by River retention / DB reset, and this teardown only runs once
	// Gate B is clear, so there is nothing unfinalized to reclaim. (The
	// CROSS-REQUEST declared cleanup does delete a namespace's own unfinalized
	// jobs, but only from its private queue and only while holding the
	// reservation — the case this teardown cannot reach, because its client is
	// already gone. See declare.CleanupNamespaces.)

	return firstErr
}

// ContactsRemaining counts surviving seeded contacts (test assertion that
// cleanup emptied the namespace).
func (h *Harness) ContactsRemaining(ctx context.Context) (int64, error) {
	return h.support.CountContactsByIds(ctx, h.snapshotContactIDs())
}

// VenueNodesRemaining counts how many of THIS run's tracked venue nodes still
// exist (test assertion that cleanup removed the venue nodes the real recorders
// minted on the replay path). Scoped to the tracked ids, so it is immune to
// parallel tests creating their own venue nodes on the shared DB.
func (h *Harness) VenueNodesRemaining(ctx context.Context) (int64, error) {
	return h.support.CountVenueNodesByIds(ctx, h.snapshotVenueNodeIDs())
}

// SignalsRemaining counts how many relationship_signal rows still exist on THIS
// run's tracked signal subject nodes. Used both ways: before teardown a coverage
// test asserts it is ≥1 (signals were seeded), and after an explicit teardown a
// determinism test asserts it is 0 (the signal-cleanup step ran before the node
// deletes). Scoped to the tracked node ids, so it is immune to parallel tests
// seeding their own signals on the shared DB.
func (h *Harness) SignalsRemaining(ctx context.Context) (int64, error) {
	return h.support.CountRelationshipSignalsForNodes(ctx, h.snapshotSignalNodeIDs())
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
