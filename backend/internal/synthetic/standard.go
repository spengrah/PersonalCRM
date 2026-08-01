package synthetic

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"
)

// ProfileStandard is the DECLARED world: every registered declaration, then
// every adversarial edge, then the pinned tour fixtures.
//
// It has no volume knobs. A catalog profile is sized by counts and then checked
// against a distribution; `standard` is exactly what the two registries say it
// is, which is why its content can be asserted as named states rather than as
// statistics. It lives here rather than in profiles.go on purpose: profiles.go
// is the modelling layer this world exists to replace.
const ProfileStandard Profile = "standard"

// standardTailName names the one step that runs after every declaration and
// every edge. The pinned tour fixtures must draw LAST — gen.Contact draws the
// shared name PRNG, and a shifted numeric identifier can land on one another
// contact already owns.
const standardTailName = "pinned-tour-fixtures"

// followUpScenarioCadence is the cadence on the "awaiting reply" rider. A
// follow-up loop only opens for a contact that HAS a cadence (CAD-011/CAD-012),
// so the scenario cannot express the state without one.
const followUpScenarioCadence = "monthly"

// Message ages for the two riders that carry message history. All are backward
// offsets from the generator anchor (applied via the factories' WithMessageAge),
// so they stay anchor-relative/deterministic (no time.Now()). The mutual reply is
// strictly NEWER than the mutual outbound (smaller age) and their gap is well
// under the aggregation reply-bridge window (replyBridgeHours=48h), so the inbound
// reply promotes the outbound interaction in place to mutual.
const (
	// riderFollowUpOutboundAge dates the awaiting-reply rider's OUTBOUND at the
	// anchor itself, so it is the newest thing on the contact and the loop it opens
	// is plainly current. An outbound moves last_outreach_at and touches neither
	// last_contacted nor last_interaction_at, which is the honest shape for a
	// contact still waiting on a first reply.
	riderFollowUpOutboundAge time.Duration = 0
	// messageOutboundAge dates the OUTBOUND-only rider's single message a few
	// days back — recent enough to be plainly current, not so recent it collides
	// with the ~2h/1h source default windows.
	messageOutboundAge = 3 * 24 * time.Hour
	// messageMutualOutboundAge dates the mutual pair's OUTBOUND half.
	messageMutualOutboundAge = 5 * 24 * time.Hour
	// messageMutualReplyAge dates the mutual pair's INBOUND reply: 6h newer than the
	// outbound (age 6h smaller), well within the 48h bridge window.
	messageMutualReplyAge = messageMutualOutboundAge - 6*time.Hour
)

// runStandardProfile builds the declared world and maps it onto the shared
// ProfileResult / SeedTimings shape, so crm-admin's existing summary printer
// works unchanged (one phase per world step).
func runStandardProfile(ctx context.Context, h *Harness, params SeedParams, res ProfileResult) (ProfileResult, error) {
	_, out, err := buildStandardWorld(ctx, h, params, res)
	return out, err
}

func buildStandardWorld(ctx context.Context, h *Harness, params SeedParams, res ProfileResult) (declare.WorldResult, ProfileResult, error) {
	support := repository.NewSyntheticSupportRepository(h.Database().Queries)

	world, err := declare.World(ctx, h, support, declare.WorldTail{
		Name: standardTailName,
		Run: func(ctx context.Context, h *Harness) ([]declare.Seeded, error) {
			return seedStandardTail(ctx, h, params, &res)
		},
	})
	res = mapStandardWorldResult(world, res)
	res.Timings.Settle = h.SettleStats()
	return world, res, err
}

func mapStandardWorldResult(world declare.WorldResult, res ProfileResult) ProfileResult {
	// The step log and partial counts are mapped on both paths. A failed world
	// still carries every completed step and entity plus the actual phase that
	// stopped it.
	for _, step := range world.Steps {
		res.Timings.Phases = append(res.Timings.Phases, PhaseTiming{
			Name:     step.Kind + ":" + step.Key,
			Duration: step.Duration,
		})
	}
	if world.Current != nil {
		res.Timings.Current = world.Current.Kind + ":" + world.Current.Key
	}

	for _, seeded := range world.Order {
		if seeded.Kind == "contact" {
			res.Contacts++
		}
	}
	return res
}

// seedStandardTail seeds the ELEVEN pinned tour fixtures and reports them in
// creation order.
//
// All eleven, not eight: three of the markers need a whole causal chain behind
// them rather than a bare contact — an awaiting-reply contact, an outbound-only
// contact, and a reply-bridged mutual contact — so they are seeded by their own
// recipes below instead of by the replay-free pinned block.
func seedStandardTail(ctx context.Context, h *Harness, params SeedParams, res *ProfileResult) ([]declare.Seeded, error) {
	gen := h.Generator()
	out := make([]declare.Seeded, 0, len(PinnedFixtureMarkers))

	// The three riders first, then the replay-free pinned block — which stays
	// genuinely last, as its own contract requires.
	pending, seedErr := seedPendingFollowUpFixture(ctx, h, gen)
	if err := accountStandardTailRider(riderPendingFollowUp, pending, seedErr, res, &out); err != nil {
		return out, fmt.Errorf("profile %s: %w", params.Profile, err)
	}

	outreach, seedErr := seedOutreachFixture(ctx, h, gen)
	if err := accountStandardTailRider(riderOutreach, outreach, seedErr, res, &out); err != nil {
		return out, fmt.Errorf("profile %s: %w", params.Profile, err)
	}

	response, seedErr := seedResponseFixture(ctx, h, gen)
	if err := accountStandardTailRider(riderResponse, response, seedErr, res, &out); err != nil {
		return out, fmt.Errorf("profile %s: %w", params.Profile, err)
	}

	for _, plan := range buildPinnedTourFixtures(gen) {
		contact, err := h.SeedContact(ctx, plan.spec)
		if err != nil {
			return out, fmt.Errorf("profile %s: seed pinned fixture %s: %w", params.Profile, plan.marker, err)
		}
		out = append(out, seededContact(contact))
	}
	return out, nil
}

func seededContact(c *repository.Contact) declare.Seeded {
	return declare.Seeded{Kind: "contact", ID: c.ID.String(), Name: c.FullName}
}

type riderKind uint8

const (
	riderPendingFollowUp riderKind = iota
	riderOutreach
	riderResponse
)

type riderSeedResult struct {
	contact  *repository.Contact
	payloads int
}

// accountStandardTailRider records the facts a rider helper PROVED before
// propagating its error, so a partial failure still reports what landed: the
// contact goes into the world's creation log the moment it exists, and the
// payload-derived counters move with the payloads actually driven. The named
// whole-rider counters describe the COMPLETED scenario, so they move only when
// the helper returned no error. Contact totals come from the creation log in
// mapStandardWorldResult, not from here.
func accountStandardTailRider(
	kind riderKind,
	rider riderSeedResult,
	seedErr error,
	res *ProfileResult,
	out *[]declare.Seeded,
) error {
	if rider.contact != nil {
		*out = append(*out, seededContact(rider.contact))
	}
	if kind == riderPendingFollowUp {
		res.SettledInteractions += rider.payloads
	}
	if seedErr != nil {
		return seedErr
	}
	accountCompletedRider(kind, res)
	return nil
}

func accountCompletedRider(kind riderKind, res *ProfileResult) {
	switch kind {
	case riderPendingFollowUp:
		res.SeededTasks++
		res.SeededPendingFollowUps = 1
	case riderOutreach:
		res.OutboundOnlyContacts++
	case riderResponse:
		res.MutualMessageContacts++
	}
}

// --- the three marker-bearing rider fixtures --------------------------------
//
// Each returns the contact it created and the number of source payloads it
// drove, leaving the ProfileResult bookkeeping to the caller — which is what
// lets a partial failure still be accounted for what it proved (see
// accountStandardTailRider) instead of being swallowed inside the recipe.

// seedPendingFollowUpFixture builds the "awaiting reply" fixture COHERENTLY BY
// CONSTRUCTION, and the causal chain is the whole point.
//
// A follow-up loop is opened BY an outbound — it is the app waiting on a reply to
// something you actually sent. Hanging one on an arbitrary cadence-bearing contact
// with no outbound renders as "awaiting reply" with nothing to be awaiting a reply
// to, a state production cannot reach.
//
// The outbound has to be an OUTBOUND, not a meeting. A matched calendar event
// records as MUTUAL, and a mutual IS a reply: it COMPLETES a live follow-up loop
// and can never open one — which is why the declared vocabulary rejects
// AwaitingReply beside MutualMeeting outright. A fixture composing the two states
// the same rules forbid is not coherent-by-construction, however it reads. So: a
// contact WITH a cadence, one OUTBOUND email, and only then the live follow-up —
// the same shape as the outreach fixture below, plus the loop.
//
// The cadence timestamps are deliberately not written here: they are sole-writer
// property of the cadence engine, applied asynchronously by its River worker,
// which is also why this cannot ASSERT on last_outreach_at (the job has not run
// yet). The coverage checks assert it post-Quiesce, which is the honest place.
func seedPendingFollowUpFixture(ctx context.Context, h *Harness, gen *factory.Generator) (riderSeedResult, error) {
	spec := gen.Contact(factory.WithEmail(), factory.WithCadence(followUpScenarioCadence), factory.WithNameMarker(FixtureMarkerPending))
	contact, err := h.SeedContact(ctx, spec)
	if err != nil {
		return riderSeedResult{}, fmt.Errorf("seed awaiting-reply contact: %w", err)
	}
	message := gen.GmailMessage(spec, factory.MatchSeeded, factory.WithOutbound(), factory.WithMessageAge(riderFollowUpOutboundAge))
	if _, err := h.ReplayGmail(ctx, contact.ID, message); err != nil {
		return riderSeedResult{contact: contact}, fmt.Errorf("replay outbound for awaiting-reply contact: %w", err)
	}
	if _, err := h.SeedPendingFollowUp(ctx, contact.ID, contact.FullName); err != nil {
		return riderSeedResult{contact: contact, payloads: 1}, fmt.Errorf("seed pending follow-up: %w", err)
	}
	return riderSeedResult{contact: contact, payloads: 1}, nil
}

// seedOutreachFixture builds the outbound-only "last outreach" fixture: one
// outbound gmail message, so last_outreach_at is set and last_contacted stays
// NULL (an outbound touches neither last_contacted nor last_interaction_at).
func seedOutreachFixture(ctx context.Context, h *Harness, gen *factory.Generator) (riderSeedResult, error) {
	spec := gen.Contact(factory.WithEmail(), factory.WithNameMarker(FixtureMarkerOutreach))
	contact, err := h.SeedContact(ctx, spec)
	if err != nil {
		return riderSeedResult{}, fmt.Errorf("seed outbound gmail contact: %w", err)
	}
	if _, err := h.ReplayGmail(ctx, contact.ID, gen.GmailMessage(spec, factory.MatchSeeded, factory.WithOutbound(), factory.WithMessageAge(messageOutboundAge))); err != nil {
		return riderSeedResult{contact: contact}, fmt.Errorf("replay outbound gmail: %w", err)
	}
	return riderSeedResult{contact: contact, payloads: 1}, nil
}

// seedResponseFixture builds the reply-bridged telegram MUTUAL fixture (the
// "last response" subject, distinct from the outreach one).
//
// Telegram runs its aggregation engine INLINE in ReplayTelegram (worker-free),
// so the promote is reliable. One OUTBOUND spec is built and then CLONED for the
// inbound reply — keeping the SAME PeerUserID and TelegramChatID, because the
// bridge requires prev.chatID == b.chatID and a second gen.TelegramMessage call
// would allocate a fresh peer/chat and never bridge — with the message id
// bumped, Out flipped, and a send time strictly newer than the outbound but
// inside the 48h bridge window. The inbound's aggregation finds the outbound
// interaction and promotes it in place: one mutual row, last_contacted ==
// last_interaction_at == last_outreach_at == last_response_at.
func seedResponseFixture(ctx context.Context, h *Harness, gen *factory.Generator) (riderSeedResult, error) {
	spec := gen.Contact(factory.WithTelegram(), factory.WithNameMarker(FixtureMarkerResponse))
	contact, err := h.SeedContact(ctx, spec)
	if err != nil {
		return riderSeedResult{}, fmt.Errorf("seed mutual telegram contact: %w", err)
	}
	outbound := gen.TelegramMessage(spec, factory.MatchSeeded, factory.WithOutbound(), factory.WithMessageAge(messageMutualOutboundAge))
	if _, err := h.ReplayTelegram(ctx, contact.ID, outbound); err != nil {
		return riderSeedResult{contact: contact}, fmt.Errorf("replay mutual telegram outbound: %w", err)
	}
	reply := outbound // clone: same peer + chat, so the bridge can fire
	reply.TelegramMessageID = outbound.TelegramMessageID + 1
	reply.Out = false
	// Newer than the outbound by exactly (outboundAge − replyAge) = 6h (< 48h).
	reply.SentAt = outbound.SentAt.Add(messageMutualOutboundAge - messageMutualReplyAge)
	if _, err := h.ReplayTelegram(ctx, contact.ID, reply); err != nil {
		return riderSeedResult{contact: contact, payloads: 1}, fmt.Errorf("replay mutual telegram reply: %w", err)
	}
	return riderSeedResult{contact: contact, payloads: 2}, nil
}
