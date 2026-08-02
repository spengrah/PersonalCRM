package declare

// Telegram-domain resolutions (spec/telegram.yaml, plus SET-027 — the Telegram
// section's presence on the settings surface, whose citing tests live in
// telegram-settings.spec.ts rather than settings.spec.ts).
//
// Every behavior here is no-fixture, by one of two routes. The TGM behaviors
// and SET-027's not-configured case mock the app's own /api/v1/telegram/*
// boundary, which cannot run live in E2E because MTProto needs real credentials
// and a real account — so there is no state a seed could put the product into.
// SET-027's section-present case mocks NOTHING: it asserts the heading on the
// live settings page.
//
// Each test mocks only the endpoints it actually reads, so where a behavior's
// citations differ the reason below states them per test rather than claiming a
// union none of them fulfils. The spec never constructs a TestAPI.
func init() {
	RegisterNone("SET-027", "its two citing tests differ: the section-present one mocks nothing and asserts the heading on the live settings page, and the not-configured one fulfils /telegram/auth/status with a 404 to reach that branch. Neither has a data shape to seed")
	RegisterNone("TGM-038", "its citing tests mock only as far as each drives the flow: the Connect-reveals-phone and connected-state cases fulfil /telegram/auth/status alone; the phone-to-code case adds /telegram/auth/start; the code-connects case adds /telegram/auth/verify-code; the 2FA case adds /telegram/auth/verify-password. The handshake is MTProto, never stored state")
	RegisterNone("TGM-039", "its three citations mock only what each reads: the connected-state case fulfils /telegram/auth/status alone, the disconnect case adds /telegram/auth** and /telegram/chats, and the backfill-progress case adds /telegram/chats. Account identity and backfill counts come off those payloads")
	RegisterNone("TGM-040", "the citing test fulfils /telegram/auth/status and /telegram/auth/start, then returns an error payload from /telegram/auth/verify-code — the only way to reach the invalid-code branch")
	RegisterNone("TGM-041", "all four citing tests fulfil /telegram/auth/status plus the chat list — /telegram/chats for the listing and empty-state cases, /telegram/chats** for the two tracking-choice writes; discovered chats are MTProto results with no local source")
}
