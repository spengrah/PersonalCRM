//go:build integration_testdb

// Identity backfill (migration 088, IXN-026) driven through the real
// crm-admin --migrate: an isolated clone is rolled back to before migration 087,
// holds observations in the base schema's shape (no interaction_node_id column),
// and --migrate then applies 087 and 088. Cases that read the backfilled rows
// share one world built once, because rolling the schema back and forth is the
// slow step; the idempotence case runs last because it rolls 088 back.
//
// The fixture does not depend on later schema. Contacts and calendar events are
// seeded at the latest schema through their repositories, before any rollback.
// The observations are inserted at 087 and read at 087 and at the latest schema
// only through test queries whose column lists exist at both.
package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	migrate "github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// backfillWorld is the observations seeded before migration 087, by name, and
// the migrated state read back after crm-admin --migrate.
type backfillWorld struct {
	cloneURL, migrationsPath string
	contacts                 []uuid.UUID
	ids                      map[string]uuid.UUID // observation name → interaction id
	before                   map[uuid.UUID]db.TestListBackfillObservationsRow
	after                    map[uuid.UUID]db.TestListBackfillObservationsRow
	nodesBefore, nodesAfter  int64
}

func (w *backfillWorld) node(t *testing.T, name string) uuid.UUID {
	t.Helper()
	row, ok := w.after[w.ids[name]]
	require.True(t, ok, "observation %q must exist after the migration", name)
	require.NotNil(t, row.InteractionNodeID, "observation %q must resolve to an interaction node", name)
	return *row.InteractionNodeID
}

func openQueries(t *testing.T, ctx context.Context, url string) *db.Queries {
	t.Helper()
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return db.New(pool)
}

// readObservations reads every seeded observation, soft-deleted ones included.
func (w *backfillWorld) readObservations(t *testing.T, ctx context.Context, q *db.Queries) map[uuid.UUID]db.TestListBackfillObservationsRow {
	t.Helper()
	out := map[uuid.UUID]db.TestListBackfillObservationsRow{}
	for _, contactID := range w.contacts {
		rows, err := q.TestListBackfillObservations(ctx, contactID)
		require.NoError(t, err)
		for _, row := range rows {
			out[row.ID] = *row
		}
	}
	require.Len(t, out, len(w.ids), "every seeded observation is read back")
	return out
}

func countNodes(t *testing.T, ctx context.Context, q *db.Queries) int64 {
	t.Helper()
	all := ""
	n, err := q.SyntheticCountNodesByLabelPrefix(ctx, &all)
	require.NoError(t, err)
	return n
}

func strPtr(s string) *string { return &s }

// rollAppTo rolls the golang-migrate app schema down to version, whatever
// migrations follow it (no raw SQL — uses the migrate library).
func rollAppTo(t *testing.T, databaseURL, migrationsPath string, version uint) {
	t.Helper()
	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), databaseURL)
	require.NoError(t, err)
	require.NoError(t, m.Migrate(version), "roll app schema to %d", version)
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)
}

// backfillBaseVersion is migration 087, the schema the observations are seeded
// at: it has interaction.interaction_node_id, which the fixture leaves NULL.
const backfillBaseVersion = 87

// newBackfillWorld seeds observations at the schema before migration 087 and
// returns the world after crm-admin --migrate has run over it.
func newBackfillWorld(t *testing.T) *backfillWorld {
	t.Helper()
	ctx := t.Context()
	cloneURL, migrationsPath := newMigrateClone(t)
	w := &backfillWorld{cloneURL: cloneURL, migrationsPath: migrationsPath, ids: map[string]uuid.UUID{}}

	// Contacts and calendar events do not need a NULL interaction node, so they
	// are seeded at the latest schema through the repositories, before any
	// rollback. The pool closes before the schema rolls back under it.
	require.NoError(t, db.RunMigrations(ctx, cloneURL, migrationsPath))
	occurred := time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)
	seedPool, err := pgxpool.New(ctx, cloneURL)
	require.NoError(t, err)
	seedQueries := db.New(seedPool)
	contactRepo := repository.NewContactRepository(seedQueries)
	calendarRepo := repository.NewCalendarEventRepository(seedQueries)
	for _, name := range []string{"contact A", "contact B", "contact C"} {
		c, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
		require.NoError(t, err)
		w.contacts = append(w.contacts, c.ID)
	}
	a, b, c := w.contacts[0], w.contacts[1], w.contacts[2]

	event := func(gcalID string) string {
		t.Helper()
		ev, err := calendarRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
			GcalEventID: gcalID, GcalCalendarID: "calendar-1", GoogleAccountID: "account-1",
			StartTime: occurred, EndTime: occurred.Add(time.Hour), Status: "confirmed", SyncedAt: occurred,
		})
		require.NoError(t, err)
		return ev.ID.String()
	}
	e1, e2 := event("event-1"), event("event-2")
	// Live gcal observations whose calendar_event row is gone: the events are
	// deleted here, the observations of them are inserted below.
	gone1, gone2 := event("event-gone-1"), event("event-gone-2")
	require.NoError(t, calendarRepo.DeleteByGcalID(ctx, "event-gone-1", "calendar-1", "account-1"))
	require.NoError(t, calendarRepo.DeleteByGcalID(ctx, "event-gone-2", "calendar-1", "account-1"))
	seedPool.Close()

	// The base-schema rows: roll to 087 by explicit version, then insert through
	// queries whose column lists exist at 087 (the fixture insert takes no node).
	rollAppTo(t, cloneURL, migrationsPath, backfillBaseVersion)

	q := openQueries(t, ctx, cloneURL)
	observe := func(name string, contactID uuid.UUID, source string, sourceRef *string) {
		t.Helper()
		id := uuid.New()
		require.NoError(t, q.TestInsertBackfillObservation(ctx, db.TestInsertBackfillObservationParams{
			ID: id, ContactID: contactID, Source: source, SourceRef: sourceRef,
			OccurredAt: occurred, Direction: "mutual",
		}))
		w.ids[name] = id
	}

	observe("gcal e1 A", a, "gcal", &e1)
	observe("gcal e1 B", b, "gcal", &e1)
	observe("gcal e2 A", a, "gcal", &e2)

	thread := func(contactID uuid.UUID, thread, day string) *string {
		return strPtr(fmt.Sprintf("%s:%s:%s", contactID, thread, day))
	}
	observe("email t1 d1 A", a, "email", thread(a, "thread-1", "2026-10-01"))
	observe("email t1 d1 B", b, "email", thread(b, "thread-1", "2026-10-01"))
	observe("email t1 d2 A", a, "email", thread(a, "thread-1", "2026-10-02"))

	observe("anarlog s1 tagged A", a, "anarlog_sessions", strPtr(fmt.Sprintf("anarlog:session-1:%s", a)))
	observe("anarlog s1 title B", b, "anarlog_sessions", strPtr(fmt.Sprintf("anarlog:session-1:title:%s", b)))
	observe("anarlog s1 walkin C", c, "anarlog_sessions", strPtr(fmt.Sprintf("anarlog:session-1:walkin:%s", c)))
	observe("anarlog s2 A", a, "anarlog_sessions", strPtr(fmt.Sprintf("anarlog:session-2:%s", a)))

	observe("call", a, "phone_calls", strPtr("call-1"))
	observe("todoist", a, "todoist", strPtr("task-1"))
	observe("manual 1", a, "manual", nil)
	observe("manual 2", a, "manual", nil)

	observe("gone1 A", a, "gcal", &gone1)
	observe("gone1 B", b, "gcal", &gone1)
	observe("gone2 A", a, "gcal", &gone2)

	for _, source := range []string{"messages", "telegram", "whatsapp", "gchat"} {
		observe("burst "+source, a, source, strPtr("burst-"+source))
	}

	observe("deleted call", a, "phone_calls", strPtr("call-deleted"))
	require.NoError(t, q.SoftDeleteInteraction(ctx, w.ids["deleted call"]))

	w.before = w.readObservations(t, ctx, q)
	for name, id := range w.ids {
		require.Nil(t, w.before[id].InteractionNodeID, "observation %q is seeded without a node", name)
	}

	rollAppDownOne(t, cloneURL, migrationsPath) // one step down from pinned 087 → before 087
	// A fresh pool: the schema changed under the seeding pool's cached plans.
	q = openQueries(t, ctx, cloneURL)
	w.nodesBefore = countNodes(t, ctx, q)

	var out bytes.Buffer
	require.NoError(t, runMigrate(ctx, cloneURL, migrationsPath, &out))

	q = openQueries(t, ctx, cloneURL)
	w.after = w.readObservations(t, ctx, q)
	w.nodesAfter = countNodes(t, ctx, q)
	return w
}

func TestIdentityBackfill(t *testing.T) {
	ctx := t.Context()
	w := newBackfillWorld(t)
	q := openQueries(t, ctx, w.cloneURL)

	t.Run("two attendees' observations of one calendar event share one node", func(t *testing.T) {
		require.Equal(t, w.node(t, "gcal e1 A"), w.node(t, "gcal e1 B"))
		require.NotEqual(t, w.node(t, "gcal e1 A"), w.node(t, "gcal e2 A"))
	})

	t.Run("one email thread shares a node per day across contacts", func(t *testing.T) {
		require.Equal(t, w.node(t, "email t1 d1 A"), w.node(t, "email t1 d1 B"))
		require.NotEqual(t, w.node(t, "email t1 d1 A"), w.node(t, "email t1 d2 A"))
	})

	t.Run("tagged, title-matched and walk-in participants of one Anarlog session share one node", func(t *testing.T) {
		require.Equal(t, w.node(t, "anarlog s1 tagged A"), w.node(t, "anarlog s1 title B"))
		require.Equal(t, w.node(t, "anarlog s1 tagged A"), w.node(t, "anarlog s1 walkin C"))
		require.NotEqual(t, w.node(t, "anarlog s1 tagged A"), w.node(t, "anarlog s2 A"))
	})

	t.Run("a call and a Todoist entry each get a node of their own", func(t *testing.T) {
		require.NotEqual(t, w.node(t, "call"), w.node(t, "todoist"))
	})

	t.Run("each manual observation gets a node of its own", func(t *testing.T) {
		require.NotEqual(t, w.node(t, "manual 1"), w.node(t, "manual 2"))
	})

	t.Run("live gcal observations whose calendar_event is gone share one keyless node per source_ref", func(t *testing.T) {
		require.Equal(t, w.node(t, "gone1 A"), w.node(t, "gone1 B"))
		require.NotEqual(t, w.node(t, "gone1 A"), w.node(t, "gone2 A"))
		for _, name := range []string{"gone1 A", "gone2 A"} {
			node, err := q.TestGetInteractionNode(ctx, w.node(t, name))
			require.NoError(t, err)
			require.Nil(t, node.IdentityKey, "%q: a node for a vanished event carries no persisted key", name)
		}
	})

	t.Run("every live correctable observation resolves to an interaction node", func(t *testing.T) {
		for name := range w.ids {
			if name == "deleted call" || strings.HasPrefix(name, "burst ") {
				continue
			}
			node, err := q.GetNode(ctx, w.node(t, name))
			require.NoError(t, err, "observation %q", name)
			require.Equal(t, "interaction", node.Type, "observation %q", name)
		}
	})

	t.Run("message bursts gain no node", func(t *testing.T) {
		for _, source := range []string{"messages", "telegram", "whatsapp", "gchat"} {
			require.Nil(t, w.after[w.ids["burst "+source]].InteractionNodeID, "burst source %s", source)
		}
	})

	t.Run("a soft-deleted observation of a correctable source is unchanged", func(t *testing.T) {
		require.Equal(t, w.before[w.ids["deleted call"]], w.after[w.ids["deleted call"]])
	})

	t.Run("every column other than interaction_node_id is unchanged and no node beyond the expected ones is created", func(t *testing.T) {
		for name, id := range w.ids {
			after := w.after[id]
			after.InteractionNodeID = nil
			require.Equal(t, w.before[id], after, "observation %q", name)
		}
		// 12 source objects: 2 events, 2 email thread-days, 2 sessions, 1 call,
		// 1 task, 2 manual entries, 2 vanished events.
		require.Equal(t, w.nodesBefore+12, w.nodesAfter)
	})

	t.Run("rolling 088 back and migrating again leaves every observation on its node and creates no node", func(t *testing.T) {
		rollAppTo(t, w.cloneURL, w.migrationsPath, backfillBaseVersion) // before 088
		var out bytes.Buffer
		require.NoError(t, runMigrate(ctx, w.cloneURL, w.migrationsPath, &out))

		q := openQueries(t, ctx, w.cloneURL)
		again := w.readObservations(t, ctx, q)
		require.Equal(t, w.after, again)
		require.Equal(t, w.nodesAfter, countNodes(t, ctx, q))
	})
}
