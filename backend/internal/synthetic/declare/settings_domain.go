package declare

// Settings-domain resolutions (spec/settings.yaml, plus TDS-034 — the manual
// Todoist sync behavior whose citing test lives in settings.spec.ts's Todoist
// section, so its resolution is stated beside the surface it describes).
//
// Every behavior here is no-fixture, and the two mechanisms are distinct.
// MOST are route-mocked: the provider surfaces read credential rows, OAuth
// scopes and sync states that a seed cannot produce without real provider
// credentials, so the spec fulfils those endpoints with page.route and asserts
// the branches the payloads drive. SET-028 and SET-035 are NOT mocked — they
// exercise live endpoints whose observable does not depend on stored rows.
// Neither mechanism has a data shape to declare, and settings.spec.ts never
// constructs a TestAPI.
func init() {
	RegisterNone("SET-019", "every citing assertion runs against page.route fulfilments of /auth/google/accounts, /auth/todoist/accounts and /sync/status; the account identity and connect date are read back out of those payloads")
	RegisterNone("SET-020", "the citing test fulfils /auth/google/accounts and /auth/google with page.route, stubbing the consent URL to a same-origin path so the navigation is observable without a live provider")
	RegisterNone("SET-021", "both citing tests fulfil /auth/google/accounts and /sync/status with page.route and drive the outcome from the ?auth= landing params, which are request state rather than stored rows")
	RegisterNone("SET-022", "both citing tests fulfil /auth/google/accounts and /auth/google/accounts/*/revoke with page.route, and the not-revoked claim is asserted by counting calls in the route handler")
	RegisterNone("SET-023", "cited by the same test as SET-019's provider-section item, which forces both provider account endpoints to 404 through page.route to reach the not-configured branch")
	RegisterNone("SET-024", "all three citing tests fulfil /auth/google/accounts with hand-built scope sets and /sync/status with page.route; the affordances are a pure function of those scopes")
	RegisterNone("SET-025", "the citing test fulfils /auth/google/accounts and /sync/status with page.route, pinning a credential timestamp against a sync auth-error timestamp — a relative ordering no seeded credential could hold")
	RegisterNone("SET-026", "the citing test fulfils /todoist/settings, /todoist/projects and /todoist/labels with page.route, because the picker options come from the Todoist API rather than from any local table")
	RegisterNone("SET-028", "the citing test drives the LIVE /export and /import endpoints and asserts the download filename and the import request; the backup surface is observable over whatever the database already holds, so no fixture changes what it shows")
	RegisterNone("SET-035", "the citing test reads the build-stamped version out of the live System Information region; the value comes from ldflags, not from any row")
	RegisterNone("TDS-034", "the citing test fulfils /todoist/settings, /todoist/projects, /todoist/labels and /sync/todoist/trigger with page.route; a live trigger would need a Todoist provider the E2E environment does not have")
}
