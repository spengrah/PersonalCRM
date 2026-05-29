package service_test

import (
	"sort"
	"testing"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/mac"
	"personal-crm/backend/internal/push"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/sync"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDaemonFamilies_AgreeWithRegistries is the centerpiece mechanical
// guard against the daemon-push "add a source in N places" ritual
// drifting. It reads service.DaemonFamilyViews() and asserts the
// descriptor table agrees set-for-set (and per-family mapping-for-
// mapping) with every other place the daemon-push ritual is duplicated:
// events.AllKinds / kindPayloadTypes, mac.AllowedPushSources, the
// repository.InteractionSource* constants, and the push ProviderRegistry
// stubs built by push.RegisterPushProviders.
//
// The test lives in package service_test because it imports push, which
// transitively imports service — an internal package service test could
// not (cycle). External test packages are separate from package service
// for Go's cycle rule, so service_test may import both.
func TestDaemonFamilies_AgreeWithRegistries(t *testing.T) {
	views := service.DaemonFamilyViews()
	require.Len(t, views, 4, "expected exactly four daemon families")

	// expectedDaemonPushKinds is the canonical set of the 8 daemon-push
	// kinds. Pinned here so adding a kind to a family without updating
	// this list (or vice versa) fails loudly.
	expectedDaemonPushKinds := []events.Kind{
		events.KindRawMessageReceived, events.KindRawMessageSent,
		events.KindExternalContactUpserted, events.KindExternalContactDeleted,
		events.KindMeetingNoteRecorded, events.KindMeetingNoteDeleted,
		events.KindCallReceived, events.KindCallSent,
	}

	// --- Assertion 1: union of family kinds == the 8 daemon-push kinds,
	// AND each is a member of events.AllKinds. ---
	allKindsSet := make(map[events.Kind]struct{}, len(events.AllKinds))
	for _, k := range events.AllKinds {
		allKindsSet[k] = struct{}{}
	}
	unionKinds := make([]events.Kind, 0, 8)
	for _, v := range views {
		unionKinds = append(unionKinds, v.Kinds...)
	}
	assert.ElementsMatch(t, expectedDaemonPushKinds, unionKinds,
		"union of family kinds must equal the 8 daemon-push kinds")
	for _, k := range unionKinds {
		_, ok := allKindsSet[k]
		assert.Truef(t, ok, "family kind %q must be in events.AllKinds", k)
	}

	// --- Assertion 2: every family kind has a registered payload type
	// (events.IsKnownKind covers kindPayloadTypes). ---
	for _, k := range unionKinds {
		assert.Truef(t, events.IsKnownKind(k),
			"family kind %q must have a registered payload type (kindPayloadTypes)", k)
	}

	// --- Assertion 3: union of allowedSources == mac.AllowedPushSources
	// exactly (5 sources). Ties the kind-side and source-side together. ---
	unionSources := make([]string, 0)
	for _, v := range views {
		unionSources = append(unionSources, v.AllowedSources...)
	}
	expectedPushSources := make([]string, 0, len(mac.AllowedPushSources))
	for s := range mac.AllowedPushSources {
		expectedPushSources = append(expectedPushSources, s)
	}
	assert.ElementsMatch(t, expectedPushSources, unionSources,
		"union of family allowedSources must equal mac.AllowedPushSources")

	// --- Assertion 4: interaction-source agreement (Go-side). ---
	// 4a: every non-empty interactionSource is a repository.InteractionSource* constant.
	allInteractionSources := map[string]struct{}{
		repository.InteractionSourceManual:          {},
		repository.InteractionSourceGCal:            {},
		repository.InteractionSourceTodoist:         {},
		repository.InteractionSourceTelegram:        {},
		repository.InteractionSourceMessages:        {},
		repository.InteractionSourceAnarlogSessions: {},
		repository.InteractionSourcePhoneCalls:      {},
	}
	// 4b: writesInteractions <=> non-empty interactionSource (both directions).
	pushInteractionSources := make([]string, 0)
	for _, v := range views {
		if v.WritesInteractions {
			assert.NotEmptyf(t, v.InteractionSource,
				"family %q writes interactions but has empty interactionSource", v.Name)
			_, known := allInteractionSources[v.InteractionSource]
			assert.Truef(t, known,
				"family %q interactionSource %q is not a repository.InteractionSource* constant",
				v.Name, v.InteractionSource)
			pushInteractionSources = append(pushInteractionSources, v.InteractionSource)
		} else {
			assert.Emptyf(t, v.InteractionSource,
				"family %q does not write interactions but has interactionSource %q",
				v.Name, v.InteractionSource)
		}
	}
	// 4c: the set of descriptor push interactionSource values equals exactly
	// the set of PUSH repository.InteractionSource* constants. The 4
	// Pi-internal constants (manual/gcal/todoist/telegram) are out of
	// descriptor scope — daemon-push families never carry them.
	expectedPushInteractionSources := []string{
		repository.InteractionSourceMessages,
		repository.InteractionSourceAnarlogSessions,
		repository.InteractionSourcePhoneCalls,
	}
	assert.ElementsMatch(t, expectedPushInteractionSources, pushInteractionSources,
		"descriptor push interactionSource set must equal the push InteractionSource* constants")

	// --- Assertion 5: provider-stub completeness against the REAL
	// push.RegisterPushProviders helper. ---
	reg := sync.NewProviderRegistry()
	push.RegisterPushProviders(reg)
	pushStrategyNames := make(map[string]struct{})
	for _, cfg := range reg.List() {
		if cfg.Strategy == repository.SyncStrategyPush {
			pushStrategyNames[cfg.Name] = struct{}{}
		}
	}
	// 5a: every providerStubSources member is a registered push provider.
	stubUnion := make(map[string]struct{})
	for _, v := range views {
		for _, src := range v.ProviderStubSources {
			stubUnion[src] = struct{}{}
			// must be a subset of the family's own allowedSources.
			assert.Containsf(t, v.AllowedSources, src,
				"family %q providerStubSources member %q must be in its allowedSources", v.Name, src)
			_, registered := pushStrategyNames[src]
			assert.Truef(t, registered,
				"providerStubSources member %q has no push provider registered by RegisterPushProviders", src)
		}
	}
	// 5b: conversely every registered push-strategy provider is covered by
	// some providerStubSources set.
	for name := range pushStrategyNames {
		_, covered := stubUnion[name]
		assert.Truef(t, covered,
			"registered push provider %q is not covered by any family's providerStubSources", name)
	}

	// --- Assertion 6: no two families share a kind (partition check). ---
	seenKind := make(map[events.Kind]string)
	for _, v := range views {
		for _, k := range v.Kinds {
			if prev, dup := seenKind[k]; dup {
				t.Fatalf("kind %q appears in both family %q and family %q", k, prev, v.Name)
			}
			seenKind[k] = v.Name
		}
	}

	// --- Assertion 7: per-family mapping pins (catch cross-family
	// metadata swaps that the set-union checks above would miss). Because
	// DaemonFamilyViews() returns sorted slices, compare each view to its
	// expected literal by Name with require.Equal. ---
	expectedByName := map[string]service.DaemonFamilyView{
		"raw_message": {
			Name:                "raw_message",
			Kinds:               sortedKinds(events.KindRawMessageReceived, events.KindRawMessageSent),
			AllowedSources:      []string{"messages"},
			ProviderStubSources: []string{"messages"},
			WritesInteractions:  true,
			InteractionSource:   repository.InteractionSourceMessages,
		},
		"external_contact": {
			Name:                "external_contact",
			Kinds:               sortedKinds(events.KindExternalContactDeleted, events.KindExternalContactUpserted),
			AllowedSources:      []string{"anarlog_humans", "icloud_contacts"},
			ProviderStubSources: []string{"icloud_contacts"},
			WritesInteractions:  false,
			InteractionSource:   "",
		},
		"meeting_note": {
			Name:                "meeting_note",
			Kinds:               sortedKinds(events.KindMeetingNoteDeleted, events.KindMeetingNoteRecorded),
			AllowedSources:      []string{"anarlog_sessions"},
			ProviderStubSources: []string{},
			WritesInteractions:  true,
			InteractionSource:   repository.InteractionSourceAnarlogSessions,
		},
		"call": {
			Name:                "call",
			Kinds:               sortedKinds(events.KindCallReceived, events.KindCallSent),
			AllowedSources:      []string{repository.InteractionSourcePhoneCalls},
			ProviderStubSources: []string{repository.InteractionSourcePhoneCalls},
			WritesInteractions:  true,
			InteractionSource:   repository.InteractionSourcePhoneCalls,
		},
	}
	require.Len(t, expectedByName, len(views))
	for _, v := range views {
		expected, ok := expectedByName[v.Name]
		require.Truef(t, ok, "unexpected family %q in DaemonFamilyViews", v.Name)
		require.Equalf(t, expected, v, "family %q descriptor does not match its expected mapping", v.Name)
	}
}

// sortedKinds returns the given kinds sorted by string, matching the
// ordering DaemonFamilyViews() applies to the Kinds slice.
func sortedKinds(kinds ...events.Kind) []events.Kind {
	out := make([]events.Kind, len(kinds))
	copy(out, kinds)
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}
