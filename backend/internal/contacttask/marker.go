package contacttask

import (
	"encoding/json"
	"strings"
)

// CRMMarker is the parsed/structured form of the JSON marker embedded in a
// Todoist task description. It is the single source of truth for the wire
// shape that links a Todoist task back to a CRM contact and task taxonomy.
//
// The wire format is a JSON object with five keys, emitted in sorted-key
// order by json.Marshal of a map[string]any:
//
//	{"contact_id":"<uuid>","crm":true,"instance":"<id>","kind":"<kind>","lifecycle":"<lifecycle>"}
//
// Instance is present on the wire and populated by DecodeMarker, but the
// recovery policy in todoist.tryRecoverPendingTempID does not branch on it —
// recovery keys on contact_id + lifecycle only.
type CRMMarker struct {
	ContactID string // required
	Kind      string
	Lifecycle string
	Instance  string
}

// crmMarkerJSON is the on-the-wire JSON shape. Decode-only: encoding goes
// through a map[string]any (see EncodeMarker) to guarantee sorted-key byte
// output. The struct exists so DecodeMarker can unmarshal in one step.
type crmMarkerJSON struct {
	CRM       bool   `json:"crm"`
	ContactID string `json:"contact_id"`
	Kind      string `json:"kind"`
	Lifecycle string `json:"lifecycle"`
	Instance  string `json:"instance"`
}

// EncodeMarker returns the canonical JSON marker bytes for a managed task.
//
// It marshals a map[string]any so the output keys are emitted in sorted
// order (contact_id, crm, instance, kind, lifecycle) — the exact byte layout
// every encoder has emitted historically. The "crm":true literal and the
// "instance" field are always present. This is the single sanctioned path
// for constructing the marker; the construction guard enforces that no other
// file builds it.
func EncodeMarker(m CRMMarker) ([]byte, error) {
	return json.Marshal(map[string]any{
		"crm":        true,
		"contact_id": m.ContactID,
		"kind":       m.Kind,
		"lifecycle":  m.Lifecycle,
		"instance":   m.Instance,
	})
}

// DecodeMarker extracts and normalizes a CRMMarker from a Todoist task
// description. It is the ONLY public decode entry point. Returns
// (marker, true) on a valid, normalized CRM marker; (zero, false) when no
// valid marker is present OR the legacy kind is unknown.
func DecodeMarker(description string) (CRMMarker, bool) {
	m, ok := parseMarker(description)
	if !ok {
		return CRMMarker{}, false
	}
	return normalizeLegacyMarker(m)
}

// parseMarker extracts a raw CRMMarker from a task description. It unmarshals
// the description as JSON; if that errs OR the parsed object is not a valid
// CRM marker (!crm || contact_id == ""), it retries from the LAST '{' in the
// description (handling markdown-prefixed markers). If the retry also fails
// the same predicate, it returns (zero, false).
func parseMarker(description string) (CRMMarker, bool) {
	var raw crmMarkerJSON
	descToTry := description
	if err := json.Unmarshal([]byte(descToTry), &raw); err != nil || !raw.CRM || raw.ContactID == "" {
		raw = crmMarkerJSON{}
		if idx := strings.LastIndex(description, "{"); idx >= 0 {
			descToTry = description[idx:]
		}
		if err := json.Unmarshal([]byte(descToTry), &raw); err != nil || !raw.CRM || raw.ContactID == "" {
			return CRMMarker{}, false
		}
	}
	return CRMMarker{
		ContactID: raw.ContactID,
		Kind:      raw.Kind,
		Lifecycle: raw.Lifecycle,
		Instance:  raw.Instance,
	}, true
}

// normalizeLegacyMarker fills Lifecycle for legacy markers that predate the
// (kind, lifecycle) split. It is a no-op when Lifecycle is already set.
// Maps legacy kind -> (kind, lifecycle):
//
//	cadence|"" -> (reach_out, cadence_due)
//	follow_up  -> (reach_out, followup_loop)
//	action     -> (action, manual)
//
// Returns (normalized, true) when recognized, (m, false) when the legacy
// kind is unknown (caller rejects).
//
// Legacy markers predate the (kind, lifecycle) split. Safe to delete this
// translation once a deploy has cleared all in-flight Todoist tasks created
// by pre-split code.
func normalizeLegacyMarker(m CRMMarker) (CRMMarker, bool) {
	if m.Lifecycle != "" {
		return m, true
	}
	switch m.Kind {
	case "cadence", "":
		m.Kind = KindReachOut
		m.Lifecycle = LifecycleCadenceDue
	case "follow_up":
		m.Kind = KindReachOut
		m.Lifecycle = LifecycleFollowUpLoop
	case KindAction:
		m.Kind = KindAction
		m.Lifecycle = LifecycleManual
	default:
		return m, false
	}
	return m, true
}
