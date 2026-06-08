package synthetic

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
)

// Profile names the synthetic world a seed entrypoint builds. It is pure
// orchestration over the existing factory + replay + meeting-note repo — no new
// replay/factory machinery.
type Profile string

const (
	// ProfileMinimalScoped is the smallest viable world: the existing SeedAll
	// shape (a few contacts, one Gmail-settled + one Telegram-settled each). It
	// is the CI/E2E namespacing baseline and the golden-scenario regression
	// pins its shape, so it MUST stay byte-stable == today's SeedAll behavior.
	ProfileMinimalScoped Profile = "minimal-scoped"
	// ProfileDev is a richer-than-minimal but fast local world spanning the
	// contact edge-case catalog + a settled interaction per source + a handful
	// of pending states, so every local UI surface has content.
	ProfileDev Profile = "dev"
	// ProfileProdShaped is the staging / #380 world: hundreds of contacts with a
	// prod-shaped distribution, the full contact-level edge-case catalog, and a
	// representative slice of the PRODUCIBLE pending states. Replay-heavy; slow
	// is fine (one-time staging seed).
	ProfileProdShaped Profile = "prod-shaped"
)

// ProfileResult is the counts-only summary a seed run reports (no PII). The
// crm-admin entrypoints print it; the QA harness (#380) parses it (or checks
// exit 0).
type ProfileResult struct {
	Profile           Profile
	Namespace         string
	Seed              uint64
	Contacts          int
	GmailSettled      int
	TelegramSettled   int
	GCalSettled       int
	IMessageSettled   int
	GChatSettled      int
	UnmatchedExternal int
	StrandedTelegram  int
	StrandedMessages  int
	UnmatchedCalendar int
	OrphanMeetingNote int
}

// ProfileParams returns the default SeedParams for a named profile (error on an
// unknown name, including the empty string). The caller may override Namespace /
// Seed / Counts before passing to RunProfile.
func ProfileParams(name Profile) (SeedParams, error) {
	switch name {
	case ProfileMinimalScoped:
		p := DefaultParams()
		// DefaultParams is already minimal-scoped; keep it byte-stable.
		return p, nil
	case ProfileDev:
		return SeedParams{
			Namespace: "dev",
			Seed:      factory.DefaultSeed,
			Profile:   ProfileDev,
			Counts: Counts{
				SeededContacts:    18,
				UnmatchedExternal: 2,
				StrandedTelegram:  1,
				StrandedMessages:  1,
				UnmatchedCalendar: 1,
				OrphanMeetingNote: 1,
			},
		}, nil
	case ProfileProdShaped:
		return SeedParams{
			Namespace: "prodshaped",
			Seed:      factory.DefaultSeed,
			Profile:   ProfileProdShaped,
			Counts: Counts{
				SeededContacts:    150,
				UnmatchedExternal: 12,
				StrandedTelegram:  6,
				StrandedMessages:  5,
				UnmatchedCalendar: 7,
				OrphanMeetingNote: 4,
			},
		}, nil
	default:
		return SeedParams{}, fmt.Errorf("synthetic: unknown profile %q (want one of %q, %q, %q)",
			name, ProfileMinimalScoped, ProfileDev, ProfileProdShaped)
	}
}

// RunProfile builds the selected profile's world against the harness. It is the
// single profile definition both crm-admin --seed and --reset-and-seed call; no
// HTTP handler consumes it (the profile world is CLI-only).
//
// The harness must be constructed for the SAME namespace + seed as params so the
// run's identifiers + cleanup scope line up (use NewHarnessWithDBForNamespace).
// RunProfile only SEEDS; the caller decides the lifecycle — Quiesce on success
// (seed-and-leave), the teardown closure on error (stop client + clean the
// partial world).
func RunProfile(ctx context.Context, h *Harness, params SeedParams) (ProfileResult, error) {
	res := ProfileResult{Profile: params.Profile, Namespace: h.Namespace(), Seed: params.Seed}
	switch params.Profile {
	case ProfileMinimalScoped:
		return runMinimalScoped(ctx, h, params, res)
	case ProfileDev, ProfileProdShaped:
		return runCatalogProfile(ctx, h, params, res)
	default:
		return res, fmt.Errorf("synthetic: RunProfile: unknown profile %q", params.Profile)
	}
}

// runMinimalScoped is exactly today's SeedAll (the byte-stable golden shape).
func runMinimalScoped(ctx context.Context, h *Harness, params SeedParams, res ProfileResult) (ProfileResult, error) {
	seedRes, err := SeedAll(ctx, h, params)
	if err != nil {
		return res, err
	}
	res.Contacts = len(seedRes.GmailContactIDs) + len(seedRes.TelegramContactIDs)
	res.GmailSettled = len(seedRes.GmailContactIDs)
	res.TelegramSettled = len(seedRes.TelegramContactIDs)
	return res, nil
}

// runCatalogProfile builds the dev / prod-shaped worlds: the full contact-level
// edge-case catalog at the requested volume, a settled interaction per source,
// and the PRODUCIBLE pending states. The two profiles differ only in volume
// (params.Counts); the orchestration is shared.
//
// PRODUCIBLE pending states (in scope): unmatched external_contact (Imports
// queue, via ReplayMacContacts MatchUnknown), stranded telegram_message
// (ReplayTelegram MatchUnknown), stranded messages_message (ReplayIMessage
// MatchUnknown), unmatched calendar attendee (ReplayGCal MatchUnknown), orphan
// meeting_note (existing meeting-note repo).
//
// DEFERRED coverage gap (NO toolkit producer today — documented, asserted-absent
// by the coverage check, tracked for a follow-on): conflict_pending meeting_note
// (needs a conflict_candidates snapshot referencing real events), title-candidate
// review rows (anarlog_title discovery), and comms_message without interaction_id
// (GChat MatchUnknown is match-only, writes no comms_message). Building these is a
// factory/replay addition out of E3 scope.
func runCatalogProfile(ctx context.Context, h *Harness, params SeedParams, res ProfileResult) (ProfileResult, error) {
	gen := h.Generator()
	n := params.Counts.SeededContacts
	if n <= 0 {
		n = 1
	}

	// --- Contact edge-case catalog (deterministic spread across n) ---------
	// These contacts carry the cadence / recency / birthday / name / no-method
	// edge-case shapes the UI + QA tour exercise. They are seeded WITHOUT a
	// settling MatchSeeded replay ON PURPOSE: a settled inbound interaction would
	// make the cadence updater overwrite last_contacted (destroying the overdue /
	// never-contacted intent), and every contact would carry a method (no
	// no-method bucket). The settled per-source interactions live on the
	// dedicated contacts below instead.
	var firstCatalogContactID uuid.UUID
	for i := 0; i < n; i++ {
		opts := catalogOptionsFor(i, n)
		spec := gen.Contact(opts...)
		contact, err := h.SeedContact(ctx, spec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed catalog contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		if i == 0 {
			firstCatalogContactID = contact.ID
		}
	}

	// Give the catalog its "≥1 contact with notes" bucket (a notepad note on the
	// first catalog contact, via the existing note repo).
	if firstCatalogContactID != uuid.Nil {
		if err := h.SeedNote(ctx, firstCatalogContactID, "catalog notepad note"); err != nil {
			return res, fmt.Errorf("profile %s: seed catalog note: %w", params.Profile, err)
		}
	}

	// A few settled interactions on DEDICATED contacts per source so every source
	// surface has matched content WITHOUT clobbering the catalog's cadence states.
	// Each settled contact carries a recent last_contacted (the inbound message),
	// which is the correct "recently contacted via X" representation.
	perSource := perSourceSettledCount(params.Profile)

	for i := 0; i < perSource; i++ {
		// Gmail-settled (email match).
		gmSpec := gen.Contact(factory.WithEmail())
		gmContact, err := h.SeedContact(ctx, gmSpec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed gmail contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		if _, err := h.ReplayGmail(ctx, gmContact.ID, gen.GmailMessage(gmSpec, factory.MatchSeeded)); err != nil {
			return res, fmt.Errorf("profile %s: replay gmail %d: %w", params.Profile, i, err)
		}
		res.GmailSettled++

		// Telegram-settled (handle match).
		tgSpec := gen.Contact(factory.WithTelegram())
		tgContact, err := h.SeedContact(ctx, tgSpec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed telegram contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		if _, err := h.ReplayTelegram(ctx, tgContact.ID, gen.TelegramMessage(tgSpec, factory.MatchSeeded)); err != nil {
			return res, fmt.Errorf("profile %s: replay telegram %d: %w", params.Profile, i, err)
		}
		res.TelegramSettled++

		// GCal-settled (email match).
		gcSpec := gen.Contact(factory.WithEmail())
		gcContact, err := h.SeedContact(ctx, gcSpec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed gcal contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		if _, err := h.ReplayGCal(ctx, gcContact.ID, gen.GCalEvent(gcSpec, factory.MatchSeeded)); err != nil {
			return res, fmt.Errorf("profile %s: replay gcal %d: %w", params.Profile, i, err)
		}
		res.GCalSettled++

		// GChat-settled (email match).
		gchatSpec := gen.Contact(factory.WithEmail())
		gchatContact, err := h.SeedContact(ctx, gchatSpec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed gchat contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		if _, err := h.ReplayGChat(ctx, gchatContact.ID, gen.GChatMessage(gchatSpec, factory.MatchSeeded)); err != nil {
			return res, fmt.Errorf("profile %s: replay gchat %d: %w", params.Profile, i, err)
		}
		res.GChatSettled++

		// iMessage-settled (phone match).
		imSpec := gen.Contact(factory.WithPhone())
		imContact, err := h.SeedContact(ctx, imSpec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed imessage contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		imageSpec, err := gen.IMessage(imSpec, factory.MatchSeeded, h.MacHostID())
		if err != nil {
			return res, fmt.Errorf("profile %s: build imessage spec %d: %w", params.Profile, i, err)
		}
		if _, err := h.ReplayIMessage(ctx, imContact.ID, imageSpec); err != nil {
			return res, fmt.Errorf("profile %s: replay imessage %d: %w", params.Profile, i, err)
		}
		res.IMessageSettled++
	}

	// --- PRODUCIBLE pending states -----------------------------------------
	// Unmatched external_contact (Imports queue) via ReplayMacContacts MatchUnknown.
	for i := 0; i < params.Counts.UnmatchedExternal; i++ {
		spec, err := gen.MacContact(gen.Contact(factory.WithEmail()), factory.MatchUnknown, h.MacHostID())
		if err != nil {
			return res, fmt.Errorf("profile %s: build unmatched mac contact %d: %w", params.Profile, i, err)
		}
		if _, err := h.ReplayMacContacts(ctx, uuid.Nil, spec); err != nil {
			return res, fmt.Errorf("profile %s: replay unmatched mac contact %d: %w", params.Profile, i, err)
		}
		res.UnmatchedExternal++
	}
	// Stranded telegram_message via ReplayTelegram MatchUnknown.
	for i := 0; i < params.Counts.StrandedTelegram; i++ {
		spec := gen.TelegramMessage(gen.Contact(factory.WithTelegram()), factory.MatchUnknown)
		if _, err := h.ReplayTelegram(ctx, uuid.Nil, spec); err != nil {
			return res, fmt.Errorf("profile %s: replay stranded telegram %d: %w", params.Profile, i, err)
		}
		res.StrandedTelegram++
	}
	// Stranded messages_message via ReplayIMessage MatchUnknown.
	for i := 0; i < params.Counts.StrandedMessages; i++ {
		spec, err := gen.IMessage(gen.Contact(factory.WithPhone()), factory.MatchUnknown, h.MacHostID())
		if err != nil {
			return res, fmt.Errorf("profile %s: build stranded imessage %d: %w", params.Profile, i, err)
		}
		if _, err := h.ReplayIMessage(ctx, uuid.Nil, spec); err != nil {
			return res, fmt.Errorf("profile %s: replay stranded imessage %d: %w", params.Profile, i, err)
		}
		res.StrandedMessages++
	}
	// Unmatched calendar attendee via ReplayGCal MatchUnknown.
	for i := 0; i < params.Counts.UnmatchedCalendar; i++ {
		spec := gen.GCalEvent(gen.Contact(factory.WithEmail()), factory.MatchUnknown)
		if _, err := h.ReplayGCal(ctx, uuid.Nil, spec); err != nil {
			return res, fmt.Errorf("profile %s: replay unmatched calendar %d: %w", params.Profile, i, err)
		}
		res.UnmatchedCalendar++
	}
	// Orphan meeting_note via the existing meeting-note repo.
	for i := 0; i < params.Counts.OrphanMeetingNote; i++ {
		if _, err := h.SeedOrphanMeetingNote(ctx,
			fmt.Sprintf("synthetic orphan note %d", i),
			"synthetic orphan meeting summary"); err != nil {
			return res, fmt.Errorf("profile %s: seed orphan meeting note %d: %w", params.Profile, i, err)
		}
		res.OrphanMeetingNote++
	}

	return res, nil
}

// catalogOptionsFor returns the contact-builder options for catalog contact i of
// n, deterministically spreading the edge-case shapes across the population:
// cadence states (overdue / recent / never-contacted), the 1900 birthday
// sentinel, a unicode name, a descender name, and a no-method contact. Catalog
// contacts get NO settling replay, so these last_contacted states survive (a
// MatchSeeded inbound would otherwise let the cadence updater overwrite them).
//
// The no-method slot (i == 3) carries WithNoMethods, which overrides the email
// default — so it must NOT also request a method. The slots are mutually
// exclusive by i, so no contact gets both.
func catalogOptionsFor(i, n int) []factory.ContactOption {
	var opts []factory.ContactOption

	// The no-method bucket: a cadence-bearing contact with NO contact_method.
	if i == 3 && n > 3 {
		opts = append(opts, factory.WithNoMethods(), factory.WithCadence("monthly"), factory.WithOverdue())
		return opts
	}

	opts = append(opts, factory.WithEmail())

	// Cadence + recency spread: every third contact overdue, every third recent,
	// the rest never-contacted (no LastContacted).
	switch i % 3 {
	case 0:
		opts = append(opts, factory.WithCadence("monthly"), factory.WithOverdue())
	case 1:
		opts = append(opts, factory.WithCadence("weekly"), factory.WithRecent())
	default:
		// never-contacted: a cadence but no last_contacted.
		opts = append(opts, factory.WithCadence("quarterly"))
	}

	// One representative of each rendering / data edge case (only when the
	// population is large enough to carry them without crowding out the spread).
	switch {
	case i == 0:
		opts = append(opts, factory.WithBirthday1900Sentinel(3, 14))
	case i == 1 && n > 2:
		opts = append(opts, factory.WithUnicodeName())
	case i == 2 && n > 3:
		opts = append(opts, factory.WithDescenderName())
	}
	return opts
}

// perSourceSettledCount bounds the per-source settled-interaction count by
// profile, keeping the dev profile fast (replay-light) while prod-shaped carries
// a thicker slice.
func perSourceSettledCount(p Profile) int {
	if p == ProfileProdShaped {
		return 3
	}
	return 1
}

// SeedAllowed is the single chokepoint guard the seed/reset entrypoints route
// through. It returns an error iff CRM_ENV is a production alias (production /
// prod), so a destructive reset or shippable fake-fetcher seed can never run in
// production. Checked PRE-DB by the entrypoints.
func SeedAllowed(cfg *config.Config) error {
	switch cfg.Runtime.CRMEnvironment {
	case "production", "prod":
		return fmt.Errorf("synthetic: seed/reset refused — CRM_ENV is %q (production); the synthetic seed entrypoints are non-production only", cfg.Runtime.CRMEnvironment)
	default:
		return nil
	}
}
