package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// stubContactMethodLister implements ContactMethodLister with caller-
// supplied per-type return values. Used by KnownIdentifiers unit tests
// to lock the response-assembly contract without a live DB.
type stubContactMethodLister struct {
	emails    []string
	emailsErr error
	phones    []string
	phonesErr error

	calls []string // recorded as "email" / "phone" in invocation order
}

func (s *stubContactMethodLister) ListCanonicalIdentifiersByType(_ context.Context, types []string) ([]string, error) {
	if len(types) != 1 {
		// The service only calls with a single type — track this so a
		// test catches a regression to a union-style query.
		s.calls = append(s.calls, "unexpected-multi-type")
		return nil, errors.New("stub: unexpected multi-type call")
	}
	s.calls = append(s.calls, types[0])
	switch types[0] {
	case "email":
		return s.emails, s.emailsErr
	case "phone":
		return s.phones, s.phonesErr
	default:
		return nil, errors.New("stub: unexpected type " + types[0])
	}
}

func TestMacHostService_KnownIdentifiers_HappyPath(t *testing.T) {
	stub := &stubContactMethodLister{
		emails: []string{"a@example.com", "b@example.com"},
		phones: []string{"+15550001111", "+15550002222"},
	}
	svc := &MacHostService{contactMethodRepo: stub}
	res, err := svc.KnownIdentifiers(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, []string{"+15550001111", "+15550002222"}, res.Phones)
	require.Equal(t, []string{"a@example.com", "b@example.com"}, res.Emails)
	// Both types must be queried separately.
	require.ElementsMatch(t, []string{"email", "phone"}, stub.calls)
}

func TestMacHostService_KnownIdentifiers_EmptyDB(t *testing.T) {
	stub := &stubContactMethodLister{
		emails: []string{},
		phones: []string{},
	}
	svc := &MacHostService{contactMethodRepo: stub}
	res, err := svc.KnownIdentifiers(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, []string{}, res.Emails)
	require.Equal(t, []string{}, res.Phones)
}

func TestMacHostService_KnownIdentifiers_NilSlicesBecomeEmpty(t *testing.T) {
	// A repo that returns nil (not []string{}) on no rows must still
	// produce JSON-friendly empty arrays so the daemon doesn't have to
	// special-case `null`.
	stub := &stubContactMethodLister{
		emails: nil,
		phones: nil,
	}
	svc := &MacHostService{contactMethodRepo: stub}
	res, err := svc.KnownIdentifiers(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotNil(t, res.Emails, "emails should be empty slice, never nil")
	require.NotNil(t, res.Phones, "phones should be empty slice, never nil")
	require.Len(t, res.Emails, 0)
	require.Len(t, res.Phones, 0)
}

func TestMacHostService_KnownIdentifiers_EmailErrorSurfaces(t *testing.T) {
	stub := &stubContactMethodLister{
		emailsErr: errors.New("db: connection refused"),
		phones:    []string{},
	}
	svc := &MacHostService{contactMethodRepo: stub}
	res, err := svc.KnownIdentifiers(context.Background())
	require.Error(t, err)
	require.Nil(t, res)
	require.Contains(t, err.Error(), "list canonical emails")
}

func TestMacHostService_KnownIdentifiers_PhoneErrorSurfaces(t *testing.T) {
	stub := &stubContactMethodLister{
		emails:    []string{},
		phonesErr: errors.New("db: connection refused"),
	}
	svc := &MacHostService{contactMethodRepo: stub}
	res, err := svc.KnownIdentifiers(context.Background())
	require.Error(t, err)
	require.Nil(t, res)
	require.Contains(t, err.Error(), "list canonical phones")
}

func TestMacHostService_KnownIdentifiers_NilRepoErrors(t *testing.T) {
	// Wiring guard: a service constructed without a contact_method repo
	// returns a clear error instead of panicking when the endpoint is
	// hit. Production main.go always wires it; test fixtures that don't
	// exercise the endpoint pass nil.
	svc := &MacHostService{contactMethodRepo: nil}
	_, err := svc.KnownIdentifiers(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "contact_method repository not wired")
}

// stubExternalContactReader is a recording ExternalContactReader stub
// for KnownIDsForSource unit tests. The pgx.Tx is unused at this
// layer.
type stubExternalContactReader struct {
	resp      []KnownExternalContactID
	err       error
	gotHostID uuid.UUID
	gotSource string
	callCount int
}

func (s *stubExternalContactReader) ListKnownIDsByHostAndSource(
	_ context.Context, hostID uuid.UUID, source string,
) ([]KnownExternalContactID, error) {
	s.callCount++
	s.gotHostID = hostID
	s.gotSource = source
	return s.resp, s.err
}

func TestMacHostService_KnownIDsForSource_HappyPath(t *testing.T) {
	hash1 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	hash2 := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	stub := &stubExternalContactReader{
		resp: []KnownExternalContactID{
			{SourceID: "CN-A", LastContentHash: &hash1},
			{SourceID: "CN-B", LastContentHash: &hash2},
		},
	}
	svc := &MacHostService{externalContactRepo: stub}
	hostID := uuid.New()
	got, err := svc.KnownIDsForSource(context.Background(), hostID, "icloud_contacts")
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "CN-A", got[0].SourceID)
	require.Equal(t, &hash1, got[0].LastContentHash)
	require.Equal(t, "CN-B", got[1].SourceID)
	require.Equal(t, &hash2, got[1].LastContentHash)
	// Service must forward host and source verbatim — no rewriting.
	require.Equal(t, hostID, stub.gotHostID)
	require.Equal(t, "icloud_contacts", stub.gotSource)
}

func TestMacHostService_KnownIDsForSource_EmptyResult(t *testing.T) {
	stub := &stubExternalContactReader{resp: []KnownExternalContactID{}}
	svc := &MacHostService{externalContactRepo: stub}
	got, err := svc.KnownIDsForSource(context.Background(), uuid.New(), "icloud_contacts")
	require.NoError(t, err)
	require.NotNil(t, got, "empty result must be a non-nil slice for JSON marshaling")
	require.Len(t, got, 0)
}

func TestMacHostService_KnownIDsForSource_RepoErrorSurfaces(t *testing.T) {
	stub := &stubExternalContactReader{err: errors.New("db: connection refused")}
	svc := &MacHostService{externalContactRepo: stub}
	_, err := svc.KnownIDsForSource(context.Background(), uuid.New(), "icloud_contacts")
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection refused")
}

func TestMacHostService_KnownIDsForSource_NilRepoErrors(t *testing.T) {
	svc := &MacHostService{externalContactRepo: nil}
	_, err := svc.KnownIDsForSource(context.Background(), uuid.New(), "icloud_contacts")
	require.Error(t, err)
	require.Contains(t, err.Error(), "external_contact repository not wired")
}
