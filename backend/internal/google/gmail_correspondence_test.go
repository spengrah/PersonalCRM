package google

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// --- fakes ---

type fakeCorrespondenceComms struct {
	identities []repository.EmailIdentity
	rows       []repository.CommsMessageParticipantRow
}

func (f *fakeCorrespondenceComms) ListEmailIdentitiesForSync(_ context.Context) ([]repository.EmailIdentity, error) {
	return f.identities, nil
}

func (f *fakeCorrespondenceComms) ListParticipantsSince(_ context.Context, _ time.Time) ([]repository.CommsMessageParticipantRow, error) {
	return f.rows, nil
}

type fakeCorrespondenceContacts struct {
	// matches keyed by exact search name → matches returned (already sorted).
	matches map[string][]repository.ContactMatch
	names   map[uuid.UUID]string
}

func (f *fakeCorrespondenceContacts) FindSimilarContacts(_ context.Context, name string, _ float64, _ int32) ([]repository.ContactMatch, error) {
	return f.matches[name], nil
}

func (f *fakeCorrespondenceContacts) GetContact(_ context.Context, id uuid.UUID) (*repository.Contact, error) {
	if n, ok := f.names[id]; ok {
		return &repository.Contact{ID: id, FullName: n}, nil
	}
	return nil, nil
}

// fakeCorrespondenceExternal records upserts and serves a seeded sticky-ignore
// row. Keyed by source_id (the normalized address).
type fakeCorrespondenceExternal struct {
	existing map[string]*repository.ExternalContact
	upserts  []repository.UpsertExternalContactRequest
}

func newFakeExternal() *fakeCorrespondenceExternal {
	return &fakeCorrespondenceExternal{existing: map[string]*repository.ExternalContact{}}
}

func (f *fakeCorrespondenceExternal) GetBySource(_ context.Context, _, sourceID string, _ *string) (*repository.ExternalContact, error) {
	return f.existing[sourceID], nil
}

func (f *fakeCorrespondenceExternal) Upsert(_ context.Context, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error) {
	f.upserts = append(f.upserts, req)
	row := &repository.ExternalContact{
		Source:      req.Source,
		SourceID:    req.SourceID,
		MatchStatus: repository.MatchStatusUnmatched,
	}
	// Preserve a pre-existing status (mirrors the real upsert's DO UPDATE SET
	// which never touches match_status).
	if prior := f.existing[req.SourceID]; prior != nil {
		row.MatchStatus = prior.MatchStatus
	}
	f.existing[req.SourceID] = row
	return row, nil
}

// --- helpers ---

func metaJSON(t *testing.T, m correspondenceMetadata) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

func newSuggester(comms *fakeCorrespondenceComms, contacts *fakeCorrespondenceContacts, ext *fakeCorrespondenceExternal, own map[string]struct{}) *GmailCorrespondenceSuggester {
	return NewGmailCorrespondenceSuggester(comms, contacts, ext, func(context.Context) (map[string]struct{}, error) {
		return own, nil
	})
}

func contactMatch(id uuid.UUID, name string, sim float64) repository.ContactMatch {
	return repository.ContactMatch{Contact: repository.Contact{ID: id, FullName: name}, Similarity: sim}
}

// --- tests ---

func TestCorrespondence_GateQualifiesAtThreshold(t *testing.T) {
	contactID := uuid.New()
	comms := &fakeCorrespondenceComms{
		rows: []repository.CommsMessageParticipantRow{{
			MatchedContactID: contactID,
			SourceMetadata: metaJSON(t, correspondenceMetadata{
				From: "me@example.com", FromName: "Me",
				To: []string{"unknown@example.com"}, ToNames: []string{"Full Name"},
			}),
		}},
	}
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Full Name": {contactMatch(contactID, "Full Name", 0.72)},
		},
		names: map[uuid.UUID]string{contactID: "Full Name"},
	}
	ext := newFakeExternal()
	s := newSuggester(comms, contacts, ext, map[string]struct{}{"me@example.com": {}})

	n, err := s.Run(context.Background(), accelerated.GetCurrentTime().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ext.upserts, 1)
	require.Equal(t, "unknown@example.com", ext.upserts[0].SourceID)
	require.Equal(t, CorrespondenceSource, ext.upserts[0].Source)
}

// Boundary case: an EXACT 0.60 match must qualify (the floor-minus-epsilon +
// Go `>=` re-check honors `>= 0.60`, not the SQL's strict `>`).
func TestCorrespondence_GateExactBoundary(t *testing.T) {
	contactID := uuid.New()
	comms := &fakeCorrespondenceComms{
		rows: []repository.CommsMessageParticipantRow{{
			MatchedContactID: contactID,
			SourceMetadata: metaJSON(t, correspondenceMetadata{
				From: "unknown@example.com", FromName: "Exact Match",
				To: []string{"me@example.com"}, ToNames: []string{"Me"},
			}),
		}},
	}
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Exact Match": {contactMatch(contactID, "Exact Match", 0.60)},
		},
		names: map[uuid.UUID]string{contactID: "Exact Match"},
	}
	ext := newFakeExternal()
	s := newSuggester(comms, contacts, ext, map[string]struct{}{"me@example.com": {}})

	n, err := s.Run(context.Background(), accelerated.GetCurrentTime().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n, "exact-0.60 similarity must qualify")
}

func TestCorrespondence_GateRejectsBelowThreshold(t *testing.T) {
	contactID := uuid.New()
	comms := &fakeCorrespondenceComms{
		rows: []repository.CommsMessageParticipantRow{{
			MatchedContactID: contactID,
			SourceMetadata: metaJSON(t, correspondenceMetadata{
				From: "unknown@example.com", FromName: "Weak Match",
				To: []string{"me@example.com"},
			}),
		}},
	}
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Weak Match": {contactMatch(contactID, "Weak Match", 0.59)},
		},
	}
	ext := newFakeExternal()
	s := newSuggester(comms, contacts, ext, map[string]struct{}{"me@example.com": {}})

	n, err := s.Run(context.Background(), accelerated.GetCurrentTime().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, n, "sub-0.60 must be rejected")
	require.Empty(t, ext.upserts)
}

func TestCorrespondence_GateRejectsBareFirstName(t *testing.T) {
	contactID := uuid.New()
	comms := &fakeCorrespondenceComms{
		rows: []repository.CommsMessageParticipantRow{{
			MatchedContactID: contactID,
			SourceMetadata: metaJSON(t, correspondenceMetadata{
				From: "unknown@example.com", FromName: "Jane",
				To: []string{"me@example.com"},
			}),
		}},
	}
	// Even a high similarity must not save a single-token name.
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Jane": {contactMatch(contactID, "Jane", 0.99)},
		},
	}
	ext := newFakeExternal()
	s := newSuggester(comms, contacts, ext, map[string]struct{}{"me@example.com": {}})

	n, err := s.Run(context.Background(), accelerated.GetCurrentTime().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, n, "bare first name must be rejected by the ≥2-token gate")
	require.Empty(t, ext.upserts)
}

func TestCorrespondence_ParticipantExtractionAndNamelessSkip(t *testing.T) {
	contactID := uuid.New()
	comms := &fakeCorrespondenceComms{
		rows: []repository.CommsMessageParticipantRow{
			// Row WITH name fields → yields a candidate.
			{
				MatchedContactID: contactID,
				SourceMetadata: metaJSON(t, correspondenceMetadata{
					From: "me@example.com", FromName: "Me",
					To: []string{"named@example.com"}, ToNames: []string{"Named Person"},
				}),
			},
			// Row with NO name fields (pre-capture shape) → no display name →
			// participant skipped at the token gate.
			{
				MatchedContactID: contactID,
				SourceMetadata: metaJSON(t, correspondenceMetadata{
					From: "me@example.com",
					To:   []string{"nameless@example.com"},
				}),
			},
		},
	}
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Named Person": {contactMatch(contactID, "Named Person", 0.8)},
		},
		names: map[uuid.UUID]string{contactID: "Named Person"},
	}
	ext := newFakeExternal()
	s := newSuggester(comms, contacts, ext, map[string]struct{}{"me@example.com": {}})

	n, err := s.Run(context.Background(), accelerated.GetCurrentTime().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ext.upserts, 1)
	require.Equal(t, "named@example.com", ext.upserts[0].SourceID)
}

func TestCorrespondence_DedupByAddress(t *testing.T) {
	contactID := uuid.New()
	rowFor := func(name string) repository.CommsMessageParticipantRow {
		return repository.CommsMessageParticipantRow{
			MatchedContactID: contactID,
			SourceMetadata: metaJSON(t, correspondenceMetadata{
				From: "me@example.com", FromName: "Me",
				To: []string{"dup@example.com"}, ToNames: []string{name},
			}),
		}
	}
	comms := &fakeCorrespondenceComms{rows: []repository.CommsMessageParticipantRow{
		rowFor("Full Name"),
		rowFor("Full Name"),
		rowFor("Fuller Longer Name"),
	}}
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			// bestDisplayName picks the most-token name.
			"Fuller Longer Name": {contactMatch(contactID, "Fuller Longer Name", 0.7)},
		},
		names: map[uuid.UUID]string{contactID: "Fuller Longer Name"},
	}
	ext := newFakeExternal()
	s := newSuggester(comms, contacts, ext, map[string]struct{}{"me@example.com": {}})

	n, err := s.Run(context.Background(), accelerated.GetCurrentTime().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n, "three rows for one address → one candidate")
	require.Len(t, ext.upserts, 1)

	var meta struct {
		DisplayNamesSeen []string `json:"display_names_seen"`
		MessageCount     int      `json:"message_count"`
	}
	b, err := json.Marshal(ext.upserts[0].Metadata)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &meta))
	require.Equal(t, 3, meta.MessageCount, "message_count sums across rows")
	require.ElementsMatch(t, []string{"Full Name", "Fuller Longer Name"}, meta.DisplayNamesSeen)
}

func TestCorrespondence_SkipKnownAndOwn(t *testing.T) {
	contactID := uuid.New()
	comms := &fakeCorrespondenceComms{
		identities: []repository.EmailIdentity{{ValueNormalized: "known@example.com", ContactID: contactID}},
		rows: []repository.CommsMessageParticipantRow{{
			MatchedContactID: contactID,
			SourceMetadata: metaJSON(t, correspondenceMetadata{
				From: "me@example.com", FromName: "Me",
				To:      []string{"known@example.com", "own2@example.com"},
				ToNames: []string{"Known Person", "Own Two"},
			}),
		}},
	}
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Known Person": {contactMatch(contactID, "Known Person", 0.9)},
			"Own Two":      {contactMatch(contactID, "Own Two", 0.9)},
		},
	}
	ext := newFakeExternal()
	s := newSuggester(comms, contacts, ext, map[string]struct{}{
		"me@example.com":   {},
		"own2@example.com": {},
	})

	n, err := s.Run(context.Background(), accelerated.GetCurrentTime().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, n, "known and own-account addresses are never emitted")
	require.Empty(t, ext.upserts)
}

// No-clobber invariant: an existing ignored row stays ignored, the producer
// skips the upsert (write-avoidance), and the address does not become unmatched.
func TestCorrespondence_StickyIgnoreNoClobber(t *testing.T) {
	contactID := uuid.New()
	comms := &fakeCorrespondenceComms{
		rows: []repository.CommsMessageParticipantRow{{
			MatchedContactID: contactID,
			SourceMetadata: metaJSON(t, correspondenceMetadata{
				From: "ignored@example.com", FromName: "Ignored Person",
				To: []string{"me@example.com"},
			}),
		}},
	}
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Ignored Person": {contactMatch(contactID, "Ignored Person", 0.9)},
		},
	}
	ext := newFakeExternal()
	ext.existing["ignored@example.com"] = &repository.ExternalContact{
		Source:      CorrespondenceSource,
		SourceID:    "ignored@example.com",
		MatchStatus: repository.MatchStatusIgnored,
	}
	s := newSuggester(comms, contacts, ext, map[string]struct{}{"me@example.com": {}})

	n, err := s.Run(context.Background(), accelerated.GetCurrentTime().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Empty(t, ext.upserts, "ignored row → no redundant write")
	require.Equal(t, repository.MatchStatusIgnored, ext.existing["ignored@example.com"].MatchStatus)
}

func TestCorrespondence_CoOccurringContactEvidence(t *testing.T) {
	coContact := uuid.New()
	comms := &fakeCorrespondenceComms{
		rows: []repository.CommsMessageParticipantRow{{
			MatchedContactID: coContact,
			SourceMetadata: metaJSON(t, correspondenceMetadata{
				From: "me@example.com", FromName: "Me",
				To: []string{"alt@example.com"}, ToNames: []string{"Alt Person"},
			}),
		}},
	}
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Alt Person": {contactMatch(coContact, "Alt Person", 0.8)},
		},
		names: map[uuid.UUID]string{coContact: "Alt Person"},
	}
	ext := newFakeExternal()
	s := newSuggester(comms, contacts, ext, map[string]struct{}{"me@example.com": {}})

	_, err := s.Run(context.Background(), accelerated.GetCurrentTime().Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, ext.upserts, 1)

	var meta struct {
		CoOccurring struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"co_occurring_contact"`
	}
	b, err := json.Marshal(ext.upserts[0].Metadata)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &meta))
	require.Equal(t, coContact.String(), meta.CoOccurring.ID)
	require.Equal(t, "Alt Person", meta.CoOccurring.Name)
}

func TestCorrespondence_EmptyInputNoError(t *testing.T) {
	comms := &fakeCorrespondenceComms{}
	contacts := &fakeCorrespondenceContacts{}
	ext := newFakeExternal()
	s := newSuggester(comms, contacts, ext, map[string]struct{}{})

	n, err := s.Run(context.Background(), accelerated.GetCurrentTime().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestBestDisplayName(t *testing.T) {
	require.Equal(t, "", bestDisplayName(nil))
	require.Equal(t, "Two Tokens", bestDisplayName([]string{"One", "Two Tokens"}))
	require.Equal(t, "Longer Full Name", bestDisplayName([]string{"Full Name", "Longer Full Name"}))
}

func TestTokenCount(t *testing.T) {
	require.Equal(t, 0, tokenCount(""))
	require.Equal(t, 1, tokenCount("Jane"))
	require.Equal(t, 2, tokenCount("Jane Doe"))
	require.Equal(t, 2, tokenCount("  Jane   Doe  "))
}
