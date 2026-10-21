//go:build integration_testdb

package threadctx_test

import (
	"math/rand/v2"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// maskElsewhere stores text as a contact's message in an email thread of its
// own, and returns it masked by a masker loaded from the CRM after every
// store: the masked text of a thread the display names' owners take no part
// in.
func (e *builderEnv) maskElsewhere(t *testing.T, text string) string {
	t.Helper()
	gmailThread := "thread-" + uuid.NewString()
	thread := e.venue(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, gmailThread)
	e.store(t, commsCopy{source: repository.InteractionSourceEmail, thread: gmailThread, external: uuid.NewString(), direction: repository.InteractionDirectionInbound, peer: "elsewhere@example.test", body: text, contact: e.contact(t), at: e.at(2)})
	masker, err := threadctx.LoadMasker(e.ctx, repository.NewThreadContentRepository(e.database.Queries), own)
	require.NoError(t, err)
	c := e.build(t, thread, e.at(3))
	require.Contains(t, c.Participants, threadctx.RoleThem)
	masked, err := masker.Mask(e.ctx, c, nil)
	require.NoError(t, err)
	return masked.Entries()[0].Text
}

// inboundEmail stores an email from a contact, in a thread of its own, with
// the given source metadata.
func (e *builderEnv) inboundEmail(t *testing.T, metadata map[string]any) {
	t.Helper()
	e.store(t, commsCopy{source: repository.InteractionSourceEmail, thread: "thread-" + uuid.NewString(), external: uuid.NewString(), direction: repository.InteractionDirectionInbound, peer: "sender@example.test", body: "hello", contact: e.contact(t), at: e.at(1), metadata: metadata})
}

// A display name a stored message gives a sender or a recipient, of any source
// that stores one, masks the same name in the messages of another thread,
// where its owner takes no part, as a numbered placeholder. The name begins
// the text, where only the dictionary masks a name.
// spec: KNW-089.display-names-masked
func TestLoadMasker_MasksDisplayNames(t *testing.T) {
	t.Parallel()
	const text, want = "Ottoline sent the minutes", "NAME_1 sent the minutes"

	senders := []struct {
		name  string
		store func(t *testing.T, e *builderEnv)
	}{
		{"an email sender's name", func(t *testing.T, e *builderEnv) {
			e.inboundEmail(t, map[string]any{"from": "sender@example.test", "from_name": "Ottoline"})
		}},
		{"a WhatsApp sender's push name", func(t *testing.T, e *builderEnv) {
			e.store(t, commsCopy{source: repository.InteractionSourceWhatsApp, thread: uuid.NewString() + "@s.whatsapp.net", external: uuid.NewString(), direction: repository.InteractionDirectionInbound, peer: "15550100123@s.whatsapp.net", body: "hello", contact: e.contact(t), at: e.at(1), metadata: map[string]any{"push_name": "Ottoline"}})
		}},
		{"a Telegram peer's name, the message attributed to no contact", func(t *testing.T, e *builderEnv) {
			peer := rand.Int64N(1 << 40)
			e.telegram(t, peer, "private", 1, peer, "Ottoline", nil, e.at(1))
		}},
	}
	for _, tc := range senders {
		t.Run(tc.name+", from a sender field", func(t *testing.T) {
			t.Parallel()
			e := newBuilderEnv(t)
			tc.store(t, e)
			assert.Equal(t, want, e.maskElsewhere(t, text))
		})
	}

	for _, field := range []string{"to", "cc", "bcc"} {
		t.Run("an email recipient's name in "+field+", attributed to no contact, from a recipient field", func(t *testing.T) {
			t.Parallel()
			e := newBuilderEnv(t)
			e.inboundEmail(t, map[string]any{"from": "sender@example.test", field: []string{"me@example.test", "o@example.test"}, field + "_names": []string{"Marlowe Quill", "Ottoline"}})
			assert.Equal(t, want, e.maskElsewhere(t, text))
		})
	}
}
