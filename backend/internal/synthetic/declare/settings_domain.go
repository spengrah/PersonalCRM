package declare

// Settings-domain resolutions (spec/settings.yaml, plus TDS-034 — the manual
// Todoist sync behavior whose citing test lives in settings.spec.ts's Todoist
// section, so its resolution is stated beside the surface it describes).
//
// Every behavior here is no-fixture, by one of two routes. MOST are
// route-mocked: the provider surfaces read credential rows, OAuth scopes and
// sync states that a seed cannot produce without real provider credentials, so
// the spec fulfils those endpoints with page.route and asserts the branches the
// payloads drive. SET-028 and SET-035 mock NOTHING — they exercise live
// endpoints whose observable does not depend on stored rows.
//
// Each test mocks only the endpoints it actually reads, so where a behavior's
// citations differ the reason below states them per test rather than claiming a
// union none of them fulfils. settings.spec.ts never constructs a TestAPI and
// issues no product write.
func init() {
	RegisterNone("SET-019", "its two citing tests mock only what each one reads: the not-connected case fulfils both provider account lists with a 404, and the connected-account case fulfils /auth/google/accounts plus /sync/status. Section state, account identity and connect date are read back out of those payloads")
	RegisterNone("SET-020", "the citing test fulfils /auth/google/accounts with a 404 and /auth/google with a consent URL stubbed to a same-origin path, so the whole-page navigation is observable without a live provider")
	RegisterNone("SET-021", "both citing tests fulfil /auth/google/accounts and /sync/status, and drive the outcome from the ?auth= landing params — request state, not stored rows")
	RegisterNone("SET-022", "both citing tests fulfil /auth/google/accounts, /sync/status and /auth/google/accounts/*/revoke; the only-the-confirmed-account-revoked claim is asserted by counting calls inside the revoke route handler")
	RegisterNone("SET-023", "cited by the same test as SET-019's provider-section item, which forces BOTH provider account endpoints to 404 so the section renders its not-connected branch and its required-configuration list")
	RegisterNone("SET-024", "all three citing tests fulfil /auth/google/accounts with a hand-built scope set plus /sync/status; the affordances are a pure function of those scopes")
	RegisterNone("SET-025", "the citing test fulfils /auth/google/accounts and /sync/status, pinning a credential timestamp against a sync auth-error timestamp — a relative ordering no seeded credential could hold")
	RegisterNone("SET-026", "the citing test fulfils /auth/todoist/accounts and /sync/status plus the three endpoints the picker reads (/todoist/settings, /todoist/projects, /todoist/labels); the options come from the Todoist API, not from any local table")
	RegisterNone("SET-028", "the citing test mocks nothing: it drives the LIVE /export and /import endpoints and asserts the download filename and the import request. The backup surface reports on whatever the database already holds, so no fixture changes what it shows")
	RegisterNone("SET-035", "the citing test mocks nothing: it reads the build-stamped version out of the live System Information region. The value comes from ldflags, not from any row")
	RegisterNone("TDS-034", "the citing test fulfils /auth/todoist/accounts, /sync/status, the three /todoist settings endpoints and /sync/todoist/trigger; a live trigger would need a Todoist provider the E2E environment does not have")
}
