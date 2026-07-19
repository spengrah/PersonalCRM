package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// methodOpsFixture is one contact plus its method repository, scoped to this
// test's own namespace so parallel copies never see each other's rows.
type methodOpsFixture struct {
	ctx        context.Context
	database   *db.Database
	contactID  uuid.UUID
	methodRepo *repository.ContactMethodRepository
	svc        *service.ContactMethodService
}

func newMethodOpsFixture(t *testing.T) *methodOpsFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	t.Cleanup(database.Close)

	contactRepo := repository.NewContactRepository(database.Queries)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "MethodOps " + syntheticNS(t) + " " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, contact.ID) })

	return &methodOpsFixture{
		ctx:        ctx,
		database:   database,
		contactID:  contact.ID,
		methodRepo: repository.NewContactMethodRepository(database.Queries),
		// bus and rematch registry are nil: these tests pin the fold and apply
		// stages, not the event contract.
		svc: service.NewContactMethodService(database, nil, nil),
	}
}

func (f *methodOpsFixture) seed(t *testing.T, methodType, value string, primary bool) repository.ContactMethod {
	t.Helper()
	m, err := f.methodRepo.CreateContactMethod(f.ctx, repository.CreateContactMethodRequest{
		ContactID: f.contactID,
		Type:      methodType,
		Value:     value,
		IsPrimary: primary,
	})
	require.NoError(t, err)
	return *m
}

func (f *methodOpsFixture) methods(t *testing.T) []repository.ContactMethod {
	t.Helper()
	got, err := f.methodRepo.ListContactMethodsByContact(f.ctx, f.contactID)
	require.NoError(t, err)
	return got
}

func (f *methodOpsFixture) byID(t *testing.T, id uuid.UUID) *repository.ContactMethod {
	t.Helper()
	for _, m := range f.methods(t) {
		if m.ID == id {
			return &m
		}
	}
	return nil
}

func idPtr(id uuid.UUID) *uuid.UUID { return &id }

// TestApplyOperations_ValueSwapSucceeds pins the delete-and-reinsert apply
// strategy against a full value swap, which an in-place rewrite cannot do:
// idx_contact_method_unique_value is enforced per statement, so whichever row
// moved first would collide with the other's still-present key.
//
// Mutation: replace the delete-and-reinsert in applyFinalState with in-place
// updates.
func TestApplyOperations_ValueSwapSucceeds(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFixture(t)
	ns := syntheticNS(t)

	a := f.seed(t, "email", "a-"+ns+"@x.test", true)
	b := f.seed(t, "email", "b-"+ns+"@x.test", false)

	_, err := f.svc.ApplyOperations(f.ctx, f.contactID, []service.ContactMethodOperation{
		{Op: service.MethodOpUpdate, MethodID: idPtr(a.ID), Type: "email", Value: "b-" + ns + "@x.test"},
		{Op: service.MethodOpUpdate, MethodID: idPtr(b.ID), Type: "email", Value: "a-" + ns + "@x.test"},
	})
	require.NoError(t, err, "a full value swap must succeed in one request")

	gotA := f.byID(t, a.ID)
	gotB := f.byID(t, b.ID)
	require.NotNil(t, gotA)
	require.NotNil(t, gotB)
	require.Equal(t, "b-"+ns+"@x.test", gotA.Value)
	require.Equal(t, "a-"+ns+"@x.test", gotB.Value)
	// Identity survives the reinsert — that is what makes update a distinct
	// verb from remove-then-add.
	require.Equal(t, a.CreatedAt.UTC(), gotA.CreatedAt.UTC(), "created_at must survive the reinsert")
	require.Equal(t, b.CreatedAt.UTC(), gotB.CreatedAt.UTC(), "created_at must survive the reinsert")
}

// TestApplyOperations_TypeOnlySwapSucceeds is the case the abandoned
// temporary-value scheme could not even detect: the stored values never change,
// only the types, so there is no value change to trigger a temp phase — yet
// (type, value_normalized) changes for both rows.
//
// Mutation: key the delete decision on the VALUE rather than the (type, value)
// key.
func TestApplyOperations_TypeOnlySwapSucceeds(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFixture(t)
	shared := "1" + syntheticNS(t) + "@a.co"

	a := f.seed(t, "email", shared, false)
	b := f.seed(t, "telegram", shared, false)

	_, err := f.svc.ApplyOperations(f.ctx, f.contactID, []service.ContactMethodOperation{
		{Op: service.MethodOpUpdate, MethodID: idPtr(a.ID), Type: "telegram", Value: shared},
		{Op: service.MethodOpUpdate, MethodID: idPtr(b.ID), Type: "email", Value: shared},
	})
	require.NoError(t, err, "a type-only swap must succeed in one request")

	require.Equal(t, "telegram", f.byID(t, a.ID).Type)
	require.Equal(t, "email", f.byID(t, b.ID).Type)
}

// TestApplyOperations_UpdatesStableKeyValue is the mechanism pin for apply
// step 4. A case-only correction changes the stored value the user asked for
// while leaving (type, value_normalized) identical, so steps 1-3 never touch the
// row. Without an in-place update phase the request returns success having
// stored nothing — a silent-success failure.
//
// Mutation: delete apply step 4. The test must then fail on the stored-value
// assertion, not on an error.
func TestApplyOperations_UpdatesStableKeyValue(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFixture(t)
	ns := syntheticNS(t)

	a := f.seed(t, "email", "Case-"+ns+"@Example.test", false)
	lowered := "case-" + ns + "@example.test"

	_, err := f.svc.ApplyOperations(f.ctx, f.contactID, []service.ContactMethodOperation{
		{Op: service.MethodOpUpdate, MethodID: idPtr(a.ID), Type: "email", Value: lowered},
	})
	require.NoError(t, err)

	got := f.byID(t, a.ID)
	require.NotNil(t, got)
	require.Equal(t, lowered, got.Value,
		"a value edit that does not move the normalized key must still persist")
	require.Equal(t, a.ID, got.ID)
	require.Equal(t, a.CreatedAt.UTC(), got.CreatedAt.UTC())
}

// TestApplyOperations_KeyChangingUpdateOfPrimaryKeepsPrimary pins apply
// steps 5-6 being defined by DATABASE STATE rather than by designation delta.
//
// The primary's own key changes, so it is deleted and reinserted non-primary
// while its designation never changed. A delta-based rule fires neither demote
// nor promote, and the contact silently ends with no primary at all.
//
// Mutation: gate the promote on "the designated primary differs from the
// pre-state primary".
func TestApplyOperations_KeyChangingUpdateOfPrimaryKeepsPrimary(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFixture(t)
	ns := syntheticNS(t)

	a := f.seed(t, "email", "p-"+ns+"@x.test", true)

	_, err := f.svc.ApplyOperations(f.ctx, f.contactID, []service.ContactMethodOperation{
		{Op: service.MethodOpUpdate, MethodID: idPtr(a.ID), Type: "email", Value: "moved-" + ns + "@x.test"},
	})
	require.NoError(t, err)

	got := f.byID(t, a.ID)
	require.NotNil(t, got)
	require.True(t, got.IsPrimary,
		"a key-changing update of the primary must leave it still primary")

	primaries := 0
	for _, m := range f.methods(t) {
		if m.IsPrimary {
			primaries++
		}
	}
	require.Equal(t, 1, primaries, "exactly one primary must remain")
}

// TestApplyOperations_UnnamedMethodsSurvive is the acceptance bar for the
// lost-update defect: a client that has never seen a method cannot destroy it,
// because absence expresses nothing in an operations payload.
//
// Asserts the FULL persisted row, including updated_at and value_normalized.
// The chosen apply mechanism is physical reinsertion, so an implementation that
// needlessly deletes and reinserts every row would preserve id/type/value and
// move updated_at — passing a weaker version of the very test written to catch
// that.
//
// Mutation: make applyFinalState delete all rows and reinsert the final state.
func TestApplyOperations_UnnamedMethodsSurvive(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFixture(t)
	ns := syntheticNS(t)

	unseen := f.seed(t, "email", "unseen-"+ns+"@x.test", true)

	_, err := f.svc.ApplyOperations(f.ctx, f.contactID, []service.ContactMethodOperation{
		{Op: service.MethodOpAdd, Type: "phone", Value: "+1555" + ns[:4] + "001"},
	})
	require.NoError(t, err)

	got := f.byID(t, unseen.ID)
	require.NotNil(t, got, "a method no operation named must survive")
	require.Equal(t, unseen.ID, got.ID)
	require.Equal(t, unseen.Type, got.Type)
	require.Equal(t, unseen.Value, got.Value)
	require.Equal(t, unseen.ValueNormalized, got.ValueNormalized)
	require.Equal(t, unseen.IsPrimary, got.IsPrimary)
	require.Equal(t, unseen.CreatedAt.UTC(), got.CreatedAt.UTC())
	require.Equal(t, unseen.UpdatedAt.UTC(), got.UpdatedAt.UTC(),
		"updated_at must not move — an untouched row must not be rewritten")
}

// TestApplyOperations_RollsBackWholePayloadOnFailure pins validation-stage
// atomicity: one invalid operation leaves zero partial effects.
//
// Mutation: apply operations incrementally instead of validating the whole
// intended final state first.
func TestApplyOperations_RollsBackWholePayloadOnFailure(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFixture(t)
	ns := syntheticNS(t)

	a := f.seed(t, "email", "keep-"+ns+"@x.test", true)
	before := f.methods(t)

	_, err := f.svc.ApplyOperations(f.ctx, f.contactID, []service.ContactMethodOperation{
		{Op: service.MethodOpAdd, Type: "phone", Value: "+1555" + ns[:4] + "002"},
		// Unsatisfiable: blank value.
		{Op: service.MethodOpAdd, Type: "email", Value: ""},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, service.ErrInvalidOperations)

	after := f.methods(t)
	require.Len(t, after, len(before), "no operation may land when any is rejected")
	require.NotNil(t, f.byID(t, a.ID))
}

// TestApplyOperations_ReplayIsSafePerOperationClass covers every operation class
// twice, asserting the second attempt changes nothing.
//
// The remove case deliberately removes a PRESENT row and then replays, so the
// replay resolves through the ownership lookup's "does not exist" branch — the
// branch that actually carries the idempotency guarantee. A remove-of-absent
// case would leave that branch unexercised, since both attempts would be no-ops.
func TestApplyOperations_ReplayIsSafePerOperationClass(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ops  func(f *methodOpsFixture, t *testing.T) []service.ContactMethodOperation
	}{
		{
			name: "add",
			ops: func(f *methodOpsFixture, t *testing.T) []service.ContactMethodOperation {
				return []service.ContactMethodOperation{
					{Op: service.MethodOpAdd, Type: "email", Value: "replay-" + syntheticNS(t) + "@x.test"},
				}
			},
		},
		{
			name: "update",
			ops: func(f *methodOpsFixture, t *testing.T) []service.ContactMethodOperation {
				m := f.seed(t, "email", "u-"+syntheticNS(t)+"@x.test", false)
				return []service.ContactMethodOperation{
					{Op: service.MethodOpUpdate, MethodID: idPtr(m.ID), Type: "email", Value: "u2-" + syntheticNS(t) + "@x.test"},
				}
			},
		},
		{
			name: "set_primary",
			ops: func(f *methodOpsFixture, t *testing.T) []service.ContactMethodOperation {
				m := f.seed(t, "email", "sp-"+syntheticNS(t)+"@x.test", false)
				return []service.ContactMethodOperation{
					{Op: service.MethodOpSetPrimary, MethodID: idPtr(m.ID)},
				}
			},
		},
		{
			name: "clear_primary",
			ops: func(f *methodOpsFixture, t *testing.T) []service.ContactMethodOperation {
				m := f.seed(t, "email", "cp-"+syntheticNS(t)+"@x.test", true)
				return []service.ContactMethodOperation{
					{Op: service.MethodOpClearPrimary, MethodID: idPtr(m.ID)},
				}
			},
		},
		{
			name: "remove_of_present_row",
			ops: func(f *methodOpsFixture, t *testing.T) []service.ContactMethodOperation {
				m := f.seed(t, "email", "rm-"+syntheticNS(t)+"@x.test", false)
				return []service.ContactMethodOperation{
					{Op: service.MethodOpRemove, MethodID: idPtr(m.ID)},
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMethodOpsFixture(t)
			ops := tc.ops(f, t)

			_, err := f.svc.ApplyOperations(f.ctx, f.contactID, ops)
			require.NoError(t, err, "first attempt should succeed")
			afterFirst := f.methods(t)

			_, err = f.svc.ApplyOperations(f.ctx, f.contactID, ops)
			require.NoError(t, err, "replaying the same payload must succeed")
			afterSecond := f.methods(t)

			require.Equal(t, len(afterFirst), len(afterSecond),
				"a replayed payload must not change the method set")
			for i := range afterFirst {
				require.Equal(t, afterFirst[i].ID, afterSecond[i].ID)
				require.Equal(t, afterFirst[i].Value, afterSecond[i].Value)
				require.Equal(t, afterFirst[i].IsPrimary, afterSecond[i].IsPrimary)
			}
		})
	}
}

// TestContactMethodRepo_ClassifiesValueConflict is seam 1: force a genuine
// unique violation by bypassing the fold entirely and inserting a
// trigger-colliding value straight through the repository. Real PostgreSQL
// error, real constraint name, real classification code, no mocking.
//
// Mutation: delete the repository's 23505 classification.
func TestContactMethodRepo_ClassifiesValueConflict(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFixture(t)
	ns := syntheticNS(t)

	// '@@foo' and 'foo' both normalize to 'foo' through the trigger's '^@+'
	// rule, so the second insert collides on (contact_id, type,
	// value_normalized).
	_ = f.seed(t, "discord", "handle"+ns, false)

	_, err := f.methodRepo.InsertContactMethodWithIdentity(f.ctx, repository.InsertContactMethodWithIdentityRequest{
		ID:        uuid.New(),
		ContactID: f.contactID,
		Type:      "discord",
		Value:     "@@handle" + ns,
		CreatedAt: accelerated.GetCurrentTime(),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, repository.ErrMethodValueConflict,
		"a unique violation on idx_contact_method_unique_value must classify as ErrMethodValueConflict")

	// The error must carry NO detail from PostgreSQL. Its Detail for this
	// violation reads "Key (contact_id, type, value_normalized)=(<uuid>, email,
	// <value>) already exists.", and this error travels to the HTTP response
	// body — so including it would put a real contact's method value, the
	// contact id, and the column layout in front of the client.
	//
	// Asserted at the repository because that is where the string is built and
	// where this path is reachable. Above here a correct C6 mirror rejects the
	// collision during the fold, so no HTTP-level test can drive it.
	msg := err.Error()
	assert.NotContains(t, msg, "handle"+ns, "the method value leaked into the error")
	assert.NotContains(t, msg, f.contactID.String(), "the contact id leaked into the error")
	assert.NotContains(t, msg, "value_normalized", "the column layout leaked into the error")
	assert.NotContains(t, msg, "already exists", "PostgreSQL's detail string leaked into the error")
}

// TestContactMethodRepo_UnrelatedConstraintNotSwallowed is the pair that makes
// the constraint-name scoping meaningful. contact_method carries a SECOND
// unique index, idx_contact_method_primary (at most one primary per contact).
// A violation of that one must pass through unwrapped: reporting it as a
// duplicate-value conflict would tell the user "duplicate value" for what is
// actually a two-primaries bug.
//
// Mutation: drop the ConstraintName scoping so any 23505 maps to
// ErrMethodValueConflict.
func TestContactMethodRepo_UnrelatedConstraintNotSwallowed(t *testing.T) {
	t.Parallel()
	f := newMethodOpsFixture(t)
	ns := syntheticNS(t)

	_ = f.seed(t, "email", "first-"+ns+"@x.test", true)
	second := f.seed(t, "email", "second-"+ns+"@x.test", false)

	// Promoting a second row while the first is still primary violates
	// idx_contact_method_primary, not the value index.
	err := f.methodRepo.PromoteContactMethodPrimaryByContact(f.ctx, f.contactID, second.ID)
	require.Error(t, err)
	require.NotErrorIs(t, err, repository.ErrMethodValueConflict,
		"a primary-index violation must not be reported as a duplicate-value conflict")
}
