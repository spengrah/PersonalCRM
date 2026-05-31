package service

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// stubDiscoveryRepo records the batch-mark calls so tests can assert
// which sibling-marking path fired (or did not fire).
type stubDiscoveryRepo struct {
	groups   []repository.AnarlogTitleGroup
	siblings []repository.ExternalContact

	importedToken    string
	importedContact  uuid.UUID
	matchedToken     string
	matchedContact   uuid.UUID
	ignoredToken     string
	importMarkCalled bool
	matchMarkCalled  bool
	ignoreMarkCalled bool

	findErr error
}

func (s *stubDiscoveryRepo) ListAnarlogTitleGroups(ctx context.Context) ([]repository.AnarlogTitleGroup, error) {
	return s.groups, nil
}
func (s *stubDiscoveryRepo) FindAnarlogTitleSiblingsByToken(ctx context.Context, token string) ([]repository.ExternalContact, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	return s.siblings, nil
}
func (s *stubDiscoveryRepo) MarkAnarlogTitleSiblingsImportedByToken(ctx context.Context, token string, id uuid.UUID) error {
	s.importMarkCalled = true
	s.importedToken = token
	s.importedContact = id
	return nil
}
func (s *stubDiscoveryRepo) MarkAnarlogTitleSiblingsMatchedByToken(ctx context.Context, token string, id uuid.UUID) error {
	s.matchMarkCalled = true
	s.matchedToken = token
	s.matchedContact = id
	return nil
}
func (s *stubDiscoveryRepo) MarkAnarlogTitleSiblingsIgnoredByToken(ctx context.Context, token string) error {
	s.ignoreMarkCalled = true
	s.ignoredToken = token
	return nil
}

// stubDiscoveryContacts records create/update calls and can be primed to
// fail or to return a specific existing contact.
type stubDiscoveryContacts struct {
	existing       *repository.Contact
	getErr         error
	createErr      error
	updateErr      error
	createdReq     *repository.CreateContactRequest
	updateReq      *repository.UpdateContactRequest
	createCalled   bool
	updateCalled   bool
	createdContact *repository.Contact
}

func (s *stubDiscoveryContacts) GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.existing != nil {
		return s.existing, nil
	}
	return &repository.Contact{ID: id, FullName: "Existing"}, nil
}
func (s *stubDiscoveryContacts) CreateContact(ctx context.Context, req repository.CreateContactRequest, methods []ContactMethodInput) (*repository.Contact, uuid.UUID, error) {
	s.createCalled = true
	r := req
	s.createdReq = &r
	if s.createErr != nil {
		return nil, uuid.Nil, s.createErr
	}
	c := &repository.Contact{ID: uuid.New(), FullName: req.FullName, Cadence: req.Cadence}
	s.createdContact = c
	return c, uuid.Nil, nil
}
func (s *stubDiscoveryContacts) UpdateContact(ctx context.Context, id uuid.UUID, req repository.UpdateContactRequest, methods []ContactMethodInput, replaceMethods bool) (*repository.Contact, uuid.UUID, error) {
	s.updateCalled = true
	r := req
	s.updateReq = &r
	if s.updateErr != nil {
		return nil, uuid.Nil, s.updateErr
	}
	return &repository.Contact{ID: id, FullName: req.FullName, Cadence: req.Cadence}, uuid.Nil, nil
}

func strptr(s string) *string { return &s }

func anarlogTitleSibling() repository.ExternalContact {
	dn := "Lena"
	return repository.ExternalContact{
		ID:          uuid.New(),
		Source:      "anarlog_title",
		SourceID:    "deadbeef",
		DisplayName: &dn,
		MatchStatus: repository.MatchStatusUnmatched,
	}
}

func TestResolveToken_UnknownToken(t *testing.T) {
	repo := &stubDiscoveryRepo{siblings: nil}
	svc := NewAnarlogDiscoveryService(repo, &stubDiscoveryContacts{})
	_, err := svc.ResolveToken(context.Background(), ResolveTokenRequest{NormalizedToken: "ghost", Action: DiscoveryActionImport})
	require.ErrorIs(t, err, ErrTokenGroupNotFound)
	require.False(t, repo.importMarkCalled)
}

func TestResolveToken_Ignore(t *testing.T) {
	repo := &stubDiscoveryRepo{siblings: []repository.ExternalContact{anarlogTitleSibling()}}
	contacts := &stubDiscoveryContacts{}
	svc := NewAnarlogDiscoveryService(repo, contacts)
	res, err := svc.ResolveToken(context.Background(), ResolveTokenRequest{NormalizedToken: "lena", Action: DiscoveryActionIgnore})
	require.NoError(t, err)
	require.Equal(t, DiscoveryActionIgnore, res.Action)
	require.Nil(t, res.ContactID)
	require.True(t, repo.ignoreMarkCalled)
	require.Equal(t, "lena", repo.ignoredToken)
	require.False(t, contacts.createCalled)
	require.False(t, contacts.updateCalled)
}

func TestResolveToken_ImportCreatesContactAndMarks(t *testing.T) {
	repo := &stubDiscoveryRepo{siblings: []repository.ExternalContact{anarlogTitleSibling()}}
	contacts := &stubDiscoveryContacts{}
	svc := NewAnarlogDiscoveryService(repo, contacts)
	res, err := svc.ResolveToken(context.Background(), ResolveTokenRequest{
		NormalizedToken: "lena",
		Action:          DiscoveryActionImport,
		Cadence:         strptr("monthly"),
	})
	require.NoError(t, err)
	require.Equal(t, DiscoveryActionImport, res.Action)
	require.NotNil(t, res.ContactID)
	require.True(t, contacts.createCalled)
	require.Equal(t, "Lena", contacts.createdReq.FullName) // defaults to representative display name
	require.NotNil(t, contacts.createdReq.Cadence)
	require.Equal(t, "monthly", *contacts.createdReq.Cadence)
	require.True(t, repo.importMarkCalled)
	require.Equal(t, *res.ContactID, repo.importedContact)
}

func TestResolveToken_ImportNameOverride(t *testing.T) {
	repo := &stubDiscoveryRepo{siblings: []repository.ExternalContact{anarlogTitleSibling()}}
	contacts := &stubDiscoveryContacts{}
	svc := NewAnarlogDiscoveryService(repo, contacts)
	_, err := svc.ResolveToken(context.Background(), ResolveTokenRequest{
		NormalizedToken: "lena",
		Action:          DiscoveryActionImport,
		Name:            strptr("Lena Smith"),
	})
	require.NoError(t, err)
	require.Equal(t, "Lena Smith", contacts.createdReq.FullName)
}

func TestResolveToken_LinkWithNameAndCadencePreservesProfile(t *testing.T) {
	contactID := uuid.New()
	loc := "Berlin"
	how := "conference"
	photo := "https://example/p.png"
	existingCadence := "weekly"
	existing := &repository.Contact{
		ID:           contactID,
		FullName:     "Old Name",
		Location:     &loc,
		HowMet:       &how,
		ProfilePhoto: &photo,
		Cadence:      &existingCadence,
	}
	repo := &stubDiscoveryRepo{siblings: []repository.ExternalContact{anarlogTitleSibling()}}
	contacts := &stubDiscoveryContacts{existing: existing}
	svc := NewAnarlogDiscoveryService(repo, contacts)

	res, err := svc.ResolveToken(context.Background(), ResolveTokenRequest{
		NormalizedToken: "lena",
		Action:          DiscoveryActionLink,
		Name:            strptr("New Name"),
		Cadence:         strptr("monthly"),
		CRMContactID:    &contactID,
	})
	require.NoError(t, err)
	require.Equal(t, DiscoveryActionLink, res.Action)
	require.NotNil(t, res.ContactID)
	require.Equal(t, contactID, *res.ContactID)

	require.True(t, contacts.updateCalled)
	// Name + cadence overlaid; all other profile fields preserved.
	require.Equal(t, "New Name", contacts.updateReq.FullName)
	require.NotNil(t, contacts.updateReq.Cadence)
	require.Equal(t, "monthly", *contacts.updateReq.Cadence)
	require.Equal(t, &loc, contacts.updateReq.Location)
	require.Equal(t, &how, contacts.updateReq.HowMet)
	require.Equal(t, &photo, contacts.updateReq.ProfilePhoto)

	require.True(t, repo.matchMarkCalled)
	require.Equal(t, contactID, repo.matchedContact)
}

func TestResolveToken_LinkNameOnlyCarriesExistingCadence(t *testing.T) {
	contactID := uuid.New()
	existingCadence := "weekly"
	existing := &repository.Contact{ID: contactID, FullName: "Old", Cadence: &existingCadence}
	repo := &stubDiscoveryRepo{siblings: []repository.ExternalContact{anarlogTitleSibling()}}
	contacts := &stubDiscoveryContacts{existing: existing}
	svc := NewAnarlogDiscoveryService(repo, contacts)

	_, err := svc.ResolveToken(context.Background(), ResolveTokenRequest{
		NormalizedToken: "lena",
		Action:          DiscoveryActionLink,
		Name:            strptr("New"),
		CRMContactID:    &contactID,
	})
	require.NoError(t, err)
	require.True(t, contacts.updateCalled)
	require.Equal(t, "New", contacts.updateReq.FullName)
	// Cadence carried forward from the existing contact (no body cadence).
	require.NotNil(t, contacts.updateReq.Cadence)
	require.Equal(t, "weekly", *contacts.updateReq.Cadence)
}

func TestResolveToken_LinkNoEditsSkipsUpdate(t *testing.T) {
	contactID := uuid.New()
	repo := &stubDiscoveryRepo{siblings: []repository.ExternalContact{anarlogTitleSibling()}}
	contacts := &stubDiscoveryContacts{existing: &repository.Contact{ID: contactID, FullName: "Stay"}}
	svc := NewAnarlogDiscoveryService(repo, contacts)

	res, err := svc.ResolveToken(context.Background(), ResolveTokenRequest{
		NormalizedToken: "lena",
		Action:          DiscoveryActionLink,
		CRMContactID:    &contactID,
	})
	require.NoError(t, err)
	require.Equal(t, DiscoveryActionLink, res.Action)
	require.False(t, contacts.updateCalled) // no name/cadence → no write
	require.True(t, repo.matchMarkCalled)
}

func TestResolveToken_LinkMissingContact(t *testing.T) {
	contactID := uuid.New()
	repo := &stubDiscoveryRepo{siblings: []repository.ExternalContact{anarlogTitleSibling()}}
	contacts := &stubDiscoveryContacts{getErr: db.ErrNotFound}
	svc := NewAnarlogDiscoveryService(repo, contacts)

	_, err := svc.ResolveToken(context.Background(), ResolveTokenRequest{
		NormalizedToken: "lena",
		Action:          DiscoveryActionLink,
		CRMContactID:    &contactID,
	})
	require.ErrorIs(t, err, ErrDiscoveryContactMissing)
	require.False(t, repo.matchMarkCalled) // FK miss → no sibling mark
}

func TestResolveToken_LinkUpdateFailureLeavesSiblingsUnmarked(t *testing.T) {
	contactID := uuid.New()
	repo := &stubDiscoveryRepo{siblings: []repository.ExternalContact{anarlogTitleSibling()}}
	contacts := &stubDiscoveryContacts{
		existing:  &repository.Contact{ID: contactID, FullName: "Old"},
		updateErr: errors.New("cadence write failed"),
	}
	svc := NewAnarlogDiscoveryService(repo, contacts)

	_, err := svc.ResolveToken(context.Background(), ResolveTokenRequest{
		NormalizedToken: "lena",
		Action:          DiscoveryActionLink,
		Cadence:         strptr("monthly"),
		CRMContactID:    &contactID,
	})
	require.Error(t, err)
	require.False(t, repo.matchMarkCalled) // update failed → no silent edit loss
}

func TestListGroups_Passthrough(t *testing.T) {
	repo := &stubDiscoveryRepo{groups: []repository.AnarlogTitleGroup{
		{NormalizedToken: "lena", TokenDisplay: "Lena", EvidenceCount: 3, SessionTitles: []string{"Design sync"}},
		{NormalizedToken: "ravi", TokenDisplay: "Ravi", EvidenceCount: 1, SessionTitles: nil},
	}}
	svc := NewAnarlogDiscoveryService(repo, &stubDiscoveryContacts{})
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 2)
	require.Equal(t, "lena", out[0].NormalizedToken)
	require.Equal(t, int64(3), out[0].EvidenceCount)
	require.Equal(t, []string{"Design sync"}, out[0].SessionTitles)
	// nil titles normalize to an empty (non-nil) slice for JSON.
	require.NotNil(t, out[1].SessionTitles)
	require.Empty(t, out[1].SessionTitles)
}
