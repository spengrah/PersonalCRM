package declare

// WhatsApp-domain resolutions (spec/whatsapp.yaml, plus SET-036 — the WhatsApp
// section's presence on the settings surface, whose citing tests live in
// whatsapp-settings.spec.ts rather than settings.spec.ts).
//
// Every behavior here is no-fixture. The WhatsApp session is a linked companion
// device: whatsmeow needs a real phone to scan a real code, so the app's own
// /api/v1/whatsapp/* boundary is route-mocked and there is no state a seed could
// put the product into. SET-036's section-present case mocks NOTHING — it
// asserts the heading on the live settings page, where the feature is off.
//
// Each entry states the endpoints its citing tests actually mock, per test,
// rather than a union none of them fulfils. The spec never constructs a TestAPI.
//
// WHA-078 is surface: api and is outside the ui completeness universe, so it
// takes no entry here; its coverage is the Go API tests over the real router.
func init() {
	RegisterNone("SET-036", "its two citing tests differ: the section-present one mocks nothing and asserts the heading on the live settings page, and the not-configured one fulfils /whatsapp/auth/status with a 404 to reach that branch. Neither has a data shape to seed")
	RegisterNone("WHA-070", "its citing tests mock only as far as each drives the flow: the both-methods case fulfils /whatsapp/auth/status alone; the QR case fulfils it with successive pairing payloads to prove the displayed code follows the poll; the phone case adds /whatsapp/auth** so the start call flips the status; the cancel case does the same for /whatsapp/auth/cancel. Pairing is a live companion-device handshake, never stored state")
	RegisterNone("WHA-071", "the citing test fulfils /whatsapp/auth/status with a not_ready payload naming the missing dependency — a readiness verdict computed from process wiring, which no seeded row can produce")
	RegisterNone("WHA-072", "its citing tests fulfil /whatsapp/auth/status plus /whatsapp/chats, which the connected branch mounts. Account identity, backfill counts and the unresolved-peer count are process-lifetime observations off that payload")
	RegisterNone("WHA-073", "both citing tests — the warning and its negative control — fulfil /whatsapp/auth/status with the dropped-chunk count set and cleared, plus /whatsapp/chats for the connected branch. The count is derived from history-notification dispositions the drainer writes, not from seedable settings state")
	RegisterNone("WHA-074", "its three citations mock only what each reads: the confirmed-disconnect case fulfils /whatsapp/auth** and /whatsapp/chats so the DELETE flips the status, the failed-unlink case fulfils /whatsapp/auth/status alone, and the forced-clear case fulfils /whatsapp/auth** to capture the forced DELETE and its warning")
	RegisterNone("WHA-075", "the citing test loops three payloads over /whatsapp/auth/status, one per degraded-store flag, plus /whatsapp/chats. The flags report device-store outcomes that only a failed store operation produces")
	RegisterNone("WHA-076", "all five citing tests fulfil /whatsapp/auth/status plus the chat list — /whatsapp/chats for the listing, title-less, honest-copy and empty-state cases, and /whatsapp/chats** for the tracking-choice write. Discovered groups come from live message traffic, so there is no seedable path to the connected branch that renders them")
	RegisterNone("WHA-077", "all three citing tests fulfil /whatsapp/auth/status with terminal payloads — a logged-out disconnect, a temporary ban with its lift time, a startup error, and the logged-out payload again for the relink affordance. Those states are reached only by WhatsApp itself ending the session or the stack failing to come up, and the relink click is pure client state over the same payload")
}
