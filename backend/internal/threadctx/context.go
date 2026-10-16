// Package threadctx builds a thread's content up to a cut-off as one thread
// context, and masks it for a hosted decision model.
//
// A thread is a venue node. Conversation threads are venues of kind dm,
// group_chat and email_thread; meeting threads are venues of kind meeting and
// session.
package threadctx

import (
	"strconv"
	"time"

	"github.com/google/uuid"
)

// VenueID identifies a thread: its venue node id.
type VenueID = uuid.UUID

// ContextKind is what a thread context holds.
type ContextKind string

const (
	// KindConversation is a one-to-one chat, a group chat or an email thread.
	KindConversation ContextKind = "conversation"
	// KindMeeting is a meeting's notes.
	KindMeeting ContextKind = "meeting"
)

// Role labels one participant within one thread context.
type Role string

const (
	// RoleMe is the user.
	RoleMe Role = "ME"
	// RoleThem is the other participant of a thread with exactly one.
	RoleThem Role = "THEM"
)

// PersonRole is the role of the n-th participant other than the user in a
// thread with several, n counting from 2.
func PersonRole(n int) Role { return Role("PERSON_" + strconv.Itoa(n)) }

// Entry is one message of a thread context. A meeting note is an entry the
// user wrote.
type Entry struct {
	// Key is the message's key in the thread's content, equal for every
	// per-contact copy of it. It is a source identifier, so it stays on the Pi:
	// a masked entry carries none.
	Key     string
	Role    Role
	SentAt  time.Time
	Headers map[string]string
	Text    string
}

// Participant is one party of a thread context.
type Participant struct {
	// ContactID is the participant's matched CRM contact; nil when the
	// participant is not a contact.
	ContactID *uuid.UUID
	IsUser    bool
	// Aliases are the names and handles the sources give the participant. They
	// let the masker replace them with the participant's role; they never decide
	// the role.
	Aliases []string
}

// Context is a thread's whole content up to a cut-off.
type Context struct {
	Thread       VenueID
	Kind         ContextKind
	CutOff       time.Time
	Entries      []Entry
	Participants map[Role]Participant
	// InputHash identifies the content up to the cut-off and each
	// participant's matched contact, and nothing else.
	InputHash string
}
