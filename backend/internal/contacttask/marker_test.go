package contacttask

import (
	"encoding/json"
	"testing"
)

// TestEncodeMarker_GoldenBytes locks the exact wire bytes: sorted keys,
// crm:true present, instance present. A regression that switches to a struct
// encoder (field-declaration order) or drops instance would change these
// bytes and break the Todoist wire format.
func TestEncodeMarker_GoldenBytes(t *testing.T) {
	got, err := EncodeMarker(CRMMarker{
		ContactID: "11111111-2222-3333-4444-555555555555",
		Kind:      KindReachOut,
		Lifecycle: LifecycleCadenceDue,
		Instance:  "inst-abc",
	})
	if err != nil {
		t.Fatalf("EncodeMarker: %v", err)
	}
	want := `{"contact_id":"11111111-2222-3333-4444-555555555555","crm":true,"instance":"inst-abc","kind":"reach_out","lifecycle":"cadence_due"}`
	if string(got) != want {
		t.Fatalf("golden bytes mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestEncodeDecode_RoundTrip covers every (kind, lifecycle) pair the codebase
// emits today. EncodeMarker -> DecodeMarker must preserve the fields, and the
// encoded bytes must carry the instance field.
func TestEncodeDecode_RoundTrip(t *testing.T) {
	const contactID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const instance = "inst-123"

	cases := []struct {
		name      string
		kind      string
		lifecycle string
	}{
		{"reach_out/cadence_due", KindReachOut, LifecycleCadenceDue},
		{"reach_out/followup_loop", KindReachOut, LifecycleFollowUpLoop},
		{"reach_out/manual", KindReachOut, LifecycleManual},
		{"send/manual", KindSend, LifecycleManual},
		{"reminder/manual", KindReminder, LifecycleManual},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := EncodeMarker(CRMMarker{
				ContactID: contactID,
				Kind:      tc.kind,
				Lifecycle: tc.lifecycle,
				Instance:  instance,
			})
			if err != nil {
				t.Fatalf("EncodeMarker: %v", err)
			}

			// The encoded bytes must contain the instance field on the wire.
			var raw map[string]any
			if err := json.Unmarshal(encoded, &raw); err != nil {
				t.Fatalf("unmarshal encoded: %v", err)
			}
			if raw["instance"] != instance {
				t.Fatalf("instance not on wire: got %v want %q", raw["instance"], instance)
			}
			if raw["crm"] != true {
				t.Fatalf("crm not true on wire: got %v", raw["crm"])
			}

			m, ok := DecodeMarker(string(encoded))
			if !ok {
				t.Fatalf("DecodeMarker returned ok=false for %s", encoded)
			}
			if m.ContactID != contactID {
				t.Errorf("ContactID: got %q want %q", m.ContactID, contactID)
			}
			if m.Kind != tc.kind {
				t.Errorf("Kind: got %q want %q", m.Kind, tc.kind)
			}
			if m.Lifecycle != tc.lifecycle {
				t.Errorf("Lifecycle: got %q want %q", m.Lifecycle, tc.lifecycle)
			}
			if m.Instance != instance {
				t.Errorf("Instance: got %q want %q", m.Instance, instance)
			}
		})
	}
}

// TestDecodeMarker_LegacyNormalization feeds legacy marker JSON (no lifecycle
// field) directly to DecodeMarker and asserts the legacy kind normalizes to
// the expected (kind, lifecycle). This locks the behavior moved out of
// tryRecoverPendingTempID.
func TestDecodeMarker_LegacyNormalization(t *testing.T) {
	const contactID = "11111111-1111-1111-1111-111111111111"

	cases := []struct {
		name          string
		legacyJSON    string
		wantKind      string
		wantLifecycle string
	}{
		{
			name:          "legacy cadence",
			legacyJSON:    `{"crm":true,"contact_id":"` + contactID + `","kind":"cadence"}`,
			wantKind:      KindReachOut,
			wantLifecycle: LifecycleCadenceDue,
		},
		{
			name:          "legacy empty kind",
			legacyJSON:    `{"crm":true,"contact_id":"` + contactID + `"}`,
			wantKind:      KindReachOut,
			wantLifecycle: LifecycleCadenceDue,
		},
		{
			name:          "legacy follow_up",
			legacyJSON:    `{"crm":true,"contact_id":"` + contactID + `","kind":"follow_up"}`,
			wantKind:      KindReachOut,
			wantLifecycle: LifecycleFollowUpLoop,
		},
		{
			name:          "legacy action",
			legacyJSON:    `{"crm":true,"contact_id":"` + contactID + `","kind":"action"}`,
			wantKind:      KindAction,
			wantLifecycle: LifecycleManual,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := DecodeMarker(tc.legacyJSON)
			if !ok {
				t.Fatalf("DecodeMarker(%s) returned ok=false", tc.legacyJSON)
			}
			if m.ContactID != contactID {
				t.Errorf("ContactID: got %q want %q", m.ContactID, contactID)
			}
			if m.Kind != tc.wantKind {
				t.Errorf("Kind: got %q want %q", m.Kind, tc.wantKind)
			}
			if m.Lifecycle != tc.wantLifecycle {
				t.Errorf("Lifecycle: got %q want %q", m.Lifecycle, tc.wantLifecycle)
			}
		})
	}
}

// TestDecodeMarker_MarkdownPrefix asserts a description that prepends prose
// or markdown before the JSON marker still parses via the last-'{' retry.
func TestDecodeMarker_MarkdownPrefix(t *testing.T) {
	const contactID = "22222222-2222-2222-2222-222222222222"
	desc := "Some notes about the contact\n\n---\n" +
		`{"crm":true,"contact_id":"` + contactID + `","kind":"reach_out","lifecycle":"manual","instance":"inst-x"}`

	m, ok := DecodeMarker(desc)
	if !ok {
		t.Fatalf("DecodeMarker returned ok=false for markdown-prefixed marker")
	}
	if m.ContactID != contactID {
		t.Errorf("ContactID: got %q want %q", m.ContactID, contactID)
	}
	if m.Kind != KindReachOut {
		t.Errorf("Kind: got %q want %q", m.Kind, KindReachOut)
	}
	if m.Lifecycle != LifecycleManual {
		t.Errorf("Lifecycle: got %q want %q", m.Lifecycle, LifecycleManual)
	}
	if m.Instance != "inst-x" {
		t.Errorf("Instance: got %q want %q", m.Instance, "inst-x")
	}
}

// TestDecodeMarker_Rejects covers the non-marker and invalid inputs that must
// return (zero, false).
func TestDecodeMarker_Rejects(t *testing.T) {
	cases := []struct {
		name string
		desc string
	}{
		{"crm false", `{"crm":false,"contact_id":"33333333-3333-3333-3333-333333333333","kind":"cadence"}`},
		{"empty contact_id", `{"crm":true,"contact_id":"","kind":"cadence"}`},
		{"unknown legacy kind", `{"crm":true,"contact_id":"33333333-3333-3333-3333-333333333333","kind":"bogus"}`},
		{"not json", `just some plain text with no marker`},
		{"empty string", ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := DecodeMarker(tc.desc)
			if ok {
				t.Fatalf("DecodeMarker(%q) returned ok=true (marker=%+v), want false", tc.desc, m)
			}
		})
	}
}
