package google

import (
	"strings"

	"golang.org/x/net/html"
)

// keepsQuotedMail reports whether a Gmail message is stored whole, quoted
// earlier mail included: the message that starts its thread, or a forward. Gmail
// gives a thread the id of the message that started it in that mailbox, so an
// introduction received fresh starts a thread even though it carries earlier
// mail. A forward's carried message is its content. An unknown message or
// thread id counts as a start.
func keepsQuotedMail(gmailMessageID, threadID, subject string) bool {
	if gmailMessageID == "" || threadID == "" || threadID == gmailMessageID {
		return true
	}
	s := strings.ToLower(strings.TrimSpace(subject))
	return strings.HasPrefix(s, "fwd:") || strings.HasPrefix(s, "fw:")
}

// messageText returns the text stored as an email's body: the text/plain part,
// or the text/html part stripped to text when the message has no plain part. A
// message that does not keep its quoted mail drops the earlier messages it
// quotes, since each of those is stored as its own message (ING-047).
func messageText(plain, htmlBody string, keepQuoted bool) string {
	if plain != "" {
		if keepQuoted {
			return plain
		}
		return trimQuotedText(plain)
	}
	if htmlBody == "" {
		return ""
	}
	if keepQuoted {
		return stripHTML(htmlBody)
	}
	return trimQuotedHTML(htmlBody)
}

// bodyLine is one line of a message body and whether it is quoted.
type bodyLine struct {
	text   string
	quoted bool
}

// ownTextEnd returns how many leading lines are the message's own text. A body
// that carries a forwarded message is all its own. Otherwise an Outlook
// original-message header ends it, and below that the trailing block of quoted
// lines, with the "... wrote:" attribution that introduces it, is not the
// message's own; quoted lines followed by new text (an inline reply) are. It
// returns len(lines) when there is nothing to drop.
func ownTextEnd(lines []bodyLine) int {
	if carriesForward(lines) {
		return len(lines)
	}
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

// carriesForward reports whether a body carries a forwarded message: an
// unquoted line is a client's forward introduction, Gmail's and Thunderbird's
// "---------- Forwarded message ---------" or Apple Mail's "Begin forwarded
// message:". The forwarded message is the body's content, its own quotes and
// headers included.
func carriesForward(lines []bodyLine) bool {
	for _, l := range lines {
		if l.quoted {
			continue
		}
		// Fields collapses the spaces HTML tags and &nbsp; leave inside a line.
		intro := strings.ToLower(strings.Trim(strings.Join(strings.Fields(l.text), " "), "-: "))
		if intro == "forwarded message" || intro == "begin forwarded message" {
			return true
		}
	}
	return false
}

// trimQuotedText removes the quoted earlier messages from a plain-text body,
// where a quoted line starts with ">". A body with nothing to remove is
// returned unchanged.
func trimQuotedText(s string) string {
	raw := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	lines := make([]bodyLine, len(raw))
	for i, line := range raw {
		lines[i] = bodyLine{text: line, quoted: strings.HasPrefix(strings.TrimSpace(line), ">")}
	}
	end := ownTextEnd(lines)
	if end == len(lines) {
		return s
	}
	return strings.TrimRight(strings.Join(raw[:end], "\n"), " \t\n")
}

// trimQuotedHTML removes the quoted earlier messages from an HTML body and
// returns its text as stripHTML would. Text inside a blockquote or a Yahoo
// quote container is quoted; block elements and <br> break lines. A body with
// nothing to remove is stripHTML's text.
func trimQuotedHTML(s string) string {
	lines := htmlLines(s)
	end := ownTextEnd(lines)
	if end == len(lines) {
		return stripHTML(s)
	}
	texts := make([]string, end)
	for i, line := range lines[:end] {
		texts[i] = line.text
	}
	return strings.Join(strings.Fields(strings.Join(texts, " ")), " ")
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
