package whatsapp

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow"
)

// forbiddenCallSites are the whatsmeow calls this integration must never make.
// Sending a message, marking a chat read, signalling presence or requesting
// history are visible to the user's contacts and put the account at risk;
// downloading media bytes is a scope violation.
//
// These are CALL-SITE regexes — leading dot, trailing paren — never bare
// identifier tokens. A bare `Download` would also match the PERMITTED
// DownloadHistorySync(, which the history path requires.
var forbiddenCallSites = []string{
	`\.SendMessage\(`,
	`\.SendPresence\(`,
	`\.SendChatPresence\(`,
	`\.MarkRead\(`,
	`\.SendHistorySyncServerErrorReceipt\(`,
	`\.BuildHistorySyncRequest\(`,
	`\.Download\(`,
	`\.DownloadAny\(`,
}

// TestManagerNeverCallsOutboundAPI scans this package for the forbidden calls.
//
// This is the one permanent gate the WhatsApp client adds. It prevents a
// concrete failure no type can catch: an outbound call that would be visible to
// a conversation partner or would get the account banned. Nothing in the type
// system distinguishes SendMessage from DownloadHistorySync.
func TestManagerNeverCallsOutboundAPI(t *testing.T) {
	sources := packageGoSources(t)
	require.NotEmpty(t, sources, "the scan must actually read files, or it can never fail")

	for _, pattern := range forbiddenCallSites {
		re := regexp.MustCompile(pattern)
		for path, content := range sources {
			for i, line := range strings.Split(content, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue // the regexes are quoted in this file's own comments
				}
				assert.False(t, re.MatchString(line),
					"forbidden whatsmeow call %s at %s:%d — the integration is read-only", pattern, filepath.Base(path), i+1)
			}
		}
	}
}

// TestOutboundGuard_MatchesTheCallItIsMeantTo is the guard's own falsification:
// it proves each regex fires on the call it forbids, and — the case that
// actually bites — that the Download regex does NOT flag the permitted
// DownloadHistorySync.
func TestOutboundGuard_MatchesTheCallItIsMeantTo(t *testing.T) {
	positives := map[string]string{
		`\.SendMessage\(`:                       "cli.SendMessage(ctx, jid, msg)",
		`\.SendPresence\(`:                      "cli.SendPresence(types.PresenceAvailable)",
		`\.SendChatPresence\(`:                  "cli.SendChatPresence(jid, a, b)",
		`\.MarkRead\(`:                          "cli.MarkRead(ids, ts, chat, sender)",
		`\.SendHistorySyncServerErrorReceipt\(`: "cli.SendHistorySyncServerErrorReceipt(ctx, id)",
		`\.BuildHistorySyncRequest\(`:           "cli.BuildHistorySyncRequest(info, 50)",
		`\.Download\(`:                          "cli.Download(ctx, msg)",
		`\.DownloadAny\(`:                       "cli.DownloadAny(ctx, msg)",
	}
	for pattern, call := range positives {
		assert.Regexp(t, pattern, call, "regex %s must match the call it forbids", pattern)
	}

	permitted := []string{
		"cli.DownloadHistorySync(ctx, notif, true)",
		"cli.SendProtocolMessageReceipt(ctx, id, types.ReceiptTypeHistorySync)",
		"cli.DeleteMedia(ctx, whatsmeow.MediaHistory, path, hash, handle)",
		"cli.AddEventHandlerWithSuccessStatus(m.handleEvent)",
		// The history projection's decode. It is documented here rather than
		// gated: it is a pure local decode with no I/O, and no mutation of THIS
		// PR could make any of the regexes above match it.
		"cli.ParseWebMessage(jid, webMsg)",
	}
	for _, pattern := range forbiddenCallSites {
		re := regexp.MustCompile(pattern)
		for _, call := range permitted {
			assert.False(t, re.MatchString(call),
				"regex %s must not flag the permitted call %q", pattern, call)
		}
	}
}

// TestNewClient_UsesSuccessStatusHandlerRegistration is the source-level half
// of the withheld-ack contract. AddEventHandler wraps a void handler and
// hard-codes a true return, so registering through it would silently discard
// every false this package produces.
func TestNewClient_UsesSuccessStatusHandlerRegistration(t *testing.T) {
	sources := packageGoSources(t)

	var registrations int
	plainRegistration := regexp.MustCompile(`\.AddEventHandler\(`)
	successRegistration := regexp.MustCompile(`\.AddEventHandlerWithSuccessStatus\(`)

	for path, content := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		for i, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			assert.False(t, plainRegistration.MatchString(line),
				"AddEventHandler swallows the handler's false return; use AddEventHandlerWithSuccessStatus (%s:%d)", filepath.Base(path), i+1)
			if successRegistration.MatchString(line) {
				registrations++
			}
		}
	}
	assert.Equal(t, 1, registrations, "exactly one handler registration, in newClient")

	// And the handler really has the signature the success-status variant
	// requires: a void handler would not compile here.
	var _ whatsmeow.EventHandlerWithSuccessStatus = (&Manager{}).handleEvent
}

// packageGoSources reads every .go file in this package.
func packageGoSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	out := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		// This file quotes the forbidden calls as test fixtures.
		if e.Name() == "outbound_guard_test.go" {
			continue
		}
		content, err := os.ReadFile(e.Name())
		require.NoError(t, err)
		out[e.Name()] = string(content)
	}
	return out
}
