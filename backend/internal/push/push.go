// Package push wires the Mac-daemon push-source providers into the sync
// ProviderRegistry. It is a thin leaf package: it imports the three push
// source packages (messages, icloudcontacts, phonecalls) plus sync, and
// nothing imports it back. Centralizing the registration here keeps the
// "register every push provider" step in one place that the agreement
// test can exercise against the daemonFamily descriptor table, and lets
// main.go register all push providers with a single call.
//
// The package deliberately lives outside service: messages already
// imports service, so a service→messages edge would create a cycle.
package push

import (
	"personal-crm/backend/internal/icloudcontacts"
	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/phonecalls"
	"personal-crm/backend/internal/sync"
)

// RegisterPushProviders registers every Mac-daemon push-source provider
// into reg. Each provider is push-only (Sync is a no-op; data lands via
// /api/v1/ingest/events), and exists so the scheduler's push-strategy
// exclusion and the /api/v1/sync/providers ground-truth list both have a
// registered entry per source. The anarlog push sources
// (anarlog_humans, anarlog_sessions) intentionally have no provider stub
// — they are not represented here.
func RegisterPushProviders(reg *sync.ProviderRegistry) {
	reg.Register(messages.New())
	reg.Register(icloudcontacts.New())
	reg.Register(phonecalls.New())
}
