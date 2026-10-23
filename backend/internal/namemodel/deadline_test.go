package namemodel_test

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/namemodel"
	"personal-crm/backend/internal/threadctx"
)

// Abandoning a request leaves the next request answerable within its own
// deadline, though the server was still reading the abandoned one.
// Cancellation starts only after inference begins.
// spec: KNW-090.abandoned-request-stops
func TestClientAfterAbandonedRequestSucceeds(t *testing.T) {
	t.Parallel()
	server := startSlowNameModel(t)
	client := server.client(t, 500*time.Millisecond)
	model := threadctx.NewModelVersion(0.5)
	short := "Testperson"
	spans, err := client.Spans(t.Context(), model, []string{short})
	if err != nil {
		t.Fatalf("short-text control failed: %v", err)
	}
	assertTailSpan(t, short, spans)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	abandoned := make(chan error, 1)
	patient := server.client(t, 15*time.Second)
	go func() {
		_, err := patient.Spans(ctx, model, []string{"begin " + strings.Repeat("word ", 40800) + "Testperson"})
		abandoned <- err
	}()
	server.event(t, "started")
	cancel()
	if err := <-abandoned; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned request: got %v, want caller cancellation", err)
	}
	t.Log("caller canceled the long request after inference started")

	start := accelerated.GetCurrentTime()
	deadline, stop := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer stop()
	spans, nextErr := client.Spans(deadline, model, []string{short})
	t.Logf("next short request: elapsed=%s error=%v", time.Since(start).Round(time.Millisecond), nextErr)
	if nextErr == nil {
		assertTailSpan(t, short, spans)
	}

	// A recovery control distinguishes head-of-line blocking from a dead
	// server or a short request that the endpoint cannot answer.
	spans, err = patient.Spans(t.Context(), model, []string{short})
	if err != nil {
		t.Fatalf("short-text recovery control failed: %v", err)
	}
	assertTailSpan(t, short, spans)
	t.Logf("short request recovered after %s", time.Since(start).Round(time.Millisecond))
	if nextErr != nil {
		t.Fatalf("request after an abandoned request must meet its own deadline: %v", nextErr)
	}
}

type slowNameModel struct {
	socket string
	events <-chan string
}

func startSlowNameModel(t *testing.T) *slowNameModel {
	t.Helper()
	// macOS limits Unix socket paths; t.TempDir's full test name is too long.
	dir, err := os.MkdirTemp("", "nm-deadline-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	fixture, err := filepath.Abs("../../../name-model/tests/slow_server.py")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-B", fixture, socket)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	events := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(events)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			events <- scanner.Text()
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		<-done
	})
	server := &slowNameModel{socket: socket, events: events}
	server.event(t, "ready")
	return server
}

func (s *slowNameModel) event(t *testing.T, want string) {
	t.Helper()
	select {
	case got, ok := <-s.events:
		if !ok || got != want {
			t.Fatalf("server event: got %q, want %q (open=%v)", got, want, ok)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("server did not report %q", want)
	}
}

func (s *slowNameModel) client(t *testing.T, deadline time.Duration) *namemodel.Client {
	t.Helper()
	client, err := namemodel.New(s.socket, namemodel.Deadline{PerText: deadline})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func assertTailSpan(t *testing.T, text string, got [][]threadctx.NameSpan) {
	t.Helper()
	want := [][]threadctx.NameSpan{{{Start: len(text) - len("Testperson"), End: len(text)}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tail name spans: got %v, want %v", got, want)
	}
}
