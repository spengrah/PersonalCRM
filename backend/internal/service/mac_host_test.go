package service

import (
	"context"
	"errors"
	"testing"

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
