package service

import (
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func ptr[T any](v T) *T { return &v }

func TestInteractionContentDerivation_LabelPrecedence(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "Written label", deriveLabel(repository.Interaction{Source: "gcal", Description: ptr("Written label")}, ptr("Event title"), []weightedVenue{{tag: VenueTag{Key: "email:z", Label: "Venue"}, count: 3}}))
	assert.Equal(t, "Event title", deriveLabel(repository.Interaction{Source: "gcal"}, ptr("Event title"), nil))
	assert.Equal(t, "Zulu", deriveLabel(repository.Interaction{Source: "email"}, nil, []weightedVenue{{tag: VenueTag{Key: "email:a", Label: "Alpha"}, count: 1}, {tag: VenueTag{Key: "email:z", Label: "Zulu"}, count: 3}}))
	assert.Equal(t, "Alpha", deriveLabel(repository.Interaction{Source: "email", Description: ptr("")}, nil, []weightedVenue{{tag: VenueTag{Key: "email:z", Label: "Zulu"}, count: 2}, {tag: VenueTag{Key: "email:a", Label: "Alpha"}, count: 2}}))
	for source, want := range map[string]string{"phone_calls": "Phone call", "gcal": "Meeting", "anarlog_sessions": "Meeting", "manual": "Logged interaction", "todoist": "Todoist task", "telegram": "Messages"} {
		assert.Equal(t, want, deriveLabel(repository.Interaction{Source: source}, nil, nil), source)
	}
}

func TestInteractionContentDerivation_VenueKindLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, source, container, chatType string
		title, subject                    *string
		group                             bool
		want                              VenueTag
	}{
		{"gchat", "gchat", "space", "", nil, nil, false, VenueTag{Key: "gchat:space", Label: "Group chat", Kind: "group_chat", IsGroup: true}},
		{"whatsapp group", "whatsapp", "room@g.us", "", nil, nil, false, VenueTag{Key: "whatsapp:room@g.us", Label: "Group chat", Kind: "group_chat", IsGroup: true}},
		{"whatsapp dm", "whatsapp", "peer@s.whatsapp.net", "", nil, nil, false, VenueTag{Key: "whatsapp:peer@s.whatsapp.net", Label: "DM", Kind: "dm"}},
		{"email", "email", "thread", "", nil, ptr("Subject"), true, VenueTag{Key: "email:thread", Label: "Subject", Kind: "email_thread"}},
		{"email fallback", "email", "thread", "", nil, ptr(""), false, VenueTag{Key: "email:thread", Label: "Email thread", Kind: "email_thread"}},
		{"telegram dm", "telegram", "7", "private", ptr("Ignored"), nil, false, VenueTag{Key: "telegram:7", Label: "DM", Kind: "dm"}},
		{"telegram group", "telegram", "8", "group", ptr("Room"), nil, false, VenueTag{Key: "telegram:8", Label: "Room", Kind: "group_chat", IsGroup: true}},
		{"telegram fallback", "telegram", "9", "group", ptr(""), nil, false, VenueTag{Key: "telegram:9", Label: "Group chat", Kind: "group_chat", IsGroup: true}},
		{"messages dm", "messages", "chat", "", nil, nil, false, VenueTag{Key: "messages:chat", Label: "DM", Kind: "dm"}},
		{"messages group", "messages", "group", "", nil, nil, true, VenueTag{Key: "messages:group", Label: "Group chat", Kind: "group_chat", IsGroup: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, deriveVenue(tc.source, tc.container, tc.chatType, tc.title, tc.subject, tc.group))
		})
	}
}

func TestInteractionContentDerivation_Sender(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", stringValue(nil))
	assert.Equal(t, "Me", deriveSender("telegram", true, nil, nil, nil, nil, nil))
	assert.Equal(t, "First Last", deriveSender("telegram", false, nil, nil, ptr("First"), ptr("Last"), ptr("user")))
	assert.Equal(t, "First", deriveSender("telegram", false, nil, nil, ptr("First"), nil, ptr("user")))
	assert.Equal(t, "Last", deriveSender("telegram", false, nil, nil, nil, ptr("Last"), ptr("user")))
	assert.Equal(t, "user", deriveSender("telegram", false, nil, nil, nil, nil, ptr("user")))
	assert.Equal(t, "Push", deriveSender("whatsapp", false, ptr("peer"), []byte(`{"push_name":"Push"}`), nil, nil, nil))
	assert.Equal(t, "peer", deriveSender("whatsapp", false, ptr("peer"), nil, nil, nil, nil))
	assert.Equal(t, "peer", deriveSender("email", false, ptr("peer"), nil, nil, nil, nil))
	assert.Equal(t, "Unknown", deriveSender("email", false, nil, nil, nil, nil, nil))
}

// The `named` flag decides whether a matched contact's name may replace the
// derived sender, so it — not just the string — has to be pinned: a name the
// source supplied is authoritative, a raw identifier or a missing one is not.
func TestInteractionContentDerivation_SenderNamedFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		called func() (string, bool)
		want   bool
	}{
		{"outgoing", func() (string, bool) {
			return deriveSenderDetail("messages", true, ptr("peer"), nil, nil, nil, nil)
		}, true},
		{"telegram profile name", func() (string, bool) {
			return deriveSenderDetail("telegram", false, nil, nil, ptr("First"), ptr("Last"), nil)
		}, true},
		{"telegram username", func() (string, bool) {
			return deriveSenderDetail("telegram", false, nil, nil, nil, nil, ptr("user"))
		}, true},
		{"telegram nothing", func() (string, bool) {
			return deriveSenderDetail("telegram", false, nil, nil, nil, nil, nil)
		}, false},
		{"whatsapp push name", func() (string, bool) {
			return deriveSenderDetail("whatsapp", false, ptr("peer"), []byte(`{"push_name":"Push"}`), nil, nil, nil)
		}, true},
		{"whatsapp bare handle", func() (string, bool) {
			return deriveSenderDetail("whatsapp", false, ptr("peer"), nil, nil, nil, nil)
		}, false},
		{"imessage handle", func() (string, bool) {
			return deriveSenderDetail("messages", false, ptr("+15550000001"), nil, nil, nil, nil)
		}, false},
		{"no identifier at all", func() (string, bool) {
			return deriveSenderDetail("email", false, nil, nil, nil, nil, nil)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, named := tc.called()
			assert.Equal(t, tc.want, named)
		})
	}
}

func TestInteractionContentDerivation_SessionRefShape(t *testing.T) {
	t.Parallel()
	sessionID := uuid.New()
	contactID := uuid.New()
	cases := []struct {
		name string
		ref  string
		want bool
	}{
		{name: "valid", ref: "anarlog:" + sessionID.String() + ":" + contactID.String(), want: true},
		{name: "valid title suffix", ref: "anarlog:" + sessionID.String() + ":title:" + contactID.String(), want: true},
		{name: "valid walkin suffix", ref: "anarlog:" + sessionID.String() + ":walkin:" + contactID.String(), want: true},
		{name: "malformed prefix", ref: "other:" + sessionID.String() + ":" + contactID.String(), want: false},
		{name: "missing contact segment", ref: "anarlog:" + sessionID.String(), want: false},
		{name: "empty contact segment", ref: "anarlog:" + sessionID.String() + ":", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := tc.ref
			_, ok := parseSessionUUID(&ref)
			assert.Equal(t, tc.want, ok)
		})
	}
}
