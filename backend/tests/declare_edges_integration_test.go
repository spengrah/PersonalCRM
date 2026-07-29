//go:build integration_testdb

package tests

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSyntheticDeclareEdges executes EVERY registered adversarial edge in its
// own namespace and proves, through the production API read path, that the
// hostile state it names actually exists.
//
// An edge is never asserted statistically: it either exists in the world or it
// does not. The subtests are minted from declare.Edges(), so a registered edge
// cannot lack coverage, and the per-edge postcondition map is checked for
// completeness BEFORE any of them run — registering a twelfth edge without a
// postcondition fails the suite rather than depending on a reviewer noticing.
//
// Each edge then gets its namespace cleaned and its residue asserted to zero,
// tombstones included. That is what discharges the growth rule as PROOF rather
// than assumption: "the new row classes ride existing cleanup steps" is
// demonstrated per edge, not argued.
func TestSyntheticDeclareEdges(t *testing.T) {
	testsupport.RequireLongTests(t)
	t.Parallel()

	database, ctx := declareTestDB(t)
	router := newDeclareReadRouter(t, database)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)

	edges := declare.Edges()
	require.NotEmpty(t, edges, "the adversarial catalog must not be empty")

	checks := edgeChecks()
	for _, e := range edges {
		require.Contains(t, checks, e.Name,
			"every registered edge owes an API-read postcondition — an edge nobody reads back is not an edge, it is a row")
	}

	for _, e := range edges {
		t.Run(e.Name, func(t *testing.T) {
			namespace := declareNS(t)
			res, err := declare.RunEdgeForTest(ctx, database, e.Name, namespace, factory.DefaultSeed)
			require.NoError(t, err, "seed edge %s", e.Name)
			require.Equal(t, namespace, res.Namespace, "a fresh clone has free bands, so no re-salt is expected here")
			require.False(t, res.Anchor.IsZero(), "the manifest must carry the generator anchor")

			overdue := listOverdue(t, router)
			for _, pc := range e.PostconditionsAt(res.Anchor) {
				seeded, ok := res.Entities[pc.Handle]
				require.True(t, ok, "manifest is missing handle %q", pc.Handle)
				assertEdgePostcondition(t, router, overdue, res, pc, seeded)
			}

			checks[e.Name](t, router, res)

			// The namespace must still carry a tombstone the sweep has to find,
			// exactly as the declaration suite does. Some edges tombstone rows
			// themselves (merge losers, the soft-deleted parent); the others get
			// one soft-deleted through the real service here. An edge that seeds
			// no contact at all (the import queue) has nothing to tombstone, and
			// its residue claim is about external_contact rows anyway.
			if !edgeTombstonesItsOwn(e) {
				var victim string
				for _, seeded := range res.Entities {
					if seeded.Kind == "contact" {
						victim = seeded.ID
						break
					}
				}
				if victim != "" {
					require.NoError(t, contactRepo.SoftDeleteContact(ctx, uuidMust(t, victim)))
				}
			}

			before := measureResidue(t, ctx, database, res.Namespace, factory.DefaultSeed)
			require.Greater(t, before.total(), int64(0), "the world must exist before it is cleaned")

			requireCleaned(t, ctx, database, []string{res.Namespace}, factory.DefaultSeed)
			after := measureResidue(t, ctx, database, res.Namespace, factory.DefaultSeed)
			assert.Equal(t, int64(0), after.total(), "residue after cleanup: %+v", after)
		})
	}
}

// edgeTombstonesItsOwn reports whether an edge already leaves a tombstone
// behind, so the suite does not soft-delete a contact that is already gone.
func edgeTombstonesItsOwn(e declare.Edge) bool {
	for _, pc := range e.Postconditions() {
		if pc.Present != nil && !*pc.Present {
			return true
		}
	}
	return false
}

// --- the derived postconditions ---------------------------------------------

func assertEdgePostcondition(
	t *testing.T,
	router *gin.Engine,
	overdue []handlers.OverdueContactResponse,
	res declare.Result,
	pc declare.Postcondition,
	seeded declare.Seeded,
) {
	t.Helper()

	if pc.Present != nil && !*pc.Present {
		// A removed contact: the only assertable facts are absences.
		assert.False(t, containsContactID(listContacts(t, router, url.QueryEscape(seeded.Name)), seeded.ID),
			"handle %q was merged away or tombstoned, so the contact list must not return it", pc.Handle)
		assert.False(t, containsOverdueID(overdue, seeded.ID),
			"handle %q was removed, so it must have left the overdue read", pc.Handle)
		return
	}

	assertPostcondition(t, router, overdue, res, pc, seeded)

	if pc.NameEdge != nil {
		token, ok := factory.NameEdgeToken(factory.NameEdge(*pc.NameEdge))
		require.True(t, ok, "unknown name edge %q", *pc.NameEdge)
		assert.Contains(t, seeded.Name, token, "handle %q must carry its name-edge token", pc.Handle)
		assert.LessOrEqual(t, len([]rune(seeded.Name)), 255,
			"handle %q renders a name the contact API's own validator would reject", pc.Handle)
	}

	if pc.NameTwinOf != nil {
		twin, ok := res.Entities[*pc.NameTwinOf]
		require.True(t, ok, "handle %q twins %q, which is not in the manifest", pc.Handle, *pc.NameTwinOf)
		assert.Equal(t, twin.Name, seeded.Name, "handle %q must render exactly its twin's name", pc.Handle)
	}

	if pc.Birthday != nil {
		detail := getContact(t, router, seeded.ID)
		require.NotNil(t, detail.Birthday, "handle %q declared a birthday", pc.Handle)
		assert.Equal(t, pc.Birthday.UTC().Format("2006-01-02"), detail.Birthday.UTC().Format("2006-01-02"),
			"handle %q birthday must survive the read byte-identically", pc.Handle)
	}

	if pc.InteractionCount != nil {
		interactions := listInteractions(t, router, seeded.ID)
		assert.Len(t, interactions, *pc.InteractionCount,
			"handle %q must carry exactly the declared number of interactions", pc.Handle)
	}

	if pc.CreatedBeforeOldestInteraction {
		detail := getContact(t, router, seeded.ID)
		oldest := oldestOccurredAt(t, router, seeded.ID)
		assert.True(t, detail.CreatedAt.Before(oldest),
			"handle %q created_at %s is not STRICTLY before its oldest interaction %s — a contact cannot have heard from someone before it existed",
			pc.Handle, detail.CreatedAt, oldest)
	}
}

// --- per-edge API-read postconditions ---------------------------------------

type edgeCheck func(t *testing.T, router *gin.Engine, res declare.Result)

func edgeChecks() map[string]edgeCheck {
	return map[string]edgeCheck{
		"long-history":         checkLongHistory,
		"zero-method":          checkZeroMethod,
		"hostile-names":        checkHostileNames,
		"all-cadences-overdue": checkAllCadencesOverdue,
		"fully-empty":          checkFullyEmpty,
		"deep-import-queue":    checkDeepImportQueue,
		"merge-chain":          checkMergeChain,
		"soft-deleted-parent":  checkSoftDeletedParent,
		"page-overflow":        checkPageOverflow,
		"same-name-pair":       checkSameNamePair,
		"birthday-window":      checkBirthdayWindow,
	}
}

func checkLongHistory(t *testing.T, router *gin.Engine, res declare.Result) {
	subject := res.Entities["subject"]
	const want = 48

	// Page 1 at the default page size is FULL and the paging metadata is
	// internally consistent — a timeline this long is exactly where an off-by-one
	// or a missing Total surfaces.
	page1 := getEnvelope(t, router, "/api/v1/contacts/"+subject.ID+"/interactions?limit=20&page=1")
	require.Equal(t, http.StatusOK, page1.Status)
	var firstPage []interactionRow
	require.NoError(t, json.Unmarshal(page1.Data, &firstPage))
	assert.Len(t, firstPage, 20, "page 1 must be full")
	require.NotNil(t, page1.Meta)
	require.NotNil(t, page1.Meta.Pagination)
	assert.Equal(t, int64(want), page1.Meta.Pagination.Total, "the interactions read must report the whole history")
	assert.Equal(t, 3, page1.Meta.Pagination.Pages, "48 rows at 20 per page is three pages")

	// And walking the pages really does yield the whole history, with no
	// duplicates across page boundaries.
	seen := map[string]bool{}
	for page := 1; page <= page1.Meta.Pagination.Pages; page++ {
		var rows []interactionRow
		getJSON(t, router, fmt.Sprintf("/api/v1/contacts/%s/interactions?limit=20&page=%d", subject.ID, page), &rows)
		for _, r := range rows {
			assert.False(t, seen[r.ID], "interaction %s appears on more than one page", r.ID)
			seen[r.ID] = true
		}
	}
	assert.Len(t, seen, want, "the whole declared history must be readable across its pages")

	detail := getContact(t, router, subject.ID)
	require.NotNil(t, detail.LastContacted, "48 replayed inbound messages must have moved last_contacted")
	assert.True(t, detail.CreatedAt.Before(oldestOccurredAt(t, router, subject.ID)),
		"created_at must be strictly before the oldest interaction")
}

func checkZeroMethod(t *testing.T, router *gin.Engine, res declare.Result) {
	seeded := res.Entities["methodless"]
	detail := getContact(t, router, seeded.ID)
	assert.Empty(t, detail.Methods, "the zero-method contact must come back with no methods at all")
	assert.Nil(t, detail.PrimaryMethod, "there is no primary method to pick")
	assert.True(t, containsContactID(listContacts(t, router, url.QueryEscape(seeded.Name)), seeded.ID),
		"a method-less contact must still be listed — every read that assumes a method exists is what this edge catches")
}

func checkHostileNames(t *testing.T, router *gin.Engine, res declare.Result) {
	for _, handle := range []string{"long-name", "rtl-name", "emoji-name"} {
		seeded := res.Entities[handle]
		// BYTE-identical through both reads: no mangling, no truncation at the
		// API tier. The search read is included because it is the path a tour or
		// a spec uses to find a contact by name.
		detail := getContact(t, router, seeded.ID)
		assert.Equal(t, seeded.Name, detail.FullName, "%s: detail read must return the name byte-identically", handle)

		listed := listContacts(t, router, url.QueryEscape(seeded.Name))
		found := false
		for _, c := range listed {
			if c.ID == seeded.ID {
				found = true
				assert.Equal(t, seeded.Name, c.FullName, "%s: list read must return the name byte-identically", handle)
			}
		}
		assert.True(t, found, "%s must be reachable through the search read", handle)
		assert.LessOrEqual(t, len([]rune(seeded.Name)), 255, "%s exceeds what the contact API accepts", handle)
	}
}

func checkAllCadencesOverdue(t *testing.T, router *gin.Engine, res declare.Result) {
	overdue := listOverdue(t, router)
	byID := map[string]handlers.OverdueContactResponse{}
	for _, c := range overdue {
		byID[c.ID] = c
	}

	seen := map[string]int{}
	for _, cadence := range declare.Cadences() {
		seeded, ok := res.Entities[cadence]
		require.True(t, ok, "the edge must seed one contact per cadence; %q is missing", cadence)
		row, inOverdue := byID[seeded.ID]
		require.True(t, inOverdue, "the %s contact must be in the overdue read", cadence)
		require.NotNil(t, row.Cadence)
		seen[*row.Cadence]++
	}

	want := map[string]int{}
	for _, cadence := range declare.Cadences() {
		want[cadence] = 1
	}
	assert.Equal(t, want, seen, "the overdue read must contain exactly one member per cadence in the vocabulary")
}

func checkFullyEmpty(t *testing.T, router *gin.Engine, res declare.Result) {
	seeded := res.Entities["empty"]
	detail := getContact(t, router, seeded.ID)
	assert.Empty(t, detail.Methods)
	assert.Nil(t, detail.Cadence)
	assert.Nil(t, detail.LastContacted)
	assert.Nil(t, detail.Birthday)
	assert.Nil(t, detail.Location)
	assert.Nil(t, detail.HowMet)
	assert.Nil(t, detail.ContactBy, "no cadence means no due date")
	assert.Nil(t, detail.LastInteractionAt)
	assert.Nil(t, detail.LastOutreachAt)
	assert.Nil(t, detail.LastResponseAt)
	assert.False(t, detail.HasPendingFollowup)
}

func checkDeepImportQueue(t *testing.T, router *gin.Engine, res declare.Result) {
	// Per source, INDEPENDENTLY: each of the two keying shapes must paginate on
	// its own. A combined assertion would pass with a 24/0 split, which is not a
	// deep queue in both shapes — it is a deep queue in one.
	for _, source := range []string{declare.SourceGContacts, declare.SourceCorrespondence} {
		page1 := getEnvelope(t, router, "/api/v1/imports/candidates?source="+source+"&limit=10&page=1")
		require.Equal(t, http.StatusOK, page1.Status, "source %s", source)

		var rows []handlers.ImportCandidateResponse
		require.NoError(t, json.Unmarshal(page1.Data, &rows))
		assert.Len(t, rows, 10, "source %s: page 1 must be full", source)

		require.NotNil(t, page1.Meta, "source %s: the candidates read must carry pagination meta", source)
		require.NotNil(t, page1.Meta.Pagination)
		assert.Equal(t, 1, page1.Meta.Pagination.Page, "source %s", source)
		assert.Equal(t, 10, page1.Meta.Pagination.Limit, "source %s", source)
		assert.Equal(t, int64(12), page1.Meta.Pagination.Total, "source %s", source)
		assert.Equal(t, 2, page1.Meta.Pagination.Pages, "source %s", source)
	}

	unfiltered := getEnvelope(t, router, "/api/v1/imports/candidates?limit=10&page=1")
	require.Equal(t, http.StatusOK, unfiltered.Status)
	require.NotNil(t, unfiltered.Meta)
	require.NotNil(t, unfiltered.Meta.Pagination)
	assert.Equal(t, int64(24), unfiltered.Meta.Pagination.Total, "both shapes together")

	// Every manifest candidate must be readable by id, unmatched, and carry a
	// namespace-prefixed source_id in its OWN keying shape — the list DTO carries
	// neither field, so this claim can only be made per candidate.
	prefix := factory.NewGenerator(factory.DefaultSeed, res.Namespace).Prefix()
	idKeyed, emailKeyed := 0, 0
	for handle, seeded := range res.Entities {
		if seeded.Kind != "external_contact" {
			continue
		}
		var candidate repository.ExternalContact
		status := getJSONStatus(t, router, "/api/v1/imports/"+seeded.ID, &candidate)
		require.Equal(t, http.StatusOK, status, "candidate %q must be readable by id", handle)
		assert.Equal(t, repository.MatchStatusUnmatched, candidate.MatchStatus, "candidate %q", handle)
		assert.True(t, strings.HasPrefix(candidate.SourceID, prefix),
			"candidate %q source_id %q must carry the namespace prefix", handle, candidate.SourceID)
		if strings.Contains(candidate.SourceID, "@") {
			emailKeyed++
		} else {
			idKeyed++
		}
	}
	assert.Equal(t, 12, idKeyed, "the address-book shape keys source_id on an id")
	assert.Equal(t, 12, emailKeyed, "the correspondence shape keys source_id on the email address")
}

func checkMergeChain(t *testing.T, router *gin.Engine, res declare.Result) {
	a, b, c := res.Entities["a"], res.Entities["b"], res.Entities["c"]
	note := res.Entities["a-note"]

	// The survivor is present; both losers are gone from the list read.
	assert.True(t, containsContactID(listContacts(t, router, url.QueryEscape(c.Name)), c.ID), "C survives the chain")
	assert.False(t, containsContactID(listContacts(t, router, url.QueryEscape(a.Name)), a.ID), "A was merged away")
	assert.False(t, containsContactID(listContacts(t, router, url.QueryEscape(b.Name)), b.ID), "B was merged away")

	// A merged-away id is a plain 404: the API is not tombstone-aware, so it is
	// indistinguishable from an id that never existed. Pinning the exact status
	// is what makes a future 410-or-redirect a deliberate change.
	for name, seeded := range map[string]declare.Seeded{"A": a, "B": b} {
		env := getEnvelope(t, router, "/api/v1/contacts/"+seeded.ID)
		assert.Equal(t, http.StatusNotFound, env.Status, "%s detail read", name)
		if assert.NotNil(t, env.Error, "%s detail read must carry an error body", name) {
			assert.Equal(t, "NOT_FOUND", env.Error.Code, "%s detail read", name)
		}
	}

	// The POSITIVE half: A's children reparented across BOTH hops and are
	// readable through C. Without this the edge would pass on a chain that
	// silently dropped them.
	cInteractions := listInteractions(t, router, c.ID)
	assert.NotEmpty(t, cInteractions, "A's interaction must be readable through C after two hops")

	var notepad struct {
		Body string `json:"body"`
	}
	require.Equal(t, http.StatusOK, getJSONStatus(t, router, "/api/v1/contacts/"+c.ID+"/notes", &notepad))
	assert.Contains(t, notepad.Body, note.Name, "A's note must be readable through C after two hops")

	// The NEGATIVE half. The interactions read goes straight to the repository
	// with no parent check, so it answers 200 for a merged-away id — with zero
	// rows, because the merge re-pointed them.
	for name, seeded := range map[string]declare.Seeded{"A": a, "B": b} {
		var rows []map[string]any
		status := getJSONStatus(t, router, "/api/v1/contacts/"+seeded.ID+"/interactions", &rows)
		assert.Equal(t, http.StatusOK, status, "%s interactions read", name)
		assert.Empty(t, rows, "%s kept interactions the merge should have re-pointed", name)
	}
}

func checkSoftDeletedParent(t *testing.T, router *gin.Engine, res declare.Result) {
	parent := res.Entities["parent"]

	assert.False(t, containsContactID(listContacts(t, router, url.QueryEscape(parent.Name)), parent.ID),
		"a tombstoned contact must leave the contact list")
	assert.False(t, containsOverdueID(listOverdue(t, router), parent.ID),
		"a tombstoned contact must leave the overdue read despite being overdue at seed time")

	env := getEnvelope(t, router, "/api/v1/contacts/"+parent.ID)
	assert.Equal(t, http.StatusNotFound, env.Status, "detail read of a tombstoned contact")

	// THE ASYMMETRY, which is the useful adversarial fact here: notes and tasks
	// pre-check the parent (GetContact filters deleted_at IS NULL) and 404, while
	// interactions go straight to the repository and still serve the orphaned
	// rows. The orphans exist either way; only one surface reveals them. Pinning
	// today's answer is what makes a change to it deliberate.
	assert.Equal(t, http.StatusNotFound, getJSONStatus(t, router, "/api/v1/contacts/"+parent.ID+"/notes", nil),
		"the notes surface pre-checks the parent")
	assert.Equal(t, http.StatusNotFound, getJSONStatus(t, router, "/api/v1/contacts/"+parent.ID+"/tasks", nil),
		"the tasks surface pre-checks the parent")

	var rows []map[string]any
	assert.Equal(t, http.StatusOK, getJSONStatus(t, router, "/api/v1/contacts/"+parent.ID+"/interactions", &rows),
		"the interactions surface does NOT pre-check the parent")
	assert.NotEmpty(t, rows, "the orphaned interactions are still served — that is the asymmetry")
}

func checkPageOverflow(t *testing.T, router *gin.Engine, res declare.Result) {
	prefix := factory.NewGenerator(factory.DefaultSeed, res.Namespace).Prefix()

	// Unscoped, deliberately: the contact search is Postgres FULL TEXT search, so
	// a namespace PREFIX is not a term it can match. The suite runs on a per-test
	// clone that holds only this edge's world, so the unscoped read IS the
	// namespace's — and the prefix filter below keeps that assumption visible
	// rather than implicit.
	//
	// Real PaginationMeta, on the read that actually has it. (The overdue read
	// returns a bare unpaginated array with no Meta at all, so the metadata half
	// of this claim is asserted here rather than invented there.)
	env := getEnvelope(t, router, "/api/v1/contacts?limit=20&page=1")
	require.Equal(t, http.StatusOK, env.Status)
	var rows []handlers.ContactResponse
	require.NoError(t, json.Unmarshal(env.Data, &rows))
	assert.Len(t, rows, 20, "page 1 must be full")
	for _, c := range rows {
		require.True(t, strings.HasPrefix(c.FullName, prefix),
			"the clone must hold only this edge's world; %q is foreign", c.FullName)
	}
	require.NotNil(t, env.Meta)
	require.NotNil(t, env.Meta.Pagination)
	assert.Greater(t, env.Meta.Pagination.Total, int64(20), "the cohort must exceed one page")
	assert.Greater(t, env.Meta.Pagination.Pages, 1)
	assert.Equal(t, int64(56), env.Meta.Pagination.Total, "the whole cohort, exactly")

	// EXACT SET MEMBERSHIP of the seeded overdue ids — strictly stronger than a
	// ">= 50" bound, which would keep passing if half the cohort stopped being
	// overdue.
	wantOverdue := map[string]bool{}
	for _, pc := range mustEdge(t, "page-overflow").Postconditions() {
		if pc.OverdueMember != nil && *pc.OverdueMember {
			wantOverdue[res.Entities[pc.Handle].ID] = true
		}
	}
	require.Greater(t, len(wantOverdue), 50, "the cohort itself must exceed fifty overdue")

	gotOverdue := map[string]bool{}
	for _, c := range listOverdue(t, router) {
		if strings.HasPrefix(c.FullName, prefix) {
			gotOverdue[c.ID] = true
		}
	}
	assert.Equal(t, wantOverdue, gotOverdue, "the overdue read must contain exactly the seeded overdue ids")
}

func checkSameNamePair(t *testing.T, router *gin.Engine, res declare.Result) {
	twinA, twinB := res.Entities["twin-a"], res.Entities["twin-b"]
	require.Equal(t, twinA.Name, twinB.Name, "the twins must render one name")
	require.NotEqual(t, twinA.ID, twinB.ID)

	listed := listContacts(t, router, url.QueryEscape(twinA.Name))
	found := map[string]string{}
	for _, c := range listed {
		if c.ID == twinA.ID || c.ID == twinB.ID {
			found[c.ID] = c.FullName
		}
	}
	require.Len(t, found, 2, "both twins must be returned by the list read")
	assert.Equal(t, found[twinA.ID], found[twinB.ID], "both must render the SAME name")

	// What the matcher ACTUALLY does with a display-name tie, pinned exactly.
	//
	// The ambiguity suppression (usernameMatchGap) is gated on a Telegram
	// username-derived term, so a pure display-name tie falls through to the
	// sort's tiebreak — lexicographically smaller contact id wins — and the queue
	// silently proposes ONE of the two with no ambiguity signal. The score is
	// name similarity (1.0) times the import name weight (0.6), with NO method
	// contribution: the collider's generated email matches neither twin, so the
	// 0.4 method term never fires. Asserting ">= 0.5" would only prove the
	// threshold was crossed and would keep passing if the score drifted.
	env := getEnvelope(t, router, "/api/v1/imports/candidates?source="+declare.SourceGContacts)
	require.Equal(t, http.StatusOK, env.Status)
	var candidates []handlers.ImportCandidateResponse
	require.NoError(t, json.Unmarshal(env.Data, &candidates))

	collider := res.Entities["collider"]
	var match *handlers.SuggestedMatch
	for i := range candidates {
		if candidates[i].ID == collider.ID {
			match = candidates[i].SuggestedMatch
		}
	}
	require.NotNil(t, match, "the collider candidate must carry a suggested match")

	winner := twinA.ID
	if twinB.ID < winner {
		winner = twinB.ID
	}
	assert.Equal(t, winner, match.ContactID,
		"a display-name tie is resolved silently by contact-id order, not suppressed as ambiguous")
	assert.LessOrEqual(t, math.Abs(match.Confidence-0.6), 0.01,
		"name-only similarity times the import name weight, with no method contribution: got %v", match.Confidence)
}

func checkBirthdayWindow(t *testing.T, router *gin.Engine, res declare.Result) {
	anchor := res.Anchor

	cases := map[string]string{
		"bday-today":    "today",
		"bday-tomorrow": "week",
	}
	for handle, wantBucket := range cases {
		seeded := res.Entities[handle]
		detail := getContact(t, router, seeded.ID)
		require.NotNil(t, detail.Birthday, "%s must carry a birthday", handle)
		assert.Equal(t, wantBucket, synthetic.BirthdayBucket(*detail.Birthday, anchor), "%s bucket", handle)
	}

	// The leap fixture's bucket is date-dependent (a Feb-29 birthday is observed
	// on March 1 in common years), so the expectation comes from the SAME rule
	// the page implements rather than from a hand-picked answer. What is
	// asserted unconditionally is the thing the old 1900 sentinel got wrong: the
	// stored date is still February 29.
	leap := res.Entities["bday-leap"]
	detail := getContact(t, router, leap.ID)
	require.NotNil(t, detail.Birthday)
	assert.Equal(t, "02-29", detail.Birthday.UTC().Format("01-02"),
		"a leap-day birthday must survive storage as February 29, not roll to March 1")
	assert.Contains(t, []string{"celebrated", "today", "week", "distant"},
		synthetic.BirthdayBucket(*detail.Birthday, anchor))
}

// --- helpers -----------------------------------------------------------------

func mustEdge(t *testing.T, name string) declare.Edge {
	t.Helper()
	e, ok := declare.LookupEdge(name)
	require.True(t, ok, "edge %q is not registered", name)
	return e
}

type interactionRow struct {
	ID         string    `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
}

// listInteractions reads a contact's interactions at the handler's MAXIMUM page
// size. The default is 20, which would silently truncate any declared history
// longer than that and turn an exact count into a page-size assertion.
func listInteractions(t *testing.T, router *gin.Engine, contactID string) []interactionRow {
	t.Helper()
	var rows []interactionRow
	getJSON(t, router, "/api/v1/contacts/"+contactID+"/interactions?limit=100", &rows)
	return rows
}

// oldestOccurredAt is the earliest occurred_at across a contact's interactions.
func oldestOccurredAt(t *testing.T, router *gin.Engine, contactID string) time.Time {
	t.Helper()
	rows := listInteractions(t, router, contactID)
	require.NotEmpty(t, rows, "contact %s has no interactions to date against", contactID)
	oldest := rows[0].OccurredAt
	for _, r := range rows[1:] {
		if r.OccurredAt.Before(oldest) {
			oldest = r.OccurredAt
		}
	}
	return oldest
}
