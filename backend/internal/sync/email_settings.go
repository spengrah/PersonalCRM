package sync

import (
	"fmt"
	"strings"
)

// MetadataKeyEmailOwnDomains is the external_sync_state.metadata key holding
// the per-account own-domain list used by Gmail participant discovery: a
// domain in this list anchors trust (the sender counts as "me") and excludes
// its addresses from the candidate pool. The value is deployment
// configuration only — never hardcoded, and never a real domain in tests.
const MetadataKeyEmailOwnDomains = "discovery_own_domains"

// EmailOwnDomains parses the per-account own-domain list from an email sync
// state's metadata. Nil-safe: nil/absent metadata, a missing key, or a value
// that isn't a JSON array yields an empty set. Each entry is lowercased and
// trimmed; entries that are empty or contain '@' are ignored rather than
// rejected, since this reads back what NormalizeOwnDomains already validated
// (or metadata written outside that path) — a malformed entry should degrade
// silently, not panic or block discovery.
func EmailOwnDomains(metadata map[string]any) map[string]struct{} {
	domains := make(map[string]struct{})
	if metadata == nil {
		return domains
	}
	raw, ok := metadata[MetadataKeyEmailOwnDomains].([]any)
	if !ok {
		return domains
	}
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || strings.Contains(s, "@") {
			continue
		}
		domains[s] = struct{}{}
	}
	return domains
}

// NormalizeOwnDomains validates and lowercases a user-supplied domain list
// (crm-admin --set-email-own-domains input). It rejects any entry that is
// empty, contains '@' or whitespace, or lacks a dot — unlike EmailOwnDomains,
// operator input errors loudly rather than degrading silently. An empty input
// slice is valid and returns an empty slice (the explicit-clear case).
func NormalizeOwnDomains(raw []string) ([]string, error) {
	normalized := make([]string, 0, len(raw))
	for _, d := range raw {
		trimmed := strings.TrimSpace(d)
		if trimmed == "" {
			return nil, fmt.Errorf("own-domain entry must not be empty")
		}
		if strings.ContainsAny(trimmed, " \t\n\r") {
			return nil, fmt.Errorf("own-domain entry %q must not contain whitespace", d)
		}
		if strings.Contains(trimmed, "@") {
			return nil, fmt.Errorf("own-domain entry %q must be a domain, not an address", d)
		}
		lower := strings.ToLower(trimmed)
		if !strings.Contains(lower, ".") {
			return nil, fmt.Errorf("own-domain entry %q must contain a dot", d)
		}
		normalized = append(normalized, lower)
	}
	return normalized, nil
}

// WithEmailOwnDomains returns a COPY of metadata with
// MetadataKeyEmailOwnDomains set to domains, or removed when domains is
// empty (the explicit-clear case). Every other key is preserved — the caller
// (crm-admin's --set-email-own-domains) must read-modify-write through this
// helper because UpdateSyncStateMetadata is a full JSONB replace, so passing
// a metadata map holding only the new key would silently drop
// backfill_since/terminal_reason/etc. The input map is never mutated.
func WithEmailOwnDomains(metadata map[string]any, domains []string) map[string]any {
	out := make(map[string]any, len(metadata)+1)
	for k, v := range metadata {
		out[k] = v
	}
	if len(domains) == 0 {
		delete(out, MetadataKeyEmailOwnDomains)
		return out
	}
	list := make([]any, len(domains))
	for i, d := range domains {
		list[i] = d
	}
	out[MetadataKeyEmailOwnDomains] = list
	return out
}
