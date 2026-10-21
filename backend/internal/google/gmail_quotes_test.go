package google

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/gmail/v1"
)

// quotedCase is one inbound message run through processMessage: its ids,
// subject and payload, and the body it must store.
type quotedCase struct {
	name     string
	gmailID  string
	threadID string
	subject  string
	payload  *gmail.MessagePart
	want     string
}

// reply is a message that is neither its thread's start nor a forward.
func reply(name string, payload *gmail.MessagePart, want string) quotedCase {
	return quotedCase{name: name, gmailID: "g2", threadID: "g1", subject: "Re: Plan", payload: payload, want: want}
}

// runQuotedCases stores each case's message and asserts its body.
func runQuotedCases(t *testing.T, cases []quotedCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contactA := uuid.New()
			knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
			meSet := map[string]struct{}{"me@example.com": {}}
			msg := buildMessage(t, tc.gmailID, tc.threadID, "Contact A <contact@example.com>",
				[]string{"me@example.com"}, nil, nil, tc.subject, "", "<m1@example.com>", 1700000000000)
			tc.payload.Headers = msg.Payload.Headers
			msg.Payload = tc.payload

			rows, err := newProviderForResolution().RunProcessMessageForTest(t.Context(), msg, "me@example.com", knownMap, meSet)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.NotNil(t, rows[0].Body)
			require.Equal(t, tc.want, *rows[0].Body)
		})
	}
}

// spec: ING-047.reply-drops-trailing-quote
func TestQuotedMail_ReplyDropsTrailingQuote(t *testing.T) {
	runQuotedCases(t, []quotedCase{
		reply("gmail top-posted reply",
			plainTextPart("Sounds good, see you then.\n\nOn Mon, Oct 20, 2026 at 3:14 PM Contact A <a@example.com> wrote:\n\n> Want to meet Tuesday?\n> \n> > Earlier quote\n"),
			"Sounds good, see you then."),
		reply("attribution wrapped onto two lines",
			plainTextPart("Yes.\r\n\r\nOn Mon, Oct 20, 2026 at 3:14 PM Contact A With A Long Name <\r\na@example.com> wrote:\r\n> Are you in?\r\n"),
			"Yes."),
		reply("signature above the attribution stays",
			plainTextPart("Thanks!\n-- \nMe\n\nOn Tue, Contact A wrote:\n> hi\n"),
			"Thanks!\n-- \nMe"),
		reply("non-English attribution line stays, quote goes",
			plainTextPart("Ja.\n\nAm Mo., 20. Okt. 2026 schrieb Contact A:\n> Kommst du?\n"),
			"Ja.\n\nAm Mo., 20. Okt. 2026 schrieb Contact A:"),
	})
}

// spec: ING-047.reply-drops-outlook-original
func TestQuotedMail_ReplyDropsOutlookOriginal(t *testing.T) {
	runQuotedCases(t, []quotedCase{
		reply("original message line",
			plainTextPart("Confirmed.\n\n-----Original Message-----\nFrom: Contact A\nSent: Monday\nSubject: Plan\n\nAre we on?"),
			"Confirmed."),
		reply("header block under an underscore rule",
			plainTextPart("Confirmed.\n\n________________________________\nFrom: Contact A <a@example.com>\nSent: Monday, October 20, 2026 3:14 PM\nTo: Me\nSubject: Plan\n\nAre we on?"),
			"Confirmed."),
	})
}

// spec: ING-047.html-only-reply-drops-quote
func TestQuotedMail_HTMLOnlyReplyDropsQuote(t *testing.T) {
	gmailQuote := `<div class="gmail_quote gmail_quote_container"><div dir="ltr" class="gmail_attr">On Mon, Oct 20, 2026 at 3:14 PM Contact A &lt;<a href="mailto:a@example.com">a@example.com</a>&gt; wrote:<br></div><blockquote class="gmail_quote">Meet Tuesday?</blockquote></div>`
	runQuotedCases(t, []quotedCase{
		reply("gmail quote container",
			htmlPart(`<div dir="ltr">Sounds good &amp; see you.</div><br>`+gmailQuote),
			"Sounds good & see you."),
		reply("apple mail blockquote",
			htmlPart(`<div>Yes</div><div><br><blockquote type="cite"><div>On Oct 20, 2026, at 3:14 PM, Contact A wrote:</div><div>Are you in?</div></blockquote></div>`),
			"Yes"),
		reply("outlook reply header",
			htmlPart(`<p>Confirmed</p><hr><div id="divRplyFwdMsg"><b>From:</b> Contact A<br><b>Sent:</b> Monday<br><b>Subject:</b> Plan</div><div>Are we on?</div>`),
			"Confirmed"),
		reply("yahoo quote container",
			htmlPart(`<div>Works for me</div><div class="yahoo_quoted"><div>On Monday, Contact A wrote:</div><div><div>Lunch?</div></div></div>`),
			"Works for me"),
		reply("non-ASCII text before the quote",
			htmlPart(`<div>İstanbul confirmed</div>`+gmailQuote),
			"İstanbul confirmed"),
		reply("text that changes byte length when lowercased",
			htmlPart(`<div>`+strings.Repeat("Ⱥ", 120)+`</div>`+gmailQuote),
			strings.Repeat("Ⱥ", 120)),
	})
}

// spec: ING-047.inline-reply-keeps-quotes
func TestQuotedMail_InlineReplyKeepsQuotes(t *testing.T) {
	runQuotedCases(t, []quotedCase{
		reply("plain inline answers",
			plainTextPart("> First question?\nFirst answer.\n> Second question?\nSecond answer."),
			"> First question?\nFirst answer.\n> Second question?\nSecond answer."),
		reply("html inline answers",
			htmlPart(`<div>Answers:</div><blockquote>Question one?</blockquote><div>First answer.</div><blockquote>Question two?</blockquote><div>Second answer.</div>`),
			"Answers: Question one? First answer. Question two? Second answer."),
		reply("outlook header inside the quote is not a boundary",
			htmlPart(`<div>Answers:</div><blockquote><div>From: Contact A</div><div>Sent: Monday</div><div>Question?</div></blockquote><div>New answer.</div>`),
			"Answers: From: Contact A Sent: Monday Question? New answer."),
		reply("plain outlook header inside the quote is not a boundary",
			plainTextPart("> From: Contact A\n> Sent: Monday\n> Question?\nNew answer."),
			"> From: Contact A\n> Sent: Monday\n> Question?\nNew answer."),
	})
}

// spec: ING-047.thread-start-kept-whole
func TestQuotedMail_ThreadStartKeptWhole(t *testing.T) {
	const quoted = "Introducing you two.\n\nOn Mon, Contact A wrote:\n> Know anyone?\n"
	const quotedHTML = `<div>Introducing you two.</div><blockquote>Know anyone?</blockquote>`
	runQuotedCases(t, []quotedCase{
		{name: "first message, plain", gmailID: "g1", threadID: "g1", subject: "Intro", payload: plainTextPart(quoted), want: quoted},
		{name: "first message, html", gmailID: "g1", threadID: "g1", subject: "Intro", payload: htmlPart(quotedHTML), want: "Introducing you two. Know anyone?"},
		{name: "unknown thread id, plain", gmailID: "g2", threadID: "", subject: "Re: Intro", payload: plainTextPart(quoted), want: quoted},
		{name: "unknown thread id, html", gmailID: "g2", threadID: "", subject: "Re: Intro", payload: htmlPart(quotedHTML), want: "Introducing you two. Know anyone?"},
		{name: "unknown message id, plain", gmailID: "", threadID: "g1", subject: "Re: Intro", payload: plainTextPart(quoted), want: quoted},
		{name: "unknown message id, html", gmailID: "", threadID: "g1", subject: "Re: Intro", payload: htmlPart(quotedHTML), want: "Introducing you two. Know anyone?"},
	})
}

// spec: ING-047.forward-kept-whole
func TestQuotedMail_ForwardKeptWhole(t *testing.T) {
	appleForward := "FYI\n\nBegin forwarded message:\n\nFrom: Contact A <a@example.com>\nSent: Monday, October 20, 2026\nSubject: Plan\n\n> Are we on?"
	runQuotedCases(t, []quotedCase{
		{name: "forward subject keeps a forwarded header block", gmailID: "g2", threadID: "g1", subject: "Fwd: Plan",
			payload: plainTextPart(appleForward), want: appleForward},
		{name: "outlook FW subject keeps quoted html", gmailID: "g2", threadID: "g1", subject: "FW: Plan",
			payload: htmlPart(`<div>FYI</div><blockquote>Are we on?</blockquote>`), want: "FYI Are we on?"},
		reply("gmail forward header in a reply is not an outlook header",
			plainTextPart("FYI\n\n---------- Forwarded message ---------\nFrom: Contact A <a@example.com>\nDate: Mon, Oct 20, 2026\nSubject: Plan\n\nAre we on?"),
			"FYI\n\n---------- Forwarded message ---------\nFrom: Contact A <a@example.com>\nDate: Mon, Oct 20, 2026\nSubject: Plan\n\nAre we on?"),
		reply("forwarded block with a Sent header in a reply",
			plainTextPart("FYI\n\nBegin forwarded message:\n\nFrom: Contact A <a@example.com>\nSent: Monday\nSubject: Plan\n\nCarried content."),
			"FYI\n\nBegin forwarded message:\n\nFrom: Contact A <a@example.com>\nSent: Monday\nSubject: Plan\n\nCarried content."),
		reply("html forwarded block with a Sent header in a reply",
			htmlPart(`<div>FYI</div><div>Begin forwarded message:</div><div><b>From:</b> Contact A</div><div><b>Sent:</b> Monday</div><div>Carried content.</div>`),
			"FYI Begin forwarded message: From: Contact A Sent: Monday Carried content."),
		reply("quoted lines inside a forwarded block",
			plainTextPart("FYI\n\nBegin forwarded message:\n\nFrom: Contact A <a@example.com>\nSent: Monday\nSubject: Plan\n\n> Are we on?"),
			"FYI\n\nBegin forwarded message:\n\nFrom: Contact A <a@example.com>\nSent: Monday\nSubject: Plan\n\n> Are we on?"),
		reply("html blockquote inside a forwarded block",
			htmlPart(`<div>FYI</div><div>Begin forwarded message:</div><div>From: Contact A</div><div>Sent: Monday</div><blockquote>Are we on?</blockquote>`),
			"FYI Begin forwarded message: From: Contact A Sent: Monday Are we on?"),
		reply("outlook original inside a forwarded block",
			plainTextPart("FYI\n\n---------- Forwarded message ---------\nFrom: Contact A\nDate: Mon\n\nCarried content.\n\n-----Original Message-----\nFrom: Contact B\nSent: Sunday\n\nEarlier."),
			"FYI\n\n---------- Forwarded message ---------\nFrom: Contact A\nDate: Mon\n\nCarried content.\n\n-----Original Message-----\nFrom: Contact B\nSent: Sunday\n\nEarlier."),
		reply("html outlook original inside a forwarded block",
			htmlPart(`<div>FYI</div><div>Begin forwarded message:</div><div>Carried content.</div><div>-----Original Message-----</div><div>Earlier.</div>`),
			"FYI Begin forwarded message: Carried content. -----Original Message----- Earlier."),
		reply("gmail html forward in a reply is not a quote",
			htmlPart(`<div>FYI</div><div class="gmail_quote">---------- Forwarded message ---------<br>From: Contact A<br>Date: Mon<br><br><div>Plan details</div></div>`),
			"FYI ---------- Forwarded message --------- From: Contact A Date: Mon Plan details"),
	})
}

// spec: ING-047.unquoted-body-unchanged
func TestQuotedMail_UnquotedBodyUnchanged(t *testing.T) {
	runQuotedCases(t, []quotedCase{
		reply("plain body byte for byte", plainTextPart("Just a reply.\n\n"), "Just a reply.\n\n"),
		reply("wrote line with no quote below", plainTextPart("Here is what I wrote:\nthe draft"), "Here is what I wrote:\nthe draft"),
		reply("html body as stripped text", htmlPart(`<p>Hi &amp; bye</p><br>`), "Hi & bye"),
	})
}
