package sync

import (
	"context"
	"fmt"
	"maps"
	"net/mail"
	"slices"
	"strings"

	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// EmailOwnList selects one of the lists an email sync state's metadata holds
// that make up the user's addresses for Gmail sync, the Gmail identifier
// rematch scan and Gmail participant discovery. EmailOwnDomains and
// EmailOwnAddresses are its values. Each list's definition (its metadata key,
// the grammar that parses operator input for it, and the rules that read it
// back) is private to this package and fixed at compile time, so every caller
// that selects a list writes and reads the same key. Set parses and writes in
// one operation, so no caller can write unparsed input or pair one list's
// input with another list's key. The values are deployment configuration only:
// never hardcoded, and never a real domain or address in tests.
type EmailOwnList int

const (
	// EmailOwnDomains is the own-domain list: every address at a listed
	// domain counts as the user. Its input is a comma-separated list of
	// domains.
	EmailOwnDomains EmailOwnList = iota + 1
	// EmailOwnAddresses is the own-address list: each listed address counts
	// as the user, and no other address at its domain does. It serves an
	// address at a domain the user shares with other people. Its input is a
	// mail header address list.
	EmailOwnAddresses
)

// emailOwnListDef is one list's definition.
type emailOwnListDef struct {
	// key is the external_sync_state.metadata key holding the list.
	key string
	// parse validates non-empty operator input and returns the stored form
	// of each entry.
	parse func(input string) ([]string, error)
	// keepStored reports whether a stored entry, normalized by
	// matching.NormalizeEmail, is well formed.
	keepStored func(normalized string) bool
}

// def returns the definition l selects. The zero EmailOwnList selects no list,
// so a selector left unset panics at its first use rather than writing another
// list's key.
func (l EmailOwnList) def() emailOwnListDef {
	switch l {
	case EmailOwnDomains:
		return emailOwnListDef{
			key:        "own_domains",
			parse:      parseOwnDomains,
			keepStored: func(s string) bool { return !strings.Contains(s, "@") },
		}
	case EmailOwnAddresses:
		return emailOwnListDef{
			key:        "own_addresses",
			parse:      parseOwnAddresses,
			keepStored: func(s string) bool { return strings.Contains(s, "@") },
		}
	default:
		panic(fmt.Sprintf("sync: EmailOwnList %d selects no list", int(l)))
	}
}

// Read parses the list from an email sync state's metadata. Nil-safe: nil or
// absent metadata, a missing key, or a value that isn't a JSON array yields an
// empty set. Each entry is normalized by matching.NormalizeEmail, the
// normalization Gmail sync compares addresses in; a malformed entry is ignored
// rather than rejected, so metadata written outside Set degrades silently
// instead of blocking sync.
func (l EmailOwnList) Read(metadata map[string]any) map[string]struct{} {
	def := l.def()
	out := make(map[string]struct{})
	raw, ok := metadata[def.key].([]any)
	if !ok {
		return out
	}
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		s = matching.NormalizeEmail(s)
		if s == "" || !def.keepStored(s) {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}

// EmailOwnListWriter is the repository surface Set writes through.
// *repository.SyncRepository satisfies it.
type EmailOwnListWriter interface {
	SetSyncStateMetadataKey(ctx context.Context, id uuid.UUID, key string, value any) (*repository.SyncState, error)
	DeleteSyncStateMetadataKey(ctx context.Context, id uuid.UUID, key string) (*repository.SyncState, error)
}

// Set parses input, operator input from crm-admin, by the list's grammar and
// writes the entries it yields as the list on the sync state with id. Input
// that is empty or only whitespace removes the list (the explicit-clear case),
// and is the only input that does. Malformed input, and input that yields no
// entry, such as the mail header empty group "undisclosed-recipients:;", errors
// loudly and writes nothing. The write changes only this list's key, in one
// statement, so every other key the row holds when it runs is kept, another
// list's key included. It returns the list the row holds after the write,
// sorted.
func (l EmailOwnList) Set(ctx context.Context, w EmailOwnListWriter, id uuid.UUID, input string) ([]string, error) {
	def := l.def()
	var (
		updated *repository.SyncState
		err     error
	)
	if strings.TrimSpace(input) == "" {
		updated, err = w.DeleteSyncStateMetadataKey(ctx, id, def.key)
	} else {
		entries, parseErr := def.parse(input)
		if parseErr != nil {
			return nil, parseErr
		}
		if len(entries) == 0 {
			return nil, fmt.Errorf("%q names no entry for %s; pass an empty string to clear the list", input, def.key)
		}
		updated, err = w.SetSyncStateMetadataKey(ctx, id, def.key, entries)
	}
	if err != nil {
		return nil, fmt.Errorf("write metadata key %s: %w", def.key, err)
	}
	return slices.Sorted(maps.Keys(l.Read(updated.Metadata))), nil
}

// parseOwnDomains splits input at commas, which no domain contains, and
// normalizes each entry by normalizeOwnDomain.
func parseOwnDomains(input string) ([]string, error) {
	var domains []string
	for _, entry := range strings.Split(input, ",") {
		domain, err := normalizeOwnDomain(entry)
		if err != nil {
			return nil, err
		}
		domains = append(domains, domain)
	}
	return domains, nil
}

// normalizeOwnDomain rejects an entry that is empty, contains '@' or
// whitespace, or lacks a dot, and lowercases the rest.
func normalizeOwnDomain(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("own-domain entry must not be empty")
	}
	if strings.ContainsAny(trimmed, " \t\n\r") {
		return "", fmt.Errorf("own-domain entry %q must not contain whitespace", raw)
	}
	if strings.Contains(trimmed, "@") {
		return "", fmt.Errorf("own-domain entry %q must be a domain, not an address", raw)
	}
	lower := strings.ToLower(trimmed)
	if !strings.Contains(lower, ".") {
		return "", fmt.Errorf("own-domain entry %q must contain a dot", raw)
	}
	return lower, nil
}

// parseOwnAddresses parses input as a mail header address list (net/mail), the
// grammar Gmail sync parses a To header by, so a comma inside a quoted local
// part stays within its address. It returns each address the list yields,
// normalized by matching.NormalizeEmail as sync normalizes a header address. An
// entry such as "<me@shared-domain.example>" is therefore stored as the
// address a header carrying it would be compared in. Input net/mail cannot
// parse as an address list is rejected.
func parseOwnAddresses(input string) ([]string, error) {
	addrs, err := mail.ParseAddressList(input)
	if err != nil {
		return nil, fmt.Errorf("own addresses %q must be a list of email addresses: %w", input, err)
	}
	normalized := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		normalized = append(normalized, matching.NormalizeEmail(addr.Address))
	}
	return normalized, nil
}
