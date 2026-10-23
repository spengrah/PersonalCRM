package google

import (
	"fmt"
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

// processQuoted runs one inbound message with the case's ids, subject and
// payload through processMessage and returns the row it stores.
func processQuoted(t *testing.T, tc quotedCase) qualifiedRow {
	t.Helper()
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
	return rows[0]
}

// runQuotedCases stores each case's message and asserts its body.
func runQuotedCases(t *testing.T, cases []quotedCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := processQuoted(t, tc)
			require.NotNil(t, row.Body)
			require.Equal(t, tc.want, *row.Body)
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
		reply("prose naming a forwarded message is not a forward",
			plainTextPart("Thanks for the forwarded message.\n\nOn Mon, Contact A wrote:\n> Here it is.\n"),
			"Thanks for the forwarded message."),
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

// threadStart is a message that starts its thread in the mailbox.
func threadStart(name string, payload *gmail.MessagePart) quotedCase {
	return quotedCase{name: name, gmailID: "g1", threadID: "g1", subject: "Intro", payload: payload}
}

// spec: ING-047.thread-start-quote-stored-apart
func TestQuotedMail_ThreadStartStoresQuoteApart(t *testing.T) {
	const plain = "Introducing you two.\n\nOn Mon, Contact A wrote:\n> Know anyone?\n"
	const quotedHTML = `<div>Introducing you two.</div><blockquote>Know anyone?</blockquote>`
	cases := []struct {
		quotedCase
		wantQuoted string
	}{
		{threadStart("first message, plain", plainTextPart(plain)), "On Mon, Contact A wrote:\n> Know anyone?"},
		{threadStart("first message, html", htmlPart(quotedHTML)), "Know anyone?"},
		{quotedCase{name: "unknown thread id", gmailID: "g2", threadID: "", subject: "Re: Intro", payload: plainTextPart(plain)}, "On Mon, Contact A wrote:\n> Know anyone?"},
		{quotedCase{name: "unknown message id", gmailID: "", threadID: "g1", subject: "Re: Intro", payload: htmlPart(quotedHTML)}, "Know anyone?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := processQuoted(t, tc.quotedCase)
			require.NotNil(t, row.Body)
			require.Equal(t, "Introducing you two.", *row.Body)
			require.NotNil(t, row.QuotedHistory)
			require.Equal(t, tc.wantQuoted, *row.QuotedHistory)
		})
	}
}

// quoteLines returns n quoted lines of width characters each, the line's
// number then the character c repeated.
func quoteLines(n, width int, c string) []string {
	return numberedLines("> ", n, width, c)
}

// numberedLines returns n lines of width characters each: the prefix, the
// line's number, then the character c repeated.
func numberedLines(prefix string, n, width int, c string) []string {
	lines := make([]string, n)
	for i := range lines {
		head := fmt.Sprintf("%s%03d ", prefix, i)
		lines[i] = head + strings.Repeat(c, width-len(head))
	}
	return lines
}

// spec: ING-047.quoted-history-capped
func TestQuotedMail_ThreadStartQuoteCapped(t *testing.T) {
	const own = "Introducing you two."
	const attribution = "On Mon, Contact A wrote:"
	// The attribution line and its break take 25 characters, and each quoted
	// line and its break 100, so line i starts at character 25+100i.
	lines := quoteLines(100, 99, "q")
	start := func(quote string) quotedCase {
		return threadStart("", plainTextPart(own+"\n\n"+quote+"\n"))
	}
	cases := []struct {
		name       string
		tc         quotedCase
		wantQuoted string
	}{
		{
			name:       "over the cap, cut back to the line break before the 10,000th character",
			tc:         start(attribution + "\n" + strings.Join(lines, "\n")),
			wantQuoted: attribution + "\n" + strings.Join(lines[:99], "\n") + "\n" + QuoteTrimmedMarker,
		},
		{
			name:       "over the cap, its 10,000th character ending a line",
			tc:         start(attribution + "\n" + strings.Join(lines[:99], "\n") + "\n> " + strings.Repeat("r", 73) + "\n> later"),
			wantQuoted: attribution + "\n" + strings.Join(lines[:99], "\n") + "\n> " + strings.Repeat("r", 73) + "\n" + QuoteTrimmedMarker,
		},
		{
			name:       "at the cap, kept whole",
			tc:         start(attribution + "\n" + strings.Join(lines[:99], "\n") + "\n> " + strings.Repeat("r", 73)),
			wantQuoted: attribution + "\n" + strings.Join(lines[:99], "\n") + "\n> " + strings.Repeat("r", 73),
		},
		{
			name:       "at the cap in characters, over it in bytes, kept whole",
			tc:         start(attribution + "\n" + strings.Join(quoteLines(99, 99, "é"), "\n") + "\n> " + strings.Repeat("é", 73)),
			wantQuoted: attribution + "\n" + strings.Join(quoteLines(99, 99, "é"), "\n") + "\n> " + strings.Repeat("é", 73),
		},
		{
			name: "html over the cap, a line per quoted line",
			tc: threadStart("", htmlPart("<div>"+own+"</div><blockquote><div>"+
				strings.Join(quoteLines(120, 99, "q"), "</div><div>")+"</div></blockquote>")),
			wantQuoted: strings.Join(quoteLines(100, 99, "q"), "\n") + "\n" + QuoteTrimmedMarker,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := processQuoted(t, c.tc)
			require.NotNil(t, row.Body)
			require.Equal(t, own, *row.Body)
			require.NotNil(t, row.QuotedHistory)
			require.Equal(t, c.wantQuoted, *row.QuotedHistory)
		})
	}
}

// spec: ING-047.forward-content-kept-capped
func TestQuotedMail_ForwardKeepsContentCapped(t *testing.T) {
	appleForward := "FYI\n\nBegin forwarded message:\n\nFrom: Contact A <a@example.com>\nSent: Monday, October 20, 2026\nSubject: Plan\n\n> Are we on?"
	runForwardCases(t, []quotedCase{
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
		reply("html introduction split by inline tags and nbsp",
			htmlPart(`<div>FYI</div><div>Begin <b>forwarded&nbsp;message:</b></div><div>From: Contact A</div><div>Sent: Monday</div><blockquote>Carried content.</blockquote>`),
			"FYI Begin forwarded message: From: Contact A Sent: Monday Carried content."),
		reply("thunderbird forward rule in a reply",
			plainTextPart("FYI\n\n-------- Forwarded Message --------\nSubject: Plan\n\n> Are we on?"),
			"FYI\n\n-------- Forwarded Message --------\nSubject: Plan\n\n> Are we on?"),
		reply("html outlook original inside a forwarded block",
			htmlPart(`<div>FYI</div><div>Begin forwarded message:</div><div>Carried content.</div><div>-----Original Message-----</div><div>Earlier.</div>`),
			"FYI Begin forwarded message: Carried content. -----Original Message----- Earlier."),
		reply("gmail html forward in a reply is not a quote",
			htmlPart(`<div>FYI</div><div class="gmail_quote">---------- Forwarded message ---------<br>From: Contact A<br>Date: Mon<br><br><div>Plan details</div></div>`),
			"FYI ---------- Forwarded message --------- From: Contact A Date: Mon Plan details"),
	})
}

// runForwardCases stores each case's message and asserts its body, and that it
// stores no quoted history apart.
func runForwardCases(t *testing.T, cases []quotedCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := processQuoted(t, tc)
			require.NotNil(t, row.Body)
			require.Equal(t, tc.want, *row.Body)
			require.Nil(t, row.QuotedHistory)
		})
	}
}

// spec: ING-047.forward-content-kept-capped
func TestQuotedMail_ForwardContentCapped(t *testing.T) {
	const intro = "---------- Forwarded message ---------"
	// The forwarded content's header takes 100 characters with its line
	// breaks, as does each carried line, so line i starts at character
	// 100(i+1) and the 10,000th character starts line 99.
	header := intro + "\nSubject: " + strings.Repeat("s", 51) + "\n"
	lines := numberedLines("", 120, 99, "c")
	kept := header + strings.Join(lines[:99], "\n") + "\n" + QuoteTrimmedMarker
	const attribution = "On Mon, Contact A wrote:"
	quoted := quoteLines(100, 99, "q")
	atCap := header + strings.Join(lines[:98], "\n") + "\n" + strings.Repeat("c", 100)
	runForwardCases(t, []quotedCase{
		{name: "plain forward over the cap", gmailID: "g2", threadID: "g2", subject: "Fwd: Plan",
			payload: plainTextPart("FYI\n\n" + header + strings.Join(lines, "\n") + "\n"),
			want:    "FYI\n\n" + kept},
		reply("html forward introduction in a reply, over the cap",
			htmlPart(`<div>FYI</div><div>`+intro+`</div><div>Subject: `+strings.Repeat("s", 51)+`</div><div>`+
				strings.Join(lines, "</div><div>")+`</div>`),
			"FYI\n\n"+kept),
		{name: "forward subject over a quote over the cap", gmailID: "g2", threadID: "g1", subject: "FW: Plan",
			payload: plainTextPart("FYI\n\n" + attribution + "\n" + strings.Join(quoted, "\n") + "\n"),
			want:    "FYI\n\n" + attribution + "\n" + strings.Join(quoted[:99], "\n") + "\n" + QuoteTrimmedMarker},
		{name: "plain forward at the cap, kept whole", gmailID: "g2", threadID: "g2", subject: "Fwd: Plan",
			payload: plainTextPart("FYI\n\n" + atCap + "\n"),
			want:    "FYI\n\n" + atCap + "\n"},
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
