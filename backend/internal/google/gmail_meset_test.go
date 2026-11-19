package google

import (
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestGmailMeSet_UnparsedHeaderIsNeverTheUser is the regression for a header
// entry that does not parse as one mailbox but whose text ends in "@" and an
// own domain: it is never the user's address, so it neither makes a message
// the user's for direction nor anchors discovery trust. Each case's control
// sends the same message with a parsed own-domain address, which does count.
func TestGmailMeSet_UnparsedHeaderIsNeverTheUser(t *testing.T) {
	meSet := buildGmailMeSet([]string{"me@example.com"}, []repository.SyncState{
		{Metadata: map[string]any{"own_domains": []any{"own-domain.example"}}},
	})
	contactID := uuid.New()
	knownMap := map[string][]uuid.UUID{"contact@example.com": {contactID}}

	classify := func(t *testing.T, from string, to []string) []qualifiedRow {
		t.Helper()
		msg := buildMessage(t, "g1", "t1", from, to, nil, nil, "Subj", "body", "<m1@example.com>", 1700000000000)
		rows, err := newProviderForResolution().processMessage(t.Context(), msg, "me@example.com", knownMap, meSet)
		require.NoError(t, err)
		return rows
	}

	t.Run("a From with no parseable mailbox is not the user's outbound mail", func(t *testing.T) {
		// spec: ING-051.unparsed-header-not-own
		require.Empty(t, classify(t, "broken@@own-domain.example", []string{"contact@example.com"}))
		rows := classify(t, "alias@own-domain.example", []string{"contact@example.com"})
		require.Len(t, rows, 1, "control: a parsed own-domain sender is the user")
		require.Equal(t, "outbound", rows[0].Direction)
	})
	t.Run("a From with two mailboxes is not the user's outbound mail", func(t *testing.T) {
		// spec: ING-051.unparsed-header-not-own
		require.Empty(t, classify(t, "stranger@example.net, alias@own-domain.example", []string{"contact@example.com"}))
	})
	t.Run("a recipient with no parseable mailbox does not make a contact's message inbound", func(t *testing.T) {
		// spec: ING-051.unparsed-header-not-own
		require.Empty(t, classify(t, "contact@example.com", []string{"broken@@own-domain.example"}))
		rows := classify(t, "contact@example.com", []string{"alias@own-domain.example"})
		require.Len(t, rows, 1, "control: a parsed own-domain recipient is the user")
		require.Equal(t, "inbound", rows[0].Direction)
	})
	t.Run("a From with no parseable mailbox does not anchor discovery trust", func(t *testing.T) {
		discover := func(t *testing.T, from string) []string {
			t.Helper()
			ext := newFakeExternal()
			d := NewCorrespondenceDiscoverer(&fakeCorrespondenceContacts{}, ext)
			p := newParticipantProvider(d)
			agg := map[string]*correspondenceAggregate{}
			msg := buildMessage(t, "g1", "t1", from, []string{"guest@example.net"}, nil, nil, "Subj", "body", "<m1@example.com>", 1700000000000)
			p.foldDiscovery(msg, nil, emptySet(), meSet, agg)
			_, err := d.EvaluateAddresses(t.Context(), sortedAggregates(agg))
			require.NoError(t, err)
			var proposed []string
			for _, u := range ext.upserts {
				proposed = append(proposed, u.SourceID)
			}
			return proposed
		}
		require.Empty(t, discover(t, "broken@@own-domain.example"))
		require.Equal(t, []string{"guest@example.net"}, discover(t, "alias@own-domain.example"),
			"control: a parsed own-domain sender anchors trust")
	})
}
