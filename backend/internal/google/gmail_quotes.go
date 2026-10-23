package google

import (
	"strings"

	"golang.org/x/net/html"
)

// quoteCap is how many characters of quoted mail an email keeps: a thread
// start's quoted history, or a forward's forwarded content. A quote starts with
// its newest mail, so the cap keeps the quote's first characters.
const quoteCap = 10000

// QuoteTrimmedMarker is the line that follows quoted mail the cap cut short.
const QuoteTrimmedMarker = "[Older quoted mail trimmed]"

// mailKind is how an email's quoted mail is stored (ING-047).
type mailKind int

const (
	// mailReply drops the quoted mail, since each earlier message of the
	// thread is stored as its own message.
	mailReply mailKind = iota
	// mailThreadStart stores the quoted mail apart from the body, as the
	// thread's history the mailbox holds no other copy of.
	mailThreadStart
	// mailForward keeps the forwarded mail in the body, as its content.
	mailForward
)

// mailKindOf tells a Gmail message's kind from its ids and subject. Gmail
// gives a thread the id of the message that started it in that mailbox, so an
// introduction received fresh starts a thread even though it carries earlier
// mail. An unknown message or thread id counts as a start. A body that carries
// a forward introduction is a forward whatever its kind here (storedText).
func mailKindOf(gmailMessageID, threadID, subject string) mailKind {
	s := strings.ToLower(strings.TrimSpace(subject))
	switch {
	case strings.HasPrefix(s, "fwd:") || strings.HasPrefix(s, "fw:"):
		return mailForward
	case gmailMessageID == "" || threadID == "" || threadID == gmailMessageID:
		return mailThreadStart
	default:
		return mailReply
	}
}

// emailText is what an email stores of its text: the body, and the quoted
// history apart from it, "" for none.
type emailText struct {
	body   string
	quoted string
}

// storedText returns the text an email stores, from its text/plain part, or
// from its text/html part stripped to text when it has no plain part. The body
// is the message's own text, up to where ownTextEnd finds its quoted mail. A
// thread start stores that quoted mail apart as its quoted history, a forward
// keeps it in the body as its content, and a reply drops it. Quoted mail kept
// either way is capped (capQuote). A body with nothing to cut is stored
// unchanged, or as stripHTML's text.
func storedText(plain, htmlBody string, kind mailKind) emailText {
	var b splitBody
	switch {
	case plain != "":
		b = splitPlain(plain)
	case htmlBody != "":
		b = splitHTML(htmlBody)
	default:
		return emailText{}
	}
	if b.forward {
		kind = mailForward
	}
	if b.carried == "" {
		return emailText{body: b.whole}
	}
	switch kind {
	case mailThreadStart:
		quoted, _ := capQuote(b.carried)
		return emailText{body: b.own, quoted: quoted}
	case mailForward:
		carried, cut := capQuote(b.carried)
		if !cut {
			return emailText{body: b.whole}
		}
		return emailText{body: strings.TrimLeft(b.own+"\n\n"+carried, "\n")}
	default:
		return emailText{body: b.own}
	}
}

// splitBody is an email body split where its own text ends. own is the text
// before the split and carried the quoted or forwarded mail after it, one line
// per line; whole is the body's text when nothing is cut. forward reports a
// split at a forward introduction line.
type splitBody struct {
	own, carried, whole string
	forward             bool
}

// splitAt returns the index of the line where a body's own text ends: its
// forward introduction line, else ownTextEnd's boundary.
func splitAt(lines []bodyLine) (end int, forward bool) {
	if i := forwardLine(lines); i >= 0 {
		return i, true
	}
	return ownTextEnd(lines), false
}

// splitPlain splits a plain-text body, where a quoted line starts with ">".
func splitPlain(s string) splitBody {
	raw := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	lines := make([]bodyLine, len(raw))
	for i, line := range raw {
		lines[i] = bodyLine{text: line, quoted: strings.HasPrefix(strings.TrimSpace(line), ">")}
	}
	end, forward := splitAt(lines)
	return splitBody{
		own:     strings.TrimRight(strings.Join(raw[:end], "\n"), " \t\n"),
		carried: strings.TrimSpace(strings.Join(raw[end:], "\n")),
		whole:   s,
		forward: forward,
	}
}

// splitHTML splits an HTML body. Text inside a blockquote or a Yahoo quote
// container is quoted; block elements and <br> break lines. The own text is
// one line, its words joined by single spaces, as stripHTML gives them; the
// carried mail keeps a line per line.
func splitHTML(s string) splitBody {
	lines := htmlLines(s)
	end, forward := splitAt(lines)
	own := make([]string, end)
	for i, line := range lines[:end] {
		own[i] = line.text
	}
	var carried []string
	for _, line := range lines[end:] {
		if text := strings.Join(strings.Fields(line.text), " "); text != "" {
			carried = append(carried, text)
		}
	}
	return splitBody{
		own:     strings.Join(strings.Fields(strings.Join(own, " ")), " "),
		carried: strings.Join(carried, "\n"),
		whole:   stripHTML(s),
		forward: forward,
	}
}

// capQuote caps quoted mail at quoteCap characters. A longer quote keeps its
// first quoteCap characters, cut back to the last line break among them, then
// a line holding QuoteTrimmedMarker; a quote with no line break there is cut
// at the cap. It reports whether it cut. A capped quote caps to itself.
func capQuote(s string) (string, bool) {
	runes := []rune(s)
	if len(runes) <= quoteCap {
		return s, false
	}
	kept := string(runes[:quoteCap])
	if runes[quoteCap] != '\n' {
		if i := strings.LastIndexByte(kept, '\n'); i >= 0 {
			kept = kept[:i]
		}
	}
	return strings.TrimRight(kept, " \t\n") + "\n" + QuoteTrimmedMarker, true
}

// bodyLine is one line of a message body and whether it is quoted.
type bodyLine struct {
	text   string
	quoted bool
}

// ownTextEnd returns how many leading lines are the message's own text. An
// Outlook original-message header ends it, and below that the trailing block
// of quoted lines, with the "... wrote:" attribution that introduces it, is
// not the message's own; quoted lines followed by new text (an inline reply)
// are. It returns len(lines) when there is no quoted mail.
func ownTextEnd(lines []bodyLine) int {
	end := len(lines)
	if i := outlookHeaderLine(lines); i >= 0 {
		end = i
	}
	cut := end
	quoted := false
scan:
	for i := end - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i].text)
		switch {
		case line == "":
		case lines[i].quoted:
			quoted = true
		case quoted && strings.HasSuffix(line, "wrote:"):
			// Gmail wraps a long attribution, leaving "On <date> <name>" on
			// the line above the one that ends in "wrote:".
			if !strings.HasPrefix(line, "On ") && i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1].text), "On ") {
				i--
			}
		default:
			break scan
		}
		cut = i
	}
	if !quoted {
		return end
	}
	return cut
}

// outlookHeaderLine returns the index of the line where an Outlook reply's
// quoted original begins, or -1. Outlook quotes without ">" markers or a
// blockquote, behind an "-----Original Message-----" line or a header block
// whose "From:" line is followed by "Sent:". An underscore rule directly above
// the header is part of it. A line already quoted is never the boundary.
func outlookHeaderLine(lines []bodyLine) int {
	for i, l := range lines {
		if l.quoted {
			continue
		}
		line := strings.TrimSpace(l.text)
		if strings.EqualFold(line, "-----Original Message-----") {
			return i
		}
		if strings.HasPrefix(line, "From:") && i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1].text), "Sent:") {
			if i > 0 {
				if above := strings.TrimSpace(lines[i-1].text); above != "" && strings.Trim(above, "_") == "" {
					return i - 1
				}
			}
			return i
		}
	}
	return -1
}

// forwardLine returns the index of the line that introduces a forwarded
// message, or -1: an unquoted line that is a client's forward introduction,
// Gmail's and Thunderbird's "---------- Forwarded message ---------" or Apple
// Mail's "Begin forwarded message:". The forwarded message is the body's
// content, its own quotes and headers included.
func forwardLine(lines []bodyLine) int {
	for i, l := range lines {
		if l.quoted {
			continue
		}
		// Fields collapses the spaces HTML tags and &nbsp; leave inside a line.
		intro := strings.ToLower(strings.Trim(strings.Join(strings.Fields(l.text), " "), "-: "))
		if intro == "forwarded message" || intro == "begin forwarded message" {
			return i
		}
	}
	return -1
}

// htmlLineBreaks are the elements that start a new line of text.
var htmlLineBreaks = map[string]bool{
	"br": true, "p": true, "div": true, "blockquote": true, "hr": true, "li": true, "tr": true,
	"table": true, "ul": true, "ol": true, "pre": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// htmlVoid are the elements that never hold content, so they open no scope.
var htmlVoid = map[string]bool{
	"br": true, "hr": true, "img": true, "meta": true, "link": true, "input": true,
	"area": true, "base": true, "col": true, "embed": true, "source": true, "track": true, "wbr": true,
}

// htmlLines splits an HTML body into lines of text, marking each line whose
// text sits inside a quote container.
func htmlLines(s string) []bodyLine {
	type scope struct {
		tag    string
		quoted bool
	}
	var open []scope
	depth := 0
	lines := []bodyLine{{}}
	newLine := func() {
		if strings.TrimSpace(lines[len(lines)-1].text) != "" {
			lines = append(lines, bodyLine{})
		}
	}
	z := html.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return lines
		case html.TextToken:
			text := string(z.Text())
			cur := &lines[len(lines)-1]
			if strings.TrimSpace(cur.text) == "" {
				cur.quoted = depth > 0
			}
			cur.text += " " + text
		case html.StartTagToken:
			tok := z.Token()
			if htmlLineBreaks[tok.Data] {
				newLine()
			}
			if htmlVoid[tok.Data] {
				continue
			}
			quoted := tok.Data == "blockquote" || hasClass(tok, "yahoo_quoted")
			open = append(open, scope{tag: tok.Data, quoted: quoted})
			if quoted {
				depth++
			}
		case html.SelfClosingTagToken:
			if tok := z.Token(); htmlLineBreaks[tok.Data] {
				newLine()
			}
		case html.EndTagToken:
			tok := z.Token()
			if htmlLineBreaks[tok.Data] {
				newLine()
			}
			// Close the innermost open element of this name and everything
			// opened inside it; an unmatched end tag closes nothing.
			for i := len(open) - 1; i >= 0; i-- {
				if open[i].tag != tok.Data {
					continue
				}
				for _, sc := range open[i:] {
					if sc.quoted {
						depth--
					}
				}
				open = open[:i]
				break
			}
		}
	}
}

// hasClass reports whether a start tag's class attribute lists class.
func hasClass(tok html.Token, class string) bool {
	for _, attr := range tok.Attr {
		if attr.Key == "class" && strings.Contains(" "+attr.Val+" ", " "+class+" ") {
			return true
		}
	}
	return false
}
