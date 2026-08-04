package whatsapp

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCommsAttacher records the exact arguments the handler passes, so the test
// pins the attach SELECTOR (peer_normalized, not peer_handle) rather than only
// the count.
type fakeCommsAttacher struct {
	calls         int
	gotSource     string
	gotNormalized *string
	gotHandle     *string
	gotContactID  uuid.UUID
	attached      int64
	deduped       int64
	err           error
}

func (f *fakeCommsAttacher) AttachUnmatchedByPeer(_ context.Context, source string, peerNormalized, peerHandle *string, contactID uuid.UUID) (int64, int64, error) {
	f.calls++
	f.gotSource = source
	f.gotNormalized = peerNormalized
	f.gotHandle = peerHandle
	f.gotContactID = contactID
	return f.attached, f.deduped, f.err
}

type fakeContactAggregator struct {
	calls        int
	gotContactID uuid.UUID
	err          error
}

func (f *fakeContactAggregator) AggregateForContactBatch(_ context.Context, contactID uuid.UUID) error {
	f.calls++
	f.gotContactID = contactID
	return f.err
}

// Both handlers must satisfy the service-layer contract.
var (
	_ service.RematchHandler = (*WhatsAppMethodRematchHandler)(nil)
	_ service.RematchHandler = (*PhoneRematchHandler)(nil)
)

// spec: WHA-042.attach-by-recovered-number
// spec: WHA-042.interactions-appear-without-waiting-for-a-sweep
func TestWhatsAppRematch_AttachesByNormalizedPhoneOnly(t *testing.T) {
	t.Parallel()

	attacher := &fakeCommsAttacher{attached: 3, deduped: 1}
	engine := &fakeContactAggregator{}
	contactID := uuid.New()

	n, err := NewWhatsAppMethodRematchHandler(attacher, engine).Rematch(context.Background(), contactID, "+12045550107")

	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, 1, attacher.calls)
	assert.Equal(t, "whatsapp", attacher.gotSource)
	require.NotNil(t, attacher.gotNormalized)
	assert.Equal(t, "+12045550107", *attacher.gotNormalized)
	assert.Nil(t, attacher.gotHandle, "a contact method carries no peer JID, so peer_handle must stay nil")
	assert.Equal(t, contactID, attacher.gotContactID)
	// Attaching without aggregating would leave the rows invisible until the
	// next 5-minute sweep.
	assert.Equal(t, 1, engine.calls)
	assert.Equal(t, contactID, engine.gotContactID)
}

func TestWhatsAppRematch_EmptyValueIsANoop(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "   "} {
		attacher := &fakeCommsAttacher{attached: 5}
		engine := &fakeContactAggregator{}

		n, err := NewPhoneRematchHandler(attacher, engine).Rematch(context.Background(), uuid.New(), value)

		require.NoError(t, err)
		assert.Equal(t, 0, n)
		assert.Zero(t, attacher.calls, "an empty method value must not run a full-source attach")
		assert.Zero(t, engine.calls)
	}
}

// spec: WHA-042.no-attachment-means-no-aggregation
func TestWhatsAppRematch_ZeroAttachedSkipsAggregation(t *testing.T) {
	t.Parallel()

	attacher := &fakeCommsAttacher{attached: 0}
	engine := &fakeContactAggregator{}

	n, err := NewPhoneRematchHandler(attacher, engine).Rematch(context.Background(), uuid.New(), "+12045550107")

	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, 1, attacher.calls)
	assert.Zero(t, engine.calls)
}

// spec: WHA-042.attach-failure-is-reported
func TestWhatsAppRematch_AttachErrorIsReturned(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	attacher := &fakeCommsAttacher{attached: 4, err: sentinel}
	engine := &fakeContactAggregator{}

	n, err := NewWhatsAppMethodRematchHandler(attacher, engine).Rematch(context.Background(), uuid.New(), "+12045550107")

	require.Error(t, err, "a failure to attach must be reported, not swallowed")
	assert.ErrorIs(t, err, sentinel)
	assert.Equal(t, 0, n)
	assert.Zero(t, engine.calls)
}

func TestWhatsAppRematch_AggregationErrorReturnsTheAttachedCount(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("aggregate exploded")
	attacher := &fakeCommsAttacher{attached: 7}
	engine := &fakeContactAggregator{err: sentinel}

	n, err := NewWhatsAppMethodRematchHandler(attacher, engine).Rematch(context.Background(), uuid.New(), "+12045550107")

	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	// The rows ARE attached; the count is real even when aggregation fails.
	assert.Equal(t, 7, n)
}

func TestWhatsAppRematch_IdentifierTypes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "whatsapp", NewWhatsAppMethodRematchHandler(nil, nil).IdentifierType())
	assert.Equal(t, "phone", NewPhoneRematchHandler(nil, nil).IdentifierType())
}
