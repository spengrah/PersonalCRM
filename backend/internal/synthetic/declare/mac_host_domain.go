package declare

// Mac-host-domain resolutions (spec/mac-host.yaml).
//
// Both fixtures pair a LIVE host through the real pairing services, because the
// settings surface reads the ACTIVE host list and the harness's own marker host is
// revoked and therefore invisible to it. See MacHost for what a declaration
// inherits by taking the database-wide singleton slot.
func init() {
	// The paired-host row as the settings page renders it: both permission states,
	// and two sources mid-push. `messages` carries an ordinary cursor the cell shows
	// verbatim; `icloud_contacts` carries one it must WITHHOLD, because for a source
	// that reports backfill progress the cursor is an opaque change token rather
	// than a position an operator can read.
	//
	// That second entry is deliberately part of THIS fixture, and it is what serves
	// MAC-046's in-progress key. A freshly paired host IS mid-backfill, so the shape
	// is the honest one — and MAC-046's own two keys are mutually exclusive states
	// of one JSONB flag on a singleton host, which no single fixture can hold at
	// once. The in-progress assertion rides this world; the citation stays on
	// MAC-046, because riding another behavior's fixture never moves a citation.
	Register(Declaration{
		Behavior: "MAC-018",
		Entities: []Entity{
			MacHost("host", PushedSource("messages"), PushedSource("icloud_contacts")),
		},
	})

	// The completed-backfill state, which is the one that licenses the cursor cell
	// to substitute a live count for the change token — so the host also owns three
	// iCloud candidates for it to count. They are declared AFTER the host because an
	// ingest candidate is stamped with the host it was pushed from and the count
	// route reads only that column; registration enforces the order.
	Register(Declaration{
		Behavior: "MAC-046",
		Entities: []Entity{
			MacHost("host", BackfilledSource("icloud_contacts")),
			ExternalCandidate("icloud-a", Source(SourceICloudContacts)),
			ExternalCandidate("icloud-b", Source(SourceICloudContacts)),
			ExternalCandidate("icloud-c", Source(SourceICloudContacts)),
		},
	})
}
