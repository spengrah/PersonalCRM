package google

import (
	"strings"
)

// startsThread reports whether a Gmail message is the first message of its
// thread. Gmail gives a thread the id of the message that started it in that
// mailbox, so a forward or an introduction received fresh starts a thread even
// though it carries earlier mail. An unknown message or thread id counts as a
// start, which keeps the message whole.
func startsThread(gmailMessageID, threadID string) bool {
	return gmailMessageID == "" || threadID == "" || threadID == gmailMessageID
}

// messageText returns the text stored as an email's body: the text/plain part,
// or the text/html part stripped to text when the message has no plain part. A
// reply drops the earlier messages it quotes, since each of those is stored as
// its own message; the message that starts a thread keeps everything it carries
// (ING-047).
func messageText(plain, htmlBody string, threadStart bool) string {
	if plain != "" {
		if threadStart {
			return plain
		}
		return trimQuotedText(plain)
	}
	if htmlBody == "" {
		return ""
	}
	if threadStart {
		return stripHTML(htmlBody)
	}
	return stripHTML(trimQuotedHTML(htmlBody))
}

// trimQuotedText removes the quoted earlier messages from a plain-text reply:
// an Outlook original-message header and everything after it, then the
// trailing block of ">" lines with the "... wrote:" line that introduces it.
// Quoted lines followed by new text (an inline reply) stay. A body with
// nothing to remove is returned unchanged.
func trimQuotedText(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	end := len(lines)
	if i := outlookHeaderLine(lines); i >= 0 {
		end = i
	}

	// Walk up from the end while the lines are blank, quoted, or the
	// attribution above a quote; lines[cut:end] is the trailing quoted block.
	cut := end
	quoted := false
scan:
	for i := end - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		switch {
		case line == "":
		case strings.HasPrefix(line, ">"):
			quoted = true
		case quoted && strings.HasSuffix(line, "wrote:"):
			// Gmail wraps a long attribution, leaving "On <date> <name>" on
			// the line above the one that ends in "wrote:".
			if !strings.HasPrefix(line, "On ") && i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "On ") {
				i--
			}
		default:
			break scan
		}
		cut = i
	}
	if !quoted {
		cut = end
	}
	if cut == len(lines) {
		return s
	}
	return strings.TrimRight(strings.Join(lines[:cut], "\n"), " \t\n")
}

// outlookHeaderLine returns the index of the line where an Outlook reply's
// quoted original begins, or -1. Outlook quotes without ">" markers, behind an
// "-----Original Message-----" line or a header block whose "From:" line is
// followed by "Sent:" (a Gmail forward's header uses "Date:", so a forward is
// never cut). An underscore rule directly above the header is part of it.
func outlookHeaderLine(lines []string) int {
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.EqualFold(line, "-----Original Message-----") {
			return i
		}
		if strings.HasPrefix(line, "From:") && i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "Sent:") {
			if i > 0 {
				if above := strings.TrimSpace(lines[i-1]); above != "" && strings.Trim(above, "_") == "" {
					return i - 1
				}
			}
			return i
		}
	}
	return -1
}

// htmlQuoteMarkers are the lowercase markers at which a mail client's HTML
// reply begins its quoted original: Gmail's quote container, Apple Mail's and
// Thunderbird's blockquote, Outlook's reply header, and Yahoo's quote.
var htmlQuoteMarkers = []string{
	`class="gmail_quote`,
	`<blockquote`,
	`id="divrplyfwdmsg"`,
	`id="appendonsend"`,
	`class="yahoo_quoted`,
	`-----original message-----`,
}

// trimQuotedHTML cuts an HTML reply at its first quote marker, keeping what
// comes before it. A marker inside a tag cuts at that tag's "<". HTML with no
// marker is returned unchanged.
func trimQuotedHTML(s string) string {
	lower := strings.ToLower(s)
	cut := -1
	for _, marker := range htmlQuoteMarkers {
		if i := strings.Index(lower, marker); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return s
	}
	if tagStart := strings.LastIndex(lower[:cut], "<"); tagStart >= 0 && !strings.Contains(lower[tagStart:cut], ">") {
		cut = tagStart
	}
	return s[:cut]
}
