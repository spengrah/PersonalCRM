package google

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/gmail/v1"
)

func TestTrimQuotedText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "gmail top-posted reply",
			in:   "Sounds good, see you then.\n\nOn Mon, Oct 20, 2026 at 3:14 PM Contact A <a@example.com> wrote:\n\n> Want to meet Tuesday?\n> \n> > Earlier quote\n",
			want: "Sounds good, see you then.",
		},
		{
			name: "attribution wrapped onto two lines",
			in:   "Yes.\r\n\r\nOn Mon, Oct 20, 2026 at 3:14 PM Contact A With A Long Name <\r\na@example.com> wrote:\r\n> Are you in?\r\n",
			want: "Yes.",
		},
		{
			name: "signature above the attribution stays",
			in:   "Thanks!\n-- \nMe\n\nOn Tue, Contact A wrote:\n> hi\n",
			want: "Thanks!\n-- \nMe",
		},
		{
			name: "non-English attribution line stays, quote goes",
			in:   "Ja.\n\nAm Mo., 20. Okt. 2026 schrieb Contact A:\n> Kommst du?\n",
			want: "Ja.\n\nAm Mo., 20. Okt. 2026 schrieb Contact A:",
		},
		{
			name: "outlook original message",
			in:   "Confirmed.\n\n-----Original Message-----\nFrom: Contact A\nSent: Monday\nSubject: Plan\n\nAre we on?",
			want: "Confirmed.",
		},
		{
			name: "outlook header block under an underscore rule",
			in:   "Confirmed.\n\n________________________________\nFrom: Contact A <a@example.com>\nSent: Monday, October 20, 2026 3:14 PM\nTo: Me\nSubject: Plan\n\nAre we on?",
			want: "Confirmed.",
		},
		{
			name: "gmail forward header is not an outlook header",
			in:   "FYI\n\n---------- Forwarded message ---------\nFrom: Contact A <a@example.com>\nDate: Mon, Oct 20, 2026\nSubject: Plan\n\nAre we on?",
			want: "FYI\n\n---------- Forwarded message ---------\nFrom: Contact A <a@example.com>\nDate: Mon, Oct 20, 2026\nSubject: Plan\n\nAre we on?",
		},
		{
			name: "inline reply keeps quotes followed by new text",
			in:   "> First question?\nFirst answer.\n> Second question?\nSecond answer.",
			want: "> First question?\nFirst answer.\n> Second question?\nSecond answer.",
		},
		{
			name: "wrote line without a quote below stays",
			in:   "Here is what I wrote:\nthe draft",
			want: "Here is what I wrote:\nthe draft",
		},
		{
			name: "no quote returns the body byte for byte",
			in:   "plain reply\n\n",
			want: "plain reply\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, trimQuotedText(tc.in))
			require.Equal(t, tc.want, trimQuotedText(tc.want), "trimming is idempotent")
		})
	}
}

func TestTrimQuotedHTML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "gmail quote container",
			in:   `<div dir="ltr">Sounds good</div><br><div class="gmail_quote gmail_quote_container"><div class="gmail_attr">On Mon, Contact A wrote:</div><blockquote>Meet?</blockquote></div>`,
			want: `<div dir="ltr">Sounds good</div><br>`,
		},
		{
			name: "apple mail blockquote",
			in:   `<div>Yes</div><div><br><blockquote type="cite">Are you in?</blockquote></div>`,
			want: `<div>Yes</div><div><br>`,
		},
		{
			name: "outlook reply header",
			in:   `<p>Confirmed</p><div id="appendonsend"></div><hr><div id="divRplyFwdMsg">From: Contact A</div><div>Are we on?</div>`,
			want: `<p>Confirmed</p>`,
		},
		{
			name: "no marker unchanged",
			in:   `<p>hello</p>`,
			want: `<p>hello</p>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, trimQuotedHTML(tc.in))
		})
	}
}

func TestStartsThread(t *testing.T) {
	require.True(t, startsThread("g1", "g1"))
	require.True(t, startsThread("g1", ""))
	require.True(t, startsThread("", "g1"))
	require.False(t, startsThread("g2", "g1"))
}

// processQuotedMessage runs one inbound message with the given payload through
// processMessage and returns its stored body.
func processQuotedMessage(t *testing.T, gmailID, threadID string, payload *gmail.MessagePart) string {
	t.Helper()
	contactA := uuid.New()
	knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
	meSet := map[string]struct{}{"me@example.com": {}}
	msg := buildMessage(t, gmailID, threadID, "Contact A <contact@example.com>",
		[]string{"me@example.com"}, nil, nil, "Re: Plan", "", "<m1@example.com>", 1700000000000)
	payload.Headers = msg.Payload.Headers
	msg.Payload = payload

	rows, err := newProviderForResolution().RunProcessMessageForTest(t.Context(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Body)
	return *rows[0].Body
}

func TestProcessMessage_QuotedMail(t *testing.T) {
	const quoted = "Sounds good.\n\nOn Mon, Oct 20, 2026 at 3:14 PM Contact A <contact@example.com> wrote:\n> Meet Tuesday?\n"

	t.Run("reply drops the trailing quote", func(t *testing.T) {
		// spec: ING-047.reply-drops-trailing-quote
		require.Equal(t, "Sounds good.", processQuotedMessage(t, "g2", "g1", plainTextPart(quoted)))
	})

	t.Run("reply drops an outlook original", func(t *testing.T) {
		// spec: ING-047.reply-drops-outlook-original
		body := "Confirmed.\n\n-----Original Message-----\nFrom: Contact A\nSent: Monday\n\nAre we on?"
		require.Equal(t, "Confirmed.", processQuotedMessage(t, "g2", "g1", plainTextPart(body)))
	})

	t.Run("html-only reply keeps the text before the quote", func(t *testing.T) {
		// spec: ING-047.html-only-reply-drops-quote
		raw := `<div>Sounds good.</div><div class="gmail_quote"><div class="gmail_attr">On Mon, Contact A wrote:</div><blockquote>Meet Tuesday?</blockquote></div>`
		require.Equal(t, "Sounds good.", processQuotedMessage(t, "g2", "g1", htmlPart(raw)))
	})

	t.Run("inline reply keeps its quotes", func(t *testing.T) {
		// spec: ING-047.inline-reply-keeps-quotes
		body := "> Tuesday?\nYes.\n> Where?\nThe cafe."
		require.Equal(t, body, processQuotedMessage(t, "g2", "g1", plainTextPart(body)))
	})

	t.Run("thread start keeps everything", func(t *testing.T) {
		// spec: ING-047.thread-start-kept-whole
		require.Equal(t, quoted, processQuotedMessage(t, "g1", "g1", plainTextPart(quoted)))
		raw := `<div>FYI</div><div class="gmail_quote">---------- Forwarded message ---------<br>Plan details</div>`
		require.Equal(t, "FYI ---------- Forwarded message --------- Plan details", processQuotedMessage(t, "g1", "g1", htmlPart(raw)))
	})

	t.Run("unquoted reply unchanged", func(t *testing.T) {
		// spec: ING-047.unquoted-body-unchanged
		require.Equal(t, "Just a reply.\n", processQuotedMessage(t, "g2", "g1", plainTextPart("Just a reply.\n")))
	})
}
