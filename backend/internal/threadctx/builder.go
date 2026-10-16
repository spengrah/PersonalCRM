package threadctx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// contextKinds maps each thread venue kind to the context it gives. A venue of
// any other kind (a call) is not a thread.
var contextKinds = map[string]ContextKind{
	repository.VenueKindDM:          KindConversation,
	repository.VenueKindGroupChat:   KindConversation,
	repository.VenueKindEmailThread: KindConversation,
	repository.VenueKindMeeting:     KindMeeting,
	repository.VenueKindSession:     KindMeeting,
}

// Builder reads thread contexts and the content facts derived from them. Every
// read goes through one definition of thread content, so a thread's latest
// content time and the eligibility window agree with the contexts built.
type Builder struct {
	content *repository.ThreadContentRepository
	venues  *repository.VenueRepository
}

// NewBuilder builds a Builder.
func NewBuilder(content *repository.ThreadContentRepository, venues *repository.VenueRepository) *Builder {
	return &Builder{content: content, venues: venues}
}

// WithTx returns a Builder that reads through the transaction, so a caller
// inside a transaction builds contexts and content facts from the content
// that transaction wrote.
func (b *Builder) WithTx(tx pgx.Tx) *Builder {
	return &Builder{content: b.content.WithTx(tx), venues: b.venues.WithTx(tx)}
}

// Build returns the thread's content at or before cutOff, each message once
// however many per-contact copies the store keeps.
func (b *Builder) Build(ctx context.Context, thread VenueID, cutOff time.Time) (Context, error) {
	venue, err := b.venues.GetVenue(ctx, thread)
	if err != nil {
		return Context{}, fmt.Errorf("get thread venue: %w", err)
	}
	kind, ok := contextKinds[venue.Kind]
	if !ok {
		return Context{}, fmt.Errorf("venue kind %q is not a thread", venue.Kind)
	}
	rows, err := b.content.ListThreadContent(ctx, thread, cutOff)
	if err != nil {
		return Context{}, fmt.Errorf("list thread content: %w", err)
	}
	var attendees []uuid.UUID
	if kind == KindMeeting {
		if attendees, err = b.content.ListVenueContactsAsOf(ctx, thread, cutOff); err != nil {
			return Context{}, fmt.Errorf("list meeting contacts: %w", err)
		}
	}
	c, err := assemble(rows, attendees)
	if err != nil {
		return Context{}, err
	}
	c.Thread, c.Kind, c.CutOff = thread, kind, cutOff
	return c, nil
}

// LatestContentAt returns the time of the thread's latest content at or before
// asOf, and false when it has none.
func (b *Builder) LatestContentAt(ctx context.Context, thread VenueID, asOf time.Time) (time.Time, bool, error) {
	return b.content.LatestThreadContentAt(ctx, thread, asOf)
}

// ThreadsWithContentIn returns the threads with content after after, up to and
// including upTo.
func (b *Builder) ThreadsWithContentIn(ctx context.Context, after, upTo time.Time) ([]VenueID, error) {
	return b.content.ListThreadsWithContentIn(ctx, after, upTo)
}

// party is one participant while a context is assembled. key is its identity:
// its matched contact, else its source identity.
type party struct {
	key     string
	contact *uuid.UUID
	aliases []string
}

// assemble turns a thread's content rows, ordered by time, into its entries and
// participants. Rows sharing a message key are copies of one message: the
// message becomes one entry, and each copy names its peer, the sender of a
// received message or a recipient of the user's own, with the names and handles
// the source gives that peer.
func assemble(rows []repository.ThreadContentRow, attendees []uuid.UUID) (Context, error) {
	// A peer identity's contact is the one any of its copies is attributed to,
	// so the participant does not split when only some copies are matched.
	peerContacts := map[string]uuid.UUID{}
	for _, row := range rows {
		if row.PeerKey == "" || row.PeerContactID == nil {
			continue
		}
		if known, ok := peerContacts[row.PeerKey]; !ok || bytes.Compare(row.PeerContactID[:], known[:]) < 0 {
			peerContacts[row.PeerKey] = *row.PeerContactID
		}
	}

	me := &party{}
	var others []*party
	byKey := map[string]*party{}
	join := func(key string, contact *uuid.UUID) *party {
		if p, ok := byKey[key]; ok {
			return p
		}
		p := &party{key: key, contact: contact}
		byKey[key] = p
		others = append(others, p)
		return p
	}
	byContact := func(id uuid.UUID) *party { return join("contact:"+id.String(), &id) }
	// peer returns the participant a copy names as its peer, and nil when the
	// copy names none.
	peer := func(row repository.ThreadContentRow) *party {
		if contact, ok := peerContacts[row.PeerKey]; ok {
			return byContact(contact)
		}
		if row.PeerContactID != nil {
			return byContact(*row.PeerContactID)
		}
		if row.PeerKey != "" {
			return join("source:"+row.PeerKey, nil)
		}
		return nil
	}

	type message struct {
		key     string
		from    *party
		sentAt  time.Time
		headers map[string]string
		text    string
	}
	var messages []message
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && rows[end].MessageKey == rows[start].MessageKey {
			end++
		}
		first := rows[start]
		from := me
		if !first.FromUser {
			from = peer(first)
		}
		for _, copy := range rows[start:end] {
			me.aliases = append(me.aliases, copy.UserAliases...)
			if p := peer(copy); p != nil {
				p.aliases = append(p.aliases, copy.PeerAliases...)
			}
		}
		headers, err := renderHeaders(first.Headers)
		if err != nil {
			return Context{}, err
		}
		messages = append(messages, message{key: first.MessageKey, from: from, sentAt: first.SentAt, headers: headers, text: first.Body})
		start = end
	}
	for _, id := range attendees {
		byContact(id)
	}

	roles := map[*party]Role{me: RoleMe}
	participants := map[Role]Participant{RoleMe: {IsUser: true, Aliases: uniqueSorted(me.aliases)}}
	for i, p := range others {
		role := PersonRole(i + 2)
		if len(others) == 1 {
			role = RoleThem
		}
		roles[p] = role
		participants[role] = Participant{ContactID: p.contact, Aliases: uniqueSorted(p.aliases)}
	}
	entries := make([]Entry, len(messages))
	for i, m := range messages {
		entries[i] = Entry{Key: m.key, Role: roles[m.from], SentAt: m.sentAt, Headers: m.headers, Text: m.text}
	}
	hash, err := inputHash(entries, participants)
	if err != nil {
		return Context{}, err
	}
	return Context{Entries: entries, Participants: participants, InputHash: hash}, nil
}

// storedHeaders is the header object the thread_content view carries.
type storedHeaders struct {
	From     string   `json:"from"`
	FromName string   `json:"from_name"`
	To       []string `json:"to"`
	ToNames  []string `json:"to_names"`
	Cc       []string `json:"cc"`
	CcNames  []string `json:"cc_names"`
	Bcc      []string `json:"bcc"`
	BccNames []string `json:"bcc_names"`
	Subject  string   `json:"subject"`
	Title    string   `json:"title"`
}

// renderHeaders renders a message's stored headers as header lines, pairing
// each address with its display name. It returns nil when there are none.
func renderHeaders(raw []byte) (map[string]string, error) {
	var stored storedHeaders
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &stored); err != nil {
			return nil, fmt.Errorf("decode thread content headers: %w", err)
		}
	}
	headers := map[string]string{}
	set := func(name, value string) {
		if value != "" {
			headers[name] = value
		}
	}
	set("from", address(stored.FromName, stored.From))
	set("to", addresses(stored.ToNames, stored.To))
	set("cc", addresses(stored.CcNames, stored.Cc))
	set("bcc", addresses(stored.BccNames, stored.Bcc))
	set("subject", stored.Subject)
	set("title", stored.Title)
	if len(headers) == 0 {
		return nil, nil
	}
	return headers, nil
}

func address(name, addr string) string {
	if name == "" || addr == "" {
		return name + addr
	}
	return name + " <" + addr + ">"
}

func addresses(names, addrs []string) string {
	out := make([]string, len(addrs))
	for i, addr := range addrs {
		name := ""
		if i < len(names) {
			name = names[i]
		}
		out[i] = address(name, addr)
	}
	return strings.Join(out, ", ")
}

func uniqueSorted(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}

// inputHash identifies the entries and each participant's matched contact,
// which decides the participant's questions and the contact their outcomes
// land on, so a participant matched, unmatched or rematched gives a new hash.
// It leaves out the thread and the cut-off, so equal content and bindings up
// to a cut-off give an equal hash whatever later reads it.
func inputHash(entries []Entry, participants map[Role]Participant) (string, error) {
	type hashed struct {
		Role    Role              `json:"role"`
		SentAt  string            `json:"sent_at"`
		Headers map[string]string `json:"headers"`
		Text    string            `json:"text"`
	}
	type binding struct {
		Role    Role       `json:"role"`
		Contact *uuid.UUID `json:"contact"`
	}
	canonical := struct {
		Entries  []hashed  `json:"entries"`
		Bindings []binding `json:"bindings"`
	}{Entries: make([]hashed, len(entries))}
	for i, e := range entries {
		canonical.Entries[i] = hashed{Role: e.Role, SentAt: e.SentAt.UTC().Format(time.RFC3339Nano), Headers: e.Headers, Text: e.Text}
	}
	for _, role := range slices.Sorted(maps.Keys(participants)) {
		canonical.Bindings = append(canonical.Bindings, binding{Role: role, Contact: participants[role].ContactID})
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode input hash: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
