//go:build integration_testdb

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storedText is what a comms_message row stores of an email's text.
type storedText struct {
	body          string
	quotedHistory *string
}

// crm-admin --trim-email-quotes re-derives each stored email by the ingest's
// rule, against the test's own database clone: rows stored whole before the
// rule are split or capped, rows the rule already holds stay as they are.
// spec: ING-050.thread-start-split, ING-050.forward-capped, ING-050.trimmed-reply-unchanged, ING-050.second-run-changes-nothing, ING-050.counts-only
func TestTrimEmailQuotes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	h := synthetic.NewHarnessForNamespace(t, ctx, "qt"+uuid.NewString()[:8], factory.DefaultSeed)
	contact, err := h.SeedContact(ctx, h.Generator().Contact(factory.WithEmail()))
	require.NoError(t, err)
	comms := repository.NewCommsMessageRepository(h.Database().Queries)
	account := "me@example.com"

	// store writes a row the way an earlier ingest did, the html under the
	// source metadata as ingest keeps it.
	store := func(gmailID, threadID, subject, body, html string) uuid.UUID {
		t.Helper()
		metadata, err := json.Marshal(map[string]string{"html": html})
		require.NoError(t, err)
		msg, err := comms.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
			Source:           repository.InteractionSourceEmail,
			ExternalID:       "<" + uuid.NewString() + "@example.com>",
			ThreadID:         &threadID,
			Subject:          &subject,
			Body:             &body,
			Direction:        "inbound",
			SentAt:           h.Generator().Anchor(),
			AccountID:        &account,
			SourceMetadata:   metadata,
			MatchedContactID: contact.ID,
			GmailMessageID:   &gmailID,
		})
		require.NoError(t, err)
		return msg.ID
	}
	textOf := func(id uuid.UUID) storedText {
		t.Helper()
		msg, err := comms.GetByID(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, msg.Body)
		return storedText{body: *msg.Body, quotedHistory: msg.QuotedHistory}
	}
	trim := func() string {
		t.Helper()
		var stdout bytes.Buffer
		require.NoError(t, run(ctx, runOptions{trimEmailQuotes: true}, adminDeps{
			quoteTrim: google.NewEmailQuoteTrimService(comms),
			stdout:    &stdout,
			stderr:    &bytes.Buffer{},
		}))
		return stdout.String()
	}
	strPtr := func(s string) *string { return &s }

	const own = "Introducing you two."
	const quote = "On Mon, Contact A wrote:\n> Know anyone?"
	html := `<div>Introducing you two.</div><blockquote><div>Know anyone?</div><div>Anyone at all?</div></blockquote>`
	// A forward whose content runs past the cap: a 100-character header, then
	// lines of 100 characters each with their breaks, so the 10,000th
	// character starts its line 99.
	header := "---------- Forwarded message ---------\nSubject: " + strings.Repeat("s", 51) + "\n"
	lines := make([]string, 120)
	for i := range lines {
		lines[i] = fmt.Sprintf("%03d ", i) + strings.Repeat("c", 95)
	}
	forwardBody := "FYI\n\n" + header + strings.Join(lines, "\n")

	starter := store("g1", "g1", "Intro", own+"\n\n"+quote+"\n", "")
	htmlStarter := store("g2", "g2", "Intro", "Introducing you two. Know anyone? Anyone at all?", html)
	forward := store("g3", "g3", "Fwd: Plan", forwardBody, "")
	trimmedReply := store("g5", "g1", "Re: Intro", "See you then.", "")
	untrimmedReply := store("g6", "g1", "Re: Intro", "See you then.\n\n"+quote+"\n", "")

	want := map[uuid.UUID]storedText{
		starter:        {body: own, quotedHistory: strPtr(quote)},
		htmlStarter:    {body: own, quotedHistory: strPtr("Know anyone?\nAnyone at all?")},
		forward:        {body: "FYI\n\n" + header + strings.Join(lines[:99], "\n") + "\n" + google.QuoteTrimmedMarker},
		trimmedReply:   {body: "See you then."},
		untrimmedReply: {body: "See you then."},
	}

	// The command scans every email row of the clone, so the first run's
	// assertions read this test's rows rather than its counts.
	first := trim()
	for id, text := range want {
		assert.Equal(t, text, textOf(id))
	}
	assert.Regexp(t, `^trim-email-quotes summary:\n  scanned: \d+\n  trimmed: \d+\n$`, first, "the output holds counts only")

	second := trim()
	for id, text := range want {
		assert.Equal(t, text, textOf(id), "a second run changes nothing")
	}
	assert.Regexp(t, `\n  trimmed: 0\n$`, second, "a second run rewrites no row")
}
