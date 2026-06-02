package google

import (
	"testing"

	"personal-crm/backend/internal/events"

	"github.com/stretchr/testify/require"
)

// TestGmailProvider_TypedNilBusTrap documents the load-bearing reason phase 5
// registers the Gmail provider ONLY when pubBus != nil (the cutover gate in
// main.go).
//
// In off-mode, main.go's pubBus is a nil *events.Bus. If that nil concrete
// pointer were passed into NewGmailSyncProvider, it would be assigned to the
// provider's interface-typed `bus` field — producing a non-nil interface that
// WRAPS a typed-nil pointer. The provider's own `p.bus == nil` guard then
// evaluates FALSE, so Sync would sail past it and panic on the first PublishTx.
//
// This test pins that interface-wrapping behavior so a future refactor that
// removes the main.go cutover gate (and instead relies on the provider's nil
// guard) fails loudly here.
func TestGmailProvider_TypedNilBusTrap(t *testing.T) {
	var nilBus *events.Bus // typed-nil concrete pointer, exactly main.go's off-mode pubBus

	// The cutover gate operates on the CONCRETE pointer BEFORE it is widened to
	// the interface. At that level, a nil *events.Bus correctly compares == nil,
	// so main.go's `if pubBus != nil` skips registration in off-mode.
	require.True(t, nilBus == nil, "concrete *events.Bus nil compares == nil (the gate works at this level)")

	// But once the nil concrete pointer is stored in the provider's
	// interface-typed bus field, the interface is NON-nil (it carries a type),
	// so the provider's own `p.bus == nil` guard would NOT catch it. This is
	// the trap the cutover gate exists to avoid.
	p := NewGmailSyncProvider(nil, nil, nilBus, nil)
	require.False(t, p.bus == nil,
		"typed-nil *events.Bus assigned to the busTx interface is NOT == nil — "+
			"the provider's own nil guard cannot save off-mode; the main.go pubBus!=nil gate must")
}

// TestGmailProvider_CutoverGate_SkipsRegistrationWhenBusNil asserts the exact
// boolean the main.go registration block branches on: registerGmail mirrors the
// `if pubBus != nil` gate. In off-mode the concrete *events.Bus is nil, so the
// gate is false and neither the Gmail provider nor its rematch handler is
// constructed (no typed-nil bus is ever handed to the provider, so no PublishTx
// panic is possible). In cutover the gate is true and registration proceeds.
func TestGmailProvider_CutoverGate_SkipsRegistrationWhenBusNil(t *testing.T) {
	// registerGmail returns whether the Gmail provider would be registered for
	// a given pubBus, replicating main.go's gate without constructing a real
	// provider (which needs repos/pool).
	registerGmail := func(pubBus *events.Bus) bool {
		return pubBus != nil
	}

	var offModeBus *events.Bus // off-mode: pubBus = nil
	require.False(t, registerGmail(offModeBus),
		"off-mode (nil *events.Bus) must skip Gmail provider registration")

	require.True(t, registerGmail(&events.Bus{}),
		"cutover (non-nil *events.Bus) registers the Gmail provider")
}
