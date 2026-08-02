package declare

// Telegram-domain resolutions (spec/telegram.yaml, plus SET-027 — the Telegram
// section's presence on the settings surface, whose citing tests live in
// telegram-settings.spec.ts rather than settings.spec.ts).
//
// Every behavior here is no-fixture for one reason: the section talks to the
// app's own /api/v1/telegram/* endpoints, and those cannot run live in E2E
// because MTProto needs real credentials and a real account. There is no state
// a seed could put the product into, so the spec fulfils that boundary with
// page.route and asserts the real surface branches the mocked states drive.
func init() {
	RegisterNone("SET-027", "one citing test asserts the section heading on the live settings page and the other forces the not-configured branch by fulfilling /telegram/auth/status with a 404 through page.route; neither has a data shape to seed")
	RegisterNone("TGM-038", "every citing assertion runs against page.route fulfilments of /telegram/auth/status, /telegram/auth/start and /telegram/auth/verify-code — the connect flow is an MTProto handshake, not stored state")
	RegisterNone("TGM-039", "the citing tests fulfil /telegram/auth/status, /telegram/auth** and /telegram/chats with page.route; the account identity and backfill counts come off those payloads")
	RegisterNone("TGM-040", "the citing test fulfils /telegram/auth/start and then /telegram/auth/verify-code with an error payload through page.route, which is the only way to reach the invalid-code branch")
	RegisterNone("TGM-041", "every citing test fulfils /telegram/chats — and /telegram/chats** for the tracking-choice writes — with page.route; discovered chats are MTProto results with no local source")
}
