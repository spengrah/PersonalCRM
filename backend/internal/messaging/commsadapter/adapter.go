package commsadapter

import (
	"fmt"
	"strings"

	"personal-crm/backend/internal/repository"
)

// Adapter is a source-parameterized aggregation.SourceAdapter for any source
// whose interaction ref prefix equals its interaction.source string. Wire
// format, for source "gchat" with label "GChat":
//
//   - SourceRef:       "gchat:<chatID>:<firstExternalID>"
//   - SourceRefPrefix: "gchat:<escaped chatID>:%"
//   - PeerRef:         "gchat:<chatID>"
//   - Description:     "GChat <label> (<n> messages)"
//
// The source==prefix equality is REQUIRED by consumer.CommsAggregatorReenqueuer,
// which recovers the chat id by stripping source + ":" from the event PeerRef.
// Deriving every format from the single source field is what makes that
// equality structural rather than a convention a new source could miss.
//
// Adapter is a value type: it satisfies aggregation.SourceAdapter when passed by
// value, so callers pass the NewAdapter result directly.
type Adapter struct {
	source string // interaction.source constant, e.g. "gchat", "whatsapp"
	label  string // human product name used in Description, e.g. "GChat", "WhatsApp"
}

// NewAdapter returns the SourceAdapter for a comms_message-backed source.
// source is the interaction.source constant (which is also the ref prefix);
// label is the human product name that appears in interaction descriptions.
func NewAdapter(source, label string) Adapter {
	return Adapter{source: source, label: label}
}

// SourceName returns the source string written into interaction.source.
func (a Adapter) SourceName() string {
	return a.source
}

// SourceRef formats the deterministic burst sourceRef.
func (a Adapter) SourceRef(chatID, firstExternalID string) string {
	return a.source + ":" + chatID + ":" + firstExternalID
}

// SourceRefPrefix returns the LIKE pattern for scoped "recent interaction in
// this chat" queries.
//
// Chat ids are opaque per-source strings that may legitimately contain the
// PostgreSQL LIKE wildcards `_` and `%` (Apple Messages guids empirically do).
// We escape UNCONDITIONALLY so the prefix stays correct for any source, present
// or future. The underlying InteractionFinder queries use
// `LIKE pattern ESCAPE '\'`, so the explicit escape takes effect end-to-end.
func (a Adapter) SourceRefPrefix(chatID string) string {
	return a.source + ":" + escapeLike(chatID) + ":%"
}

// PeerRef formats the event-payload PeerRef field. NOT a LIKE pattern — the
// escape is deliberately NOT applied because the consumer's reenqueuer parses
// this field with a literal string strip.
func (a Adapter) PeerRef(chatID string) string {
	return a.source + ":" + chatID
}

// Description formats the human-readable interaction description. Label
// phrasing (outreach/response/exchange) is shared across every source so UI
// surfaces stay consistent.
func (a Adapter) Description(direction string, msgCount int) string {
	label := "exchange"
	switch direction {
	case repository.InteractionDirectionOutbound:
		label = "outreach"
	case repository.InteractionDirectionInbound:
		label = "response"
	}
	return fmt.Sprintf("%s %s (%d messages)", a.label, label, msgCount)
}

// likeEscaper escapes the three LIKE metacharacters. The literal `\` is escaped
// too — belt-and-suspenders for a source whose chat ids ever contain one.
var likeEscaper = strings.NewReplacer(
	`\`, `\\`,
	`%`, `\%`,
	`_`, `\_`,
)

// escapeLike escapes the LIKE metacharacters so SourceRefPrefix is a correct
// pattern for `LIKE ... ESCAPE '\'`.
func escapeLike(s string) string {
	return likeEscaper.Replace(s)
}
