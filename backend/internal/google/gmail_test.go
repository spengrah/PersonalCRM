package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/gmail/v1"
)

// --- test helpers ---

// b64url encodes s as a Gmail-style base64url string (with padding).
func b64url(s string) string {
	return base64.URLEncoding.EncodeToString([]byte(s))
}

// header builds a gmail header.
func header(name, value string) *gmail.MessagePartHeader {
	return &gmail.MessagePartHeader{Name: name, Value: value}
}

// plainTextPart builds a text/plain leaf part.
func plainTextPart(body string) *gmail.MessagePart {
	return &gmail.MessagePart{
		MimeType: "text/plain",
		Body:     &gmail.MessagePartBody{Data: b64url(body), Size: int64(len(body))},
	}
}

// htmlPart builds a text/html leaf part.
func htmlPart(body string) *gmail.MessagePart {
	return &gmail.MessagePart{
		MimeType: "text/html",
		Body:     &gmail.MessagePartBody{Data: b64url(body), Size: int64(len(body))},
	}
}

// attachmentPart builds an attachment leaf part (filename + attachmentId, no
// inline content).
func attachmentPart(filename, mime string, size int64) *gmail.MessagePart {
	return &gmail.MessagePart{
		MimeType: mime,
		Filename: filename,
		Body:     &gmail.MessagePartBody{AttachmentId: "att-" + filename, Size: size},
	}
}

// fakeFetcher is an in-memory gmailFetcher. listPages maps a query → ordered
// list pages (each a slice of refs); messages maps id → message. getCalls
// records per-id GetMessage call counts. getErrIDs forces GetMessage to error
// for the given ids.
type fakeFetcher struct {
	listPages map[string][][]gmailMessageRef
	messages  map[string]*gmail.Message
	getCalls  map[string]int
	getErrIDs map[string]struct{}
	listErr   error
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		listPages: map[string][][]gmailMessageRef{},
		messages:  map[string]*gmail.Message{},
		getCalls:  map[string]int{},
		getErrIDs: map[string]struct{}{},
	}
}

func (f *fakeFetcher) ListMessageIDs(_ context.Context, query, pageToken string) ([]gmailMessageRef, string, error) {
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	pages := f.listPages[query]
	idx := 0
	if pageToken != "" {
		// pageToken encodes the page index as "p<n>".
		if _, err := fmt.Sscanf(pageToken, "p%d", &idx); err != nil {
			return nil, "", fmt.Errorf("bad page token %q: %w", pageToken, err)
		}
	}
	if idx >= len(pages) {
		return nil, "", nil
	}
	next := ""
	if idx+1 < len(pages) {
		next = fmt.Sprintf("p%d", idx+1)
	}
	return pages[idx], next, nil
}

func (f *fakeFetcher) GetMessage(_ context.Context, id string) (*gmail.Message, error) {
	f.getCalls[id]++
	if _, bad := f.getErrIDs[id]; bad {
		return nil, fmt.Errorf("forced get error for %s", id)
	}
	msg, ok := f.messages[id]
	if !ok {
		return nil, fmt.Errorf("message %s not found", id)
	}
	return msg, nil
}

// --- Config ---

func TestGmailSyncProvider_Config(t *testing.T) {
	p := NewGmailSyncProvider(nil, nil, nil, nil)
	cfg := p.Config()
	require.Equal(t, GmailSourceName, cfg.Name)
	require.Equal(t, "email", cfg.Name)
	require.Equal(t, "Gmail", cfg.DisplayName)
	require.True(t, cfg.SupportsMultiAccount)
	require.False(t, cfg.SupportsDiscovery)
	require.Equal(t, GmailDefaultInterval, cfg.DefaultInterval)
}

// --- OR-chunk byte budgeting + sanitization ---

func TestBuildORChunks_BasicShapeAndPrefix(t *testing.T) {
	chunks := buildORChunks([]string{"b@example.com", "a@example.com"}, 1700000000)
	require.Len(t, chunks, 1)
	q := chunks[0]
	require.True(t, strings.HasPrefix(q, "-category:promotions -category:social -category:updates -category:forums after:1700000000 ("), "got %q", q)
	// Sorted: a before b.
	require.Less(t, strings.Index(q, "a@example.com"), strings.Index(q, "b@example.com"))
	require.Contains(t, q, "(from:a@example.com OR to:a@example.com OR cc:a@example.com OR bcc:a@example.com)")
	require.LessOrEqual(t, len(url.QueryEscape(q)), gmailChunkByteCap)
}

func TestBuildORChunks_EmptyList_NoChunks(t *testing.T) {
	require.Empty(t, buildORChunks(nil, 1700000000))
	require.Empty(t, buildORChunks([]string{}, 1700000000))
}

func TestBuildORChunks_Deterministic(t *testing.T) {
	addrs := []string{"c@example.com", "a@example.com", "b@example.com"}
	first := buildORChunks(addrs, 1700000000)
	second := buildORChunks([]string{"b@example.com", "c@example.com", "a@example.com"}, 1700000000)
	require.Equal(t, first, second)
}

func TestBuildORChunks_CrossingCapForcesNewChunk(t *testing.T) {
	// Build enough addresses that the (k+1)-th group tips over the cap.
	var addrs []string
	for i := 0; i < 200; i++ {
		addrs = append(addrs, fmt.Sprintf("user%03d@example.com", i))
	}
	chunks := buildORChunks(addrs, 1700000000)
	require.Greater(t, len(chunks), 1, "expected the address set to span multiple chunks")
	for _, c := range chunks {
		require.LessOrEqual(t, len(url.QueryEscape(c)), gmailChunkByteCap, "chunk exceeds byte cap: %q", c)
	}
	// Every address appears in exactly one chunk.
	joined := strings.Join(chunks, " ")
	for _, a := range addrs {
		require.Equal(t, 1, strings.Count(joined, "from:"+a+" "), "address %s should appear once", a)
	}
}

func TestBuildORChunks_SingleOversizedGroupGetsOwnChunk(t *testing.T) {
	// A pathologically long (but email-safe) local part whose single group
	// alone exceeds the cap still forms one chunk (groups are never split).
	long := strings.Repeat("a", gmailChunkByteCap) + "@example.com"
	chunks := buildORChunks([]string{long}, 1700000000)
	require.Len(t, chunks, 1)
	require.Contains(t, chunks[0], "from:"+long)
}

func TestBuildORChunks_SanitizesMalformedAddresses(t *testing.T) {
	addrs := []string{
		"clean@example.com",
		"has space@example.com", // whitespace → dropped
		"paren(@example.com",    // ( → dropped
		"quote\"@example.com",   // " → dropped
		"",                      // empty → dropped
		"second@example.com",
	}
	chunks := buildORChunks(addrs, 1700000000)
	require.Len(t, chunks, 1)
	q := chunks[0]
	require.Contains(t, q, "from:clean@example.com")
	require.Contains(t, q, "from:second@example.com")
	require.NotContains(t, q, "has space")
	require.NotContains(t, q, "paren(")
	require.NotContains(t, q, "quote\"")
}

func TestSanitizeAddresses_OnlyMalformed_Empty(t *testing.T) {
	require.Empty(t, sanitizeAddresses([]string{"a b@example.com", "x()@example.com", "  "}))
}

// --- MIME body extraction ---

func TestExtractContent_PlainTextOnly(t *testing.T) {
	body, htmlBody, atts, err := extractContent(plainTextPart("hello world"))
	require.NoError(t, err)
	require.Equal(t, "hello world", body)
	require.Empty(t, htmlBody)
	require.Empty(t, atts)
}

func TestExtractContent_HTMLOnly_StripsToBody_RetainsHTML(t *testing.T) {
	raw := "<html><body><p>Hi &amp; bye</p><br></body></html>"
	body, htmlBody, _, err := extractContent(htmlPart(raw))
	require.NoError(t, err)
	require.Equal(t, "Hi & bye", body)
	require.Equal(t, raw, htmlBody)
}

func TestExtractContent_MultipartAlternative_PrefersPlain_RetainsHTML(t *testing.T) {
	payload := &gmail.MessagePart{
		MimeType: "multipart/alternative",
		Parts: []*gmail.MessagePart{
			plainTextPart("plain body"),
			htmlPart("<p>html body</p>"),
		},
	}
	body, htmlBody, _, err := extractContent(payload)
	require.NoError(t, err)
	require.Equal(t, "plain body", body)
	require.Equal(t, "<p>html body</p>", htmlBody)
}

func TestExtractContent_MultipartMixed_BodyPlusAttachment(t *testing.T) {
	payload := &gmail.MessagePart{
		MimeType: "multipart/mixed",
		Parts: []*gmail.MessagePart{
			plainTextPart("see attached"),
			attachmentPart("report.pdf", "application/pdf", 2048),
		},
	}
	body, _, atts, err := extractContent(payload)
	require.NoError(t, err)
	require.Equal(t, "see attached", body)
	require.Len(t, atts, 1)
	require.Equal(t, attachmentMeta{Filename: "report.pdf", MimeType: "application/pdf", Size: 2048}, atts[0])
}

func TestExtractContent_NestedMultipartRecursion(t *testing.T) {
	payload := &gmail.MessagePart{
		MimeType: "multipart/mixed",
		Parts: []*gmail.MessagePart{
			{
				MimeType: "multipart/alternative",
				Parts: []*gmail.MessagePart{
					plainTextPart("nested plain"),
					htmlPart("<p>nested html</p>"),
				},
			},
			attachmentPart("a.png", "image/png", 99),
		},
	}
	body, htmlBody, atts, err := extractContent(payload)
	require.NoError(t, err)
	require.Equal(t, "nested plain", body)
	require.Equal(t, "<p>nested html</p>", htmlBody)
	require.Len(t, atts, 1)
}

func TestExtractContent_Base64URL_MissingPaddingTolerated(t *testing.T) {
	// "hi!" base64url is "aGkh" (no padding needed); use a value that requires
	// padding then strip it to exercise the RawURLEncoding fallback.
	full := base64.URLEncoding.EncodeToString([]byte("padme"))
	require.Contains(t, full, "=", "test fixture should require padding")
	stripped := strings.TrimRight(full, "=")
	part := &gmail.MessagePart{
		MimeType: "text/plain",
		Body:     &gmail.MessagePartBody{Data: stripped},
	}
	body, _, _, err := extractContent(part)
	require.NoError(t, err)
	require.Equal(t, "padme", body)
}

func TestExtractContent_NilPayload(t *testing.T) {
	body, htmlBody, atts, err := extractContent(nil)
	require.NoError(t, err)
	require.Empty(t, body)
	require.Empty(t, htmlBody)
	require.Empty(t, atts)
}

func TestExtractContent_AttachmentIDWithoutFilename_StillCollected(t *testing.T) {
	// Inline images and some forwarded parts carry an AttachmentId but no
	// filename — they must still be recorded as attachment metadata and must
	// NOT be treated as the message body.
	payload := &gmail.MessagePart{
		MimeType: "multipart/mixed",
		Parts: []*gmail.MessagePart{
			plainTextPart("body text"),
			{
				MimeType: "image/png",
				Body:     &gmail.MessagePartBody{AttachmentId: "att-inline-1", Size: 512},
			},
		},
	}
	body, _, atts, err := extractContent(payload)
	require.NoError(t, err)
	require.Equal(t, "body text", body)
	require.Len(t, atts, 1)
	require.Equal(t, "image/png", atts[0].MimeType)
	require.Equal(t, int64(512), atts[0].Size)
	require.Empty(t, atts[0].Filename)
}

// --- Message-ID extraction + fallback ---

func TestExtractExternalID_PresentTrimmed(t *testing.T) {
	h := newHeaderLookup(&gmail.MessagePart{Headers: []*gmail.MessagePartHeader{
		header("Message-ID", "  <abc123@mail.example.com>  "),
	}})
	require.Equal(t, "abc123@mail.example.com", extractExternalID(h, "me@example.com", "gmail-1"))
}

func TestExtractExternalID_AbsentFallback(t *testing.T) {
	h := newHeaderLookup(&gmail.MessagePart{Headers: nil})
	require.Equal(t, "nomsgid:me@example.com:gmail-1", extractExternalID(h, "me@example.com", "gmail-1"))
}

func TestExtractExternalID_CaseInsensitiveHeaderName(t *testing.T) {
	h := newHeaderLookup(&gmail.MessagePart{Headers: []*gmail.MessagePartHeader{
		header("message-id", "<lower@example.com>"),
	}})
	require.Equal(t, "lower@example.com", extractExternalID(h, "me@example.com", "gmail-1"))
}

// --- participant/direction rule ---

func buildMessage(t *testing.T, id, threadID, from string, to, cc, bcc []string, subject, body, msgID string, internalDateMillis int64) *gmail.Message {
	t.Helper()
	headers := []*gmail.MessagePartHeader{header("From", from)}
	if len(to) > 0 {
		headers = append(headers, header("To", strings.Join(to, ", ")))
	}
	if len(cc) > 0 {
		headers = append(headers, header("Cc", strings.Join(cc, ", ")))
	}
	if len(bcc) > 0 {
		headers = append(headers, header("Bcc", strings.Join(bcc, ", ")))
	}
	if subject != "" {
		headers = append(headers, header("Subject", subject))
	}
	if msgID != "" {
		headers = append(headers, header("Message-ID", msgID))
	}
	return &gmail.Message{
		Id:           id,
		ThreadId:     threadID,
		InternalDate: internalDateMillis,
		Snippet:      "snippet",
		LabelIds:     []string{"INBOX", "CATEGORY_PERSONAL"},
		Payload: &gmail.MessagePart{
			MimeType: "text/plain",
			Headers:  headers,
			Body:     &gmail.MessagePartBody{Data: b64url(body), Size: int64(len(body))},
		},
	}
}

func newProviderForResolution() *GmailSyncProvider {
	return NewGmailSyncProvider(nil, nil, nil, nil)
}

func TestProcessMessage_Inbound(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
	meSet := map[string]struct{}{"me@example.com": {}}
	msg := buildMessage(t, "g1", "t1", "Contact A <contact@example.com>",
		[]string{"me@example.com"}, nil, nil, "Hi", "hello", "<m1@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "inbound", rows[0].Direction)
	require.Equal(t, contactA, rows[0].ContactID)
	require.Equal(t, "contact@example.com", rows[0].PeerNormalized)
	require.Equal(t, "m1@example.com", rows[0].ExternalID)
	require.Equal(t, "hello", *rows[0].Body)
}

func TestProcessMessage_Outbound_To(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
	meSet := map[string]struct{}{"me@example.com": {}}
	msg := buildMessage(t, "g1", "t1", "me@example.com",
		[]string{"contact@example.com"}, nil, nil, "Re", "yo", "<m2@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "outbound", rows[0].Direction)
	require.Equal(t, "contact@example.com", rows[0].PeerNormalized)
}

func TestProcessMessage_Outbound_ViaCcAndBcc(t *testing.T) {
	p := newProviderForResolution()
	meSet := map[string]struct{}{"me@example.com": {}}

	t.Run("cc", func(t *testing.T) {
		contactA := uuid.New()
		knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
		msg := buildMessage(t, "g1", "t1", "me@example.com",
			[]string{"other@example.com"}, []string{"contact@example.com"}, nil, "S", "b", "<c1@example.com>", 1700000000000)
		rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, "outbound", rows[0].Direction)
		require.Equal(t, "contact@example.com", rows[0].PeerNormalized)
	})

	t.Run("bcc", func(t *testing.T) {
		contactA := uuid.New()
		knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
		msg := buildMessage(t, "g2", "t2", "me@example.com",
			[]string{"other@example.com"}, nil, []string{"contact@example.com"}, "S", "b", "<c2@example.com>", 1700000000000)
		rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, "outbound", rows[0].Direction)
		require.Equal(t, "contact@example.com", rows[0].PeerNormalized)
	})
}

func TestProcessMessage_GroupOutbound_TwoContacts(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	contactB := uuid.New()
	knownMap := map[string][]uuid.UUID{
		"a@example.com": {contactA},
		"b@example.com": {contactB},
	}
	meSet := map[string]struct{}{"me@example.com": {}}
	msg := buildMessage(t, "g1", "t1", "me@example.com",
		[]string{"a@example.com", "b@example.com"}, nil, nil, "S", "hi both", "<g@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	dirs := map[uuid.UUID]string{}
	peers := map[uuid.UUID]string{}
	for _, r := range rows {
		dirs[r.ContactID] = r.Direction
		peers[r.ContactID] = r.PeerNormalized
	}
	require.Equal(t, "outbound", dirs[contactA])
	require.Equal(t, "outbound", dirs[contactB])
	require.Equal(t, "a@example.com", peers[contactA])
	require.Equal(t, "b@example.com", peers[contactB])
}

func TestProcessMessage_Bystander_Skipped(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	// Known contact only co-Cc'd by a third party; thread not to/from me.
	knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
	meSet := map[string]struct{}{"me@example.com": {}}
	msg := buildMessage(t, "g1", "t1", "third@example.com",
		[]string{"other@example.com"}, []string{"contact@example.com"}, nil, "S", "b", "<by@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestProcessMessage_InboundForA_RecipientBNotQualifying(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	contactB := uuid.New()
	// from=contactA, To={me, contactB}: A is inbound (A wrote, I received).
	// B is a recipient with from ∉ A_B and from ∉ M → B is a bystander.
	knownMap := map[string][]uuid.UUID{
		"a@example.com": {contactA},
		"b@example.com": {contactB},
	}
	meSet := map[string]struct{}{"me@example.com": {}}
	msg := buildMessage(t, "g1", "t1", "a@example.com",
		[]string{"me@example.com", "b@example.com"}, nil, nil, "S", "b", "<ab@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, contactA, rows[0].ContactID)
	require.Equal(t, "inbound", rows[0].Direction)
}

// --- ambiguous shared-address fan-out ---

func TestProcessMessage_AmbiguousSharedAddress_FansOut(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	contactB := uuid.New()
	knownMap := map[string][]uuid.UUID{"shared@example.com": {contactA, contactB}}
	meSet := map[string]struct{}{"me@example.com": {}}
	msg := buildMessage(t, "g1", "t1", "shared@example.com",
		[]string{"me@example.com"}, nil, nil, "S", "b", "<sh@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	ids := map[uuid.UUID]bool{}
	for _, r := range rows {
		require.Equal(t, "inbound", r.Direction)
		require.Equal(t, "sh@example.com", r.ExternalID)
		ids[r.ContactID] = true
	}
	require.True(t, ids[contactA])
	require.True(t, ids[contactB])
}

// --- display-name parsing + peer-handle precedence ---

func TestProcessMessage_PeerHandlePrecedence_ToWinsOverCc(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	// Contact has two aliases; one in To, one in Cc. To must win.
	knownMap := map[string][]uuid.UUID{
		"primary@example.com": {contactA},
		"alias@example.com":   {contactA},
	}
	meSet := map[string]struct{}{"me@example.com": {}}
	msg := buildMessage(t, "g1", "t1", "me@example.com",
		[]string{"primary@example.com"}, []string{"alias@example.com"}, nil, "S", "b", "<pp@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "primary@example.com", rows[0].PeerNormalized)
}

func TestProcessMessage_PeerHandlePrecedence_FirstListedWinsInBucket(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	knownMap := map[string][]uuid.UUID{
		"first@example.com":  {contactA},
		"second@example.com": {contactA},
	}
	meSet := map[string]struct{}{"me@example.com": {}}
	// Both in To; first listed must win.
	msg := buildMessage(t, "g1", "t1", "me@example.com",
		[]string{"first@example.com", "second@example.com"}, nil, nil, "S", "b", "<fl@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "first@example.com", rows[0].PeerNormalized)
}

func TestProcessMessage_DisplayNameHeaders_Parsed(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
	meSet := map[string]struct{}{"me@example.com": {}}
	msg := buildMessage(t, "g1", "t1", "\"Contact A\" <contact@example.com>",
		[]string{"\"My Self\" <me@example.com>"}, nil, nil, "S", "b", "<dn@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "inbound", rows[0].Direction)
	require.Equal(t, "contact@example.com", rows[0].PeerHandle)
	require.Equal(t, "contact@example.com", rows[0].PeerNormalized)
}

func TestProcessMessage_DedupAddressInToAndCc(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
	meSet := map[string]struct{}{"me@example.com": {}}
	// contact appears in both To and Cc → one outbound row.
	msg := buildMessage(t, "g1", "t1", "me@example.com",
		[]string{"contact@example.com"}, []string{"contact@example.com"}, nil, "S", "b", "<dd@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestProcessMessage_NomsgidFallbackInResolution(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
	meSet := map[string]struct{}{"me@example.com": {}}
	msg := buildMessage(t, "gmail-xyz", "t1", "contact@example.com",
		[]string{"me@example.com"}, nil, nil, "S", "b", "", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "nomsgid:me@example.com:gmail-xyz", rows[0].ExternalID)
}

func TestProcessMessage_MetadataCarriesAttachmentsAndHTML(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
	meSet := map[string]struct{}{"me@example.com": {}}
	msg := &gmail.Message{
		Id:           "g1",
		ThreadId:     "t1",
		InternalDate: 1700000000000,
		LabelIds:     []string{"INBOX"},
		Payload: &gmail.MessagePart{
			MimeType: "multipart/mixed",
			Headers: []*gmail.MessagePartHeader{
				header("From", "contact@example.com"),
				header("To", "me@example.com"),
				header("Message-ID", "<meta@example.com>"),
			},
			Parts: []*gmail.MessagePart{
				htmlPart("<p>body</p>"),
				attachmentPart("file.pdf", "application/pdf", 10),
			},
		},
	}
	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	var meta emailMetadata
	require.NoError(t, json.Unmarshal(rows[0].Metadata, &meta))
	require.Equal(t, "<p>body</p>", meta.HTML)
	require.Len(t, meta.Attachments, 1)
	require.Equal(t, "file.pdf", meta.Attachments[0].Filename)
	require.Equal(t, []string{"INBOX"}, meta.Labels)
	require.Equal(t, "contact@example.com", meta.From)
	require.Equal(t, []string{"me@example.com"}, meta.To)
}

func TestProcessMessage_MetadataCapturesDisplayNames(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
	meSet := map[string]struct{}{"me@example.com": {}}
	// From carries a display name; To has two recipients, one with a name and
	// one bare. The metadata must store bare addresses unchanged plus aligned
	// display-name slices.
	msg := buildMessage(t, "g1", "t1", "\"Contact A\" <contact@example.com>",
		[]string{"\"My Self\" <me@example.com>", "bare@example.com"}, nil, nil,
		"S", "b", "<dn@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	var meta emailMetadata
	require.NoError(t, json.Unmarshal(rows[0].Metadata, &meta))
	// Bare addresses unchanged.
	require.Equal(t, "contact@example.com", meta.From)
	require.Equal(t, []string{"me@example.com", "bare@example.com"}, meta.To)
	// Display names captured, index-aligned.
	require.Equal(t, "Contact A", meta.FromName)
	require.Equal(t, []string{"My Self", ""}, meta.ToNames)
	require.Len(t, meta.ToNames, len(meta.To))
}

func TestProcessMessage_MetadataNoDisplayNameStoresEmpty(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()
	knownMap := map[string][]uuid.UUID{"contact@example.com": {contactA}}
	meSet := map[string]struct{}{"me@example.com": {}}
	// No display parts anywhere.
	msg := buildMessage(t, "g1", "t1", "contact@example.com",
		[]string{"me@example.com"}, nil, nil, "S", "b", "<nodn@example.com>", 1700000000000)

	rows, err := p.RunProcessMessageForTest(context.Background(), msg, "me@example.com", knownMap, meSet)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	var meta emailMetadata
	require.NoError(t, json.Unmarshal(rows[0].Metadata, &meta))
	require.Equal(t, "", meta.FromName)
	// The name slice stays index-aligned with the address slice: one recipient
	// with no display part → one empty-string name. The producer treats an
	// empty name as "no display name → skip at the ≥2-token gate".
	require.Equal(t, []string{""}, meta.ToNames)
	require.Len(t, meta.ToNames, len(meta.To))

	// The from_name key is ALWAYS written (even empty) so a bare-From row
	// ingested after capture is not mistaken for a pre-capture row by the
	// re-derivation predicate (NOT ? 'from_name').
	var rawMeta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rows[0].Metadata, &rawMeta))
	_, present := rawMeta["from_name"]
	require.True(t, present, "from_name key must be present even when empty")
}

func TestParseSingleAddress_Name(t *testing.T) {
	raw, norm, name := parseSingleAddress("\"Contact A\" <Contact.A@Example.com>")
	require.Equal(t, "Contact.A@Example.com", raw)
	require.Equal(t, "contact.a@example.com", norm)
	require.Equal(t, "Contact A", name)

	raw, norm, name = parseSingleAddress("bare@example.com")
	require.Equal(t, "bare@example.com", raw)
	require.Equal(t, "bare@example.com", norm)
	require.Equal(t, "", name)

	raw, norm, name = parseSingleAddress("")
	require.Equal(t, "", raw)
	require.Equal(t, "", norm)
	require.Equal(t, "", name)
}

func TestParseAddressList_NamesIndexAligned(t *testing.T) {
	// Happy path: ParseAddressList succeeds; names aligned to addresses.
	raws, norms, names := parseAddressList("\"First Last\" <first@example.com>, bare@example.com")
	require.Equal(t, []string{"first@example.com", "bare@example.com"}, raws)
	require.Equal(t, []string{"first@example.com", "bare@example.com"}, norms)
	require.Equal(t, []string{"First Last", ""}, names)
	require.Len(t, names, len(raws))

	// Empty header → all nil.
	raws, norms, names = parseAddressList("")
	require.Nil(t, raws)
	require.Nil(t, norms)
	require.Nil(t, names)
}

func TestParseAddressList_CommaSplitFallbackAligned(t *testing.T) {
	// A malformed entry forces the lenient comma-split fallback. The name/raw
	// slices must stay the same length so the producer never mis-pairs a name
	// to the wrong address.
	raws, _, names := parseAddressList("\"Valid Name\" <ok@example.com>, @@@broken")
	require.Len(t, names, len(raws))
	require.GreaterOrEqual(t, len(raws), 1)
}

// --- local_day boundary in time.Local (incl. DST) ---

func TestComputeLocalDay_PureForm_AcrossMidnight(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	// 2026-03-01 23:30 local is still 2026-03-01 even though UTC has rolled to 03-02.
	beforeMidnight := time.Date(2026, 3, 1, 23, 30, 0, 0, loc)
	afterMidnight := time.Date(2026, 3, 2, 0, 30, 0, 0, loc)
	require.Equal(t, "2026-03-01", computeLocalDay(beforeMidnight.UTC(), loc))
	require.Equal(t, "2026-03-02", computeLocalDay(afterMidnight.UTC(), loc))
}

func TestComputeLocalDay_DST_SpringForwardAndFallBack(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	// Spring forward 2026: 2026-03-08, clocks jump 2 AM → 3 AM.
	springInstant := time.Date(2026, 3, 8, 3, 30, 0, 0, loc)
	require.Equal(t, "2026-03-08", computeLocalDay(springInstant.UTC(), loc))

	// Fall back 2026: 2026-11-01, 1 AM repeats. A 23:30-local instant stays on
	// the civil day even though its UTC date has rolled over.
	fallInstant := time.Date(2026, 11, 1, 23, 30, 0, 0, loc)
	require.Equal(t, "2026-11-01", computeLocalDay(fallInstant.UTC(), loc))
}

func TestComputeLocalDay_ReadsGlobalTimeLocal(t *testing.T) {
	// This variant exercises the time.Local code path the provider uses.
	// time.Local is process-global, so this test is intentionally NOT parallel.
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	orig := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = orig })

	instant := time.Date(2026, 11, 1, 23, 30, 0, 0, loc).UTC()
	require.Equal(t, "2026-11-01", computeLocalDay(instant, time.Local))
}

// --- cursor helpers ---

func TestComputeNewCursor_FetchedAdvancesMonotonic(t *testing.T) {
	// max(maxInternalDate, prior) — never below prior.
	require.Equal(t, "200", computeNewCursor(true, 200, 100, "100", 50))
	require.Equal(t, "100", computeNewCursor(true, 80, 100, "100", 50), "out-of-order older message must not pull floor back")
}

func TestComputeNewCursor_ZeroFetched_PreservesPriorCursor(t *testing.T) {
	require.Equal(t, "12345", computeNewCursor(false, 0, 12345, "12345", 50))
}

func TestComputeNewCursor_ZeroFetched_EmptyPrior_WritesBackfillEpoch(t *testing.T) {
	require.Equal(t, "1735689600", computeNewCursor(false, 0, 0, "", 1735689600))
}

func TestResolveAfterFloor_NumericCursor(t *testing.T) {
	prior, after := resolveAfterFloor("1700000000", nil)
	require.Equal(t, int64(1700000000), prior)
	require.Equal(t, int64(1700000000), after)
}

func TestResolveAfterFloor_EmptyCursor_UsesBackfillSince(t *testing.T) {
	expected := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	prior, after := resolveAfterFloor("", nil)
	require.Equal(t, int64(0), prior)
	require.Equal(t, expected, after)
}

func TestResolveAfterFloor_EmptyCursor_MetadataOverride(t *testing.T) {
	expected := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC).Unix()
	prior, after := resolveAfterFloor("", map[string]any{"backfill_since": "2026-03-15"})
	require.Equal(t, int64(0), prior)
	require.Equal(t, expected, after)
}

// --- cross-chunk seen dedup ---
//
// The per-id `seen` set lives inside Sync, whose body fetch + persist need a
// real bus + pool, so the end-to-end "body fetched once across chunks"
// assertion lives in the integration test (TestGmailProvider_CrossChunkSeenDedup).
// Here we unit-test the precondition that makes that scenario reachable: the
// chunk builder splits a large address set across multiple OR-chunks, and the
// fake fetcher hands the SAME message id back from more than one chunk query —
// which is exactly when Sync's seen set must suppress the duplicate body fetch.

func TestCrossChunkDedup_Precondition_SameIDFromTwoChunks(t *testing.T) {
	// A large address set forces multiple OR-chunks.
	var addrs []string
	for i := 0; i < 200; i++ {
		addrs = append(addrs, fmt.Sprintf("user%03d@example.com", i))
	}
	chunks := buildORChunks(addrs, 1700000000)
	require.Greater(t, len(chunks), 1, "address set must span multiple chunks")

	// A message returned by both the first and the last chunk query.
	f := newFakeFetcher()
	f.messages["g-span"] = &gmail.Message{Id: "g-span"}
	f.listPages[chunks[0]] = [][]gmailMessageRef{{{ID: "g-span"}}}
	f.listPages[chunks[len(chunks)-1]] = [][]gmailMessageRef{{{ID: "g-span"}}}

	// Both chunk queries hand back the same id (the cross-chunk duplicate Sync's
	// seen set must collapse to a single GetMessage).
	page0, _, err := f.ListMessageIDs(context.Background(), chunks[0], "")
	require.NoError(t, err)
	pageN, _, err := f.ListMessageIDs(context.Background(), chunks[len(chunks)-1], "")
	require.NoError(t, err)
	require.Len(t, page0, 1)
	require.Len(t, pageN, 1)
	require.Equal(t, page0[0].ID, pageN[0].ID)
}
