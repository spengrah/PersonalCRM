package declare

// Notes-domain resolutions (spec/notes-meetings.yaml).
//
// Both fixtures are a bare contact and NO note, deliberately. The notepad is
// written through the real product route PUT /contacts/:id/notes, which the
// citing specs already call directly; hoisting those writes into the fixture
// would make the assertions vacuous, because what the notepad does with a short
// body versus a clamped one IS the claim under test. NTS-007's first test also
// requires the contact to start with no Notes row at all.
func init() {
	// The display fixture: one contact whose notepad the citing tests write, read
	// back, expand and collapse.
	Register(Declaration{
		Behavior: "NTS-007",
		Entities: []Entity{Contact("target")},
	})

	// The edit fixture: one contact whose notepad is saved, replaced and cleared
	// through the contact form.
	Register(Declaration{
		Behavior: "NTS-008",
		Entities: []Entity{Contact("target")},
	})
}
