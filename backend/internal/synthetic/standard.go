package synthetic

import (
	"context"
	"fmt"

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

// runStandardProfile builds the declared world and maps it onto the shared
// ProfileResult / SeedTimings shape, so crm-admin's existing summary printer
// works unchanged (one phase per world step).
func runStandardProfile(ctx context.Context, h *Harness, params SeedParams, res ProfileResult) (ProfileResult, error) {
	_, out, err := buildStandardWorld(ctx, h, params, res)
	return out, err
}

type standardWorldCaptureKey struct{}

type standardWorldCapture struct {
	world declare.WorldResult
	set   bool
}

// StandardWorldForTest runs the real RunProfile entrypoint with a private,
// per-call manifest capture. The capture keeps the test on production dispatch:
// removing or misrouting ProfileStandard cannot leave the world assertions green.
//
// The manifest carries the world's own EXECUTION-order creation log, which is
// the only sound way to assert that the pinned tour fixtures really do draw
// last: created_at order is NOT execution order, because several of those
// fixtures are deliberately backdated (one by three hundred days), so every
// edge contact created at the anchor sorts after them.
func StandardWorldForTest(ctx context.Context, h *Harness, params SeedParams) (declare.WorldResult, ProfileResult, error) {
	capture := &standardWorldCapture{}
	res, err := RunProfile(context.WithValue(ctx, standardWorldCaptureKey{}, capture), h, params)
	if err != nil {
		return capture.world, res, err
	}
	if !capture.set {
		return declare.WorldResult{}, res, fmt.Errorf(
			"synthetic: profile %q did not dispatch through the standard world builder", params.Profile)
	}
	return capture.world, res, nil
}

func buildStandardWorld(ctx context.Context, h *Harness, params SeedParams, res ProfileResult) (declare.WorldResult, ProfileResult, error) {
	support := repository.NewSyntheticSupportRepository(h.Database().Queries)

	world, err := declare.World(ctx, h, support, declare.WorldTail{
		Name: standardTailName,
		Run: func(ctx context.Context, h *Harness) ([]declare.Seeded, error) {
			return seedStandardTail(ctx, h, params, &res)
		},
	})
	if capture, ok := ctx.Value(standardWorldCaptureKey{}).(*standardWorldCapture); ok {
		capture.world = world
		capture.set = true
	}
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
// All eleven, not eight: three of the markers ride contacts the catalog profile
// creates inside its own phase blocks (an awaiting-reply contact with the whole
// causal chain behind it, an outbound-only contact, a reply-bridged mutual
// contact), and a world meant to replace that profile cannot inherit them from
// it. The three helpers below are the SHARED statements of those recipes — the
// catalog profile calls the same ones, at the same positions it always did.
func seedStandardTail(ctx context.Context, h *Harness, params SeedParams, res *ProfileResult) ([]declare.Seeded, error) {
	gen := h.Generator()
	out := make([]declare.Seeded, 0, len(PinnedFixtureMarkers))

	// The three riders first, in the order the catalog profile creates them, then
	// the replay-free pinned block — which stays genuinely last, as its own
	// contract requires.
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
		h.SetPinnedFixtureID(plan.marker, contact.ID)
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

// accountCatalogRider records facts the helper proved before propagating its
// error. The named rider counters describe the completed scenario, so they move
// only when the helper completed without error.
func accountCatalogRider(
	kind riderKind,
	rider riderSeedResult,
	seedErr error,
	res *ProfileResult,
	phasePayloads *int,
) error {
	if rider.contact != nil {
		res.Contacts++
	}
	*phasePayloads += rider.payloads
	if kind == riderPendingFollowUp {
		res.SettledInteractions += rider.payloads
	}
	if seedErr != nil {
		return seedErr
	}
	accountCompletedRider(kind, res)
	return nil
}

// accountStandardTailRider mirrors the catalog accounting, except contact
// totals come from the world's returned creation log in mapStandardWorldResult.
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
// These three recipes are stated ONCE, here, and called from both the catalog
// profile (at the positions it has always created them) and the standard
// world's tail. Each returns the contact it created and the number of source
// payloads it drove, leaving the ProfileResult bookkeeping to the caller — which
// is what keeps the catalog's own `SeededTasks > 0` gate at its call site rather
// than absorbed into a helper where it could silently stop applying.

// seedPendingFollowUpFixture builds the "awaiting reply" fixture COHERENTLY BY
// CONSTRUCTION, and the causal chain is the whole point.
//
// A follow-up loop is opened BY an outbound — it is the app waiting on a reply
// to something you actually sent. Hanging one on an arbitrary cadence-bearing
// contact with no outbound renders as "awaiting reply" with nothing to be
// awaiting a reply to, a state production cannot reach; the agentic judge caught
// exactly that and (correctly) failed the page for it. So: a contact WITH a
// cadence, a GCal event — whose interaction records as MUTUAL, and mutual bumps
// last_outreach_at — and only then the live follow-up.
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
	if _, err := h.ReplayGCal(ctx, contact.ID, gen.GCalEvent(spec, factory.MatchSeeded, factory.WithMessageAge(interactionSpreadAge(0, 0)))); err != nil {
		return riderSeedResult{contact: contact}, fmt.Errorf("replay gcal for awaiting-reply contact: %w", err)
	}
	if _, err := h.SeedPendingFollowUp(ctx, contact.ID, contact.FullName); err != nil {
		return riderSeedResult{contact: contact, payloads: 1}, fmt.Errorf("seed pending follow-up: %w", err)
	}
	h.SetPinnedFixtureID(FixtureMarkerPending, contact.ID)
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
	h.SetPinnedFixtureID(FixtureMarkerOutreach, contact.ID)
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
	h.SetMutualMessageContactID(contact.ID)
	h.SetPinnedFixtureID(FixtureMarkerResponse, contact.ID)
	return riderSeedResult{contact: contact, payloads: 2}, nil
}
