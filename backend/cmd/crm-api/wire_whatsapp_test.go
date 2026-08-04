package main

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	wapkg "personal-crm/backend/internal/whatsapp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingWhatsAppManager records the ORDER of the calls the activation
// sequence makes. The ordering is the point: Start is the sole activation point,
// and it must be last.
type recordingWhatsAppManager struct{ calls []string }

func (r *recordingWhatsAppManager) SetIngestor(wapkg.MessageIngestor) {
	r.calls = append(r.calls, "SetIngestor")
}
func (r *recordingWhatsAppManager) SetHistoryRecorder(wapkg.HistoryNotificationRecorder) {
	r.calls = append(r.calls, "SetHistoryRecorder")
}
func (r *recordingWhatsAppManager) SetHistoryDrainReady() {
	r.calls = append(r.calls, "SetHistoryDrainReady")
}
func (r *recordingWhatsAppManager) Start(context.Context) error {
	r.calls = append(r.calls, "Start")
	return nil
}

type stubIngestor struct{}

func (stubIngestor) IngestMessage(context.Context, wapkg.IngestedMessage) error { return nil }

type stubRecorder struct{}

func (stubRecorder) RecordHistoryNotification(context.Context, string, []byte, string, int32, *time.Time, string) error {
	return nil
}

// TestBuildWhatsApp_StartIsCalledAfterEverySetter is the structural guard: a
// prerequisite added AFTER the Start line would be silently installed on a
// manager that had already decided not to connect.
func TestBuildWhatsApp_StartIsCalledAfterEverySetter(t *testing.T) {
	rec := &recordingWhatsAppManager{}
	activateWhatsApp(context.Background(), rec, whatsappPrereqs{
		Ingestor:   stubIngestor{},
		Recorder:   stubRecorder{},
		DrainReady: true,
	})

	require.NotEmpty(t, rec.calls)
	assert.Equal(t, "Start", rec.calls[len(rec.calls)-1],
		"Start is the last call the wiring makes, so a later prerequisite cannot forget to activate")
	assert.Equal(t, []string{"SetIngestor", "SetHistoryRecorder", "SetHistoryDrainReady", "Start"}, rec.calls)
	assert.Equal(t, 1, countCalls(rec.calls, "Start"), "exactly one activation")
}

// TestBuildWhatsApp_ConnectsOnlyWhenAllPrerequisitesArePresent tables the eight
// combinations of the three readiness facts.
//
// It drives the real manager through the real activation sequence. The manager
// is built without a device container, so the all-three row cannot reach a live
// socket — but it leaves not_ready, which is exactly the discrimination the
// table exists to make: seven rows decline to connect and name what is missing;
// one row passes the gate and proceeds.
func TestBuildWhatsApp_ConnectsOnlyWhenAllPrerequisitesArePresent(t *testing.T) {
	cfg := &config.WhatsAppConfig{}

	for _, ingestor := range []bool{false, true} {
		for _, recorder := range []bool{false, true} {
			for _, drain := range []bool{false, true} {
				name := prereqName(ingestor, recorder, drain)
				t.Run(name, func(t *testing.T) {
					m := wapkg.NewManager(nil, wapkg.NewWALogger("whatsapp-test"), cfg, nil, nil)
					t.Cleanup(m.Stop)

					prereqs := whatsappPrereqs{DrainReady: drain}
					if ingestor {
						prereqs.Ingestor = stubIngestor{}
					}
					if recorder {
						prereqs.Recorder = stubRecorder{}
					}
					activateWhatsApp(context.Background(), m, prereqs)

					ready, missing := m.Ready()
					status := m.Status()

					if ingestor && recorder && drain {
						assert.True(t, ready)
						assert.Empty(t, missing)
						assert.NotEqual(t, "not_ready", status.State,
							"the gate passed, so Start proceeded past it")
						return
					}

					assert.False(t, ready)
					assert.Equal(t, "not_ready", status.State)
					assert.Equal(t, wapkg.ReasonIngestNotWired, status.Reason,
						"the machine-readable code stays stable across every missing piece")

					switch {
					case !ingestor:
						assert.Contains(t, status.Missing, "message ingestor")
					case !recorder:
						assert.Contains(t, status.Missing, "history notification recorder")
					default:
						assert.Contains(t, status.Missing, "history drain worker")
					}
				})
			}
		}
	}
}

func prereqName(ingestor, recorder, drain bool) string {
	name := ""
	for _, part := range []struct {
		on    bool
		label string
	}{{ingestor, "ingestor"}, {recorder, "recorder"}, {drain, "drain"}} {
		if part.on {
			if name != "" {
				name += "+"
			}
			name += part.label
		}
	}
	if name == "" {
		return "none"
	}
	return name
}

func countCalls(haystack []string, needle string) int {
	n := 0
	for _, s := range haystack {
		if s == needle {
			n++
		}
	}
	return n
}

// TestWhatsAppWiring_IngestorIsWiredAndDrainerIsNot is the D18 guard: this PR
// genuinely satisfies the ingest prerequisite, and the feature is STILL off.
//
// It drives the real activation sequence with the real ingestor shape, so it
// discriminates between "the ingestor is wired" and "the manager will connect".
func TestWhatsAppWiring_IngestorIsWiredAndDrainerIsNot(t *testing.T) {
	m := wapkg.NewManager(nil, wapkg.NewWALogger("whatsapp-test"), &config.WhatsAppConfig{}, nil, nil)
	t.Cleanup(m.Stop)

	ingestor := wapkg.NewIngestor(nil, wapkg.NewChatGate(nil, 10), nil)
	activateWhatsApp(context.Background(), m, whatsappPrereqs{
		Ingestor: ingestor,
		Recorder: stubRecorder{},
		// DrainReady deliberately false: the drain worker is the next PR's.
	})

	ready, missing := m.Ready()
	assert.False(t, ready, "the feature must still be off")
	assert.Contains(t, missing, "history drain worker",
		"the ingest prerequisite is genuinely satisfied, so the NEXT missing piece must be the drainer")
	assert.NotContains(t, missing, "message ingestor")

	status := m.Status()
	assert.Equal(t, wapkg.StateNotReady, status.State)
	assert.Equal(t, wapkg.ReasonIngestNotWired, status.Reason)
}

// TestWhatsAppWiring_GroupInfoSourceBoundBeforeStart pins the ordering the group
// gate depends on: the seam is bound by SetIngestor, which the activation
// sequence runs before the single Start, so no connected client can reach an
// ingestor whose group-info source is still nil.
func TestWhatsAppWiring_GroupInfoSourceBoundBeforeStart(t *testing.T) {
	m := wapkg.NewManager(nil, wapkg.NewWALogger("whatsapp-test"), &config.WhatsAppConfig{}, nil, nil)
	t.Cleanup(m.Stop)

	binder := &bindOrderIngestor{}
	activateWhatsApp(context.Background(), m, whatsappPrereqs{
		Ingestor:   binder,
		Recorder:   stubRecorder{},
		DrainReady: true,
	})

	assert.True(t, binder.bound, "the group-info source must be bound, not left nil")
}

// bindOrderIngestor records whether the group-info seam was bound.
type bindOrderIngestor struct{ bound bool }

func (b *bindOrderIngestor) IngestMessage(context.Context, wapkg.IngestedMessage) error { return nil }
func (b *bindOrderIngestor) BindGroupInfoSource(func() wapkg.GroupInfoFetcher)          { b.bound = true }
