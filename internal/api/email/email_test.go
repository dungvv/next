package email

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestViewPredicate(t *testing.T) {
	for _, view := range []string{"inbox", "sent", "drafts", "starred", "all", "important", "other"} {
		if _, ok := viewPredicate(view); !ok {
			t.Errorf("view %q should be supported", view)
		}
	}
	if _, ok := viewPredicate("user:CUSTOM_LABEL"); !ok {
		t.Error("user: views should be supported")
	}
	for _, view := range []string{"", "bogus", "user:", "inbox2"} {
		if _, ok := viewPredicate(view); ok {
			t.Errorf("view %q should be rejected", view)
		}
	}
}

func TestStripReplies(t *testing.T) {
	body := "Hello,\n\nthanks for the update.\n\nOn Mon, Jan 1, 2024 at 9:00 AM Jane <jane@x.com> wrote:\n> old stuff"
	got := stripReplies(body)
	if strings.Contains(got, "wrote:") || strings.Contains(got, "old stuff") {
		t.Errorf("quoted reply not stripped: %q", got)
	}
	if !strings.Contains(got, "thanks for the update") {
		t.Errorf("main body missing: %q", got)
	}
}

func TestComputeBodyReplylessPrefersHTML(t *testing.T) {
	html := `<div>Hi there</div><div class="gmail_quote"><blockquote>quoted</blockquote></div>`
	text := "plain fallback"
	got := computeBodyReplyless(&html, &text)
	if got == nil {
		t.Fatal("expected non-nil body")
	}
	if strings.Contains(*got, "quoted") {
		t.Errorf("gmail_quote content not stripped: %q", *got)
	}
	if !strings.Contains(*got, "Hi there") {
		t.Errorf("main content missing: %q", *got)
	}
}

func TestComputeBodyReplylessTextOnly(t *testing.T) {
	text := "first\n\nSent from my iPhone"
	got := computeBodyReplyless(nil, &text)
	if got == nil || !strings.Contains(*got, "first") || strings.Contains(*got, "Sent from") {
		t.Errorf("unexpected replyless: %v", got)
	}
	if computeBodyReplyless(nil, nil) != nil {
		t.Error("expected nil for empty bodies")
	}
}

func TestComputeBodyParsedLinkFootnotes(t *testing.T) {
	html := `<p>See <a href="https://example.com/doc">the doc</a> and <a href="mailto:a@b.c">mail</a></p>`
	got := computeBodyParsed(html)
	if !strings.Contains(got, "the doc[1]") {
		t.Errorf("missing footnote ref: %q", got)
	}
	if !strings.Contains(got, "Links:\n[1] https://example.com/doc") {
		t.Errorf("missing links section: %q", got)
	}
	if strings.Contains(got, "mailto:") {
		t.Errorf("mailto link should not be footnoted: %q", got)
	}
}

func TestJoinEmails(t *testing.T) {
	name := "Jane Doe"
	got := joinEmails([]DraftContactInfo{
		{Email: "jane@x.com", Name: &name},
		{Email: "bob@y.com"},
		{Email: ""}, // skipped
	})
	if got != `"Jane Doe" <jane@x.com>, bob@y.com` {
		t.Errorf("unexpected join: %q", got)
	}
	if joinEmails(nil) != "" {
		t.Error("expected empty string")
	}
}

func TestDraftAttachmentS3Key(t *testing.T) {
	d := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	a := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	got := draftAttachmentS3Key(d, a)
	want := "draft/11111111-1111-1111-1111-111111111111/22222222-2222-2222-2222-222222222222"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestTempAttachmentKey(t *testing.T) {
	l := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	a := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	fn := "report.pdf"
	got := tempAttachmentKey(l, a, &fn)
	want := "temp/11111111-1111-1111-1111-111111111111/22222222-2222-2222-2222-222222222222-report.pdf"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	got = tempAttachmentKey(l, a, nil)
	if !strings.HasSuffix(got, "-") {
		t.Errorf("nil filename should yield trailing dash: %q", got)
	}
}

func TestMimeTypeForFilename(t *testing.T) {
	cases := map[string]string{
		"a.pdf":        "application/pdf",
		"PHOTO.JPG":    "image/jpeg",
		"noext":        "application/octet-stream",
		".":            "application/octet-stream",
		"invite.ics":   "text/calendar",
		"trailing.":    "application/octet-stream",
		"archive.zip":  "application/zip",
		"unknown.xyz":  "application/octet-stream",
		"sheet.xlsx":   "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"message.eml":  "message/rfc822",
		"data.json":    "application/json",
		"doc.docx":     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"notes.txt":    "text/plain",
		"index.html":   "text/html",
		"table.csv":    "text/csv",
		"img.webp":     "image/webp",
		"animated.GIF": "image/gif",
		"old.doc":      "application/msword",
		"old.xls":      "application/vnd.ms-excel",
		"picture.png":  "image/png",
		"photo.jpeg":   "image/jpeg",
	}
	for name, want := range cases {
		if got := mimeTypeForFilename(name); got != want {
			t.Errorf("mimeTypeForFilename(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestDecodeBodyHTML(t *testing.T) {
	// nil/empty → nil
	if got, err := decodeBodyHTML(nil); err != nil || got != nil {
		t.Errorf("nil input: got %v err %v", got, err)
	}
	empty := ""
	if got, err := decodeBodyHTML(&empty); err != nil || got != nil {
		t.Errorf("empty input: got %v err %v", got, err)
	}
	// base64url (URL_SAFE_NO_PAD) input decodes + sanitizes
	raw := `<p>hi</p><script>alert(1)</script>`
	enc := base64.RawURLEncoding.EncodeToString([]byte(raw))
	got, err := decodeBodyHTML(&enc)
	if err != nil || got == nil {
		t.Fatalf("b64 decode failed: %v", err)
	}
	if strings.Contains(*got, "script") || !strings.Contains(*got, "hi") {
		t.Errorf("sanitized output wrong: %q", *got)
	}
	// non-base64 input is treated as literal HTML and still sanitized
	lit := `<b>bold</b><script>x</script>`
	got, err = decodeBodyHTML(&lit)
	if err != nil || got == nil {
		t.Fatalf("literal decode failed: %v", err)
	}
	if strings.Contains(*got, "<script") {
		t.Errorf("literal input not sanitized: %q", *got)
	}
}
