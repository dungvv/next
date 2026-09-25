package email

import (
	"html"
	"regexp"
	"strings"

	xhtml "golang.org/x/net/html"
)

// This file ports the usable core of crates/email_utils:
//   - compute_body_replyless: strip quoted reply/forward content
//   - compute_body_parsed: HTML → readable plaintext with link footnotes
//
// The Rust implementation walks a full HTML DOM and understands many
// provider-specific quote wrappers; this version handles the common cases
// (Gmail .gmail_quote, <blockquote>, Outlook/Yahoo separators, and textual
// splitters like "On ... wrote:").

// replyStarters are line prefixes that mark the beginning of quoted content.
var replyStarters = []*regexp.Regexp{
	regexp.MustCompile(`^On .{0,500}wrote:.*$`),
	regexp.MustCompile(`^On .{0,500}wrote\..*$`),
	regexp.MustCompile(`^-+\s*Original Message\s*-+.*$`),
	regexp.MustCompile(`^-+\s*Forwarded message\s*-+.*$`),
	regexp.MustCompile(`^_{3,}.*$`),
	regexp.MustCompile(`^From:\s+.*$`),
	regexp.MustCompile(`^Sent from .{0,80}$`),
	regexp.MustCompile(`^Get Outlook for .{0,80}$`),
}

// computeBodyReplyless returns the message body without quoted reply/forward
// content, preferring the HTML body (parsed to text) when present.
func computeBodyReplyless(htmlBody, textBody *string) *string {
	if htmlBody != nil && *htmlBody != "" {
		txt := htmlToText(*htmlBody, true, false)
		if r := stripReplies(txt); r != "" {
			return &r
		}
		return &txt
	}
	if textBody != nil && *textBody != "" {
		r := stripReplies(*textBody)
		return &r
	}
	return nil
}

// computeBodyParsed converts an HTML body into readable plaintext, appending
// link footnotes (`[1]`, `[2]`, ... + a trailing "Links:" section) like the
// Rust body_parsed. Used where the frontend wants plain text from HTML.
func computeBodyParsed(htmlBody string) string {
	return htmlToText(htmlBody, false, true)
}

// stripReplies truncates plaintext at the first line matching a known quote
// marker.
func stripReplies(body string) string {
	lines := strings.Split(body, "\n")
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		stop := false
		for _, re := range replyStarters {
			if re.MatchString(trimmed) {
				stop = true
				break
			}
		}
		if stop {
			break
		}
		out = append(out, line)
	}
	return strings.TrimRight(strings.Join(out, "\n"), " \t\r\n")
}

// quoteClasses are CSS classes that wrap quoted content in email HTML.
var quoteClasses = map[string]bool{
	"gmail_quote":          true,
	"gmail_extra":          true,
	"yahoo_quoted":         true,
	"protonmail_quote":     true,
	"OutlookMessageHeader": true,
}

// htmlToText renders an HTML body to plaintext. When stripQuotes is set,
// known quote containers are skipped entirely; otherwise a splitter is still
// applied afterwards by callers that want it.
func htmlToText(body string, stripQuotes bool, linkFootnotes bool) string {
	doc, err := xhtml.Parse(strings.NewReader(body))
	if err != nil {
		return stripTagsFallback(body)
	}
	var b strings.Builder
	var links []string
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			if stripQuotes && isQuoteNode(n) {
				return
			}
			switch n.Data {
			case "script", "style", "head":
				return
			case "br", "p", "div", "tr", "li", "h1", "h2", "h3", "h4", "h5", "h6", "section", "article":
				b.WriteString("\n")
			}
			if n.Data == "a" {
				href := attr(n, "href")
				if linkFootnotes && href != "" && !strings.HasPrefix(href, "mailto:") {
					links = append(links, href)
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						walk(c)
					}
					b.WriteString("[")
					b.WriteString(itoa(len(links)))
					b.WriteString("]")
					return
				}
			}
			if n.Data == "li" {
				b.WriteString("- ")
			}
		}
		if n.Type == xhtml.TextNode {
			b.WriteString(n.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	text := collapseWhitespace(html.UnescapeString(b.String()))
	if linkFootnotes && len(links) > 0 {
		var sb strings.Builder
		sb.WriteString(text)
		sb.WriteString("\n\nLinks:")
		for i, l := range links {
			sb.WriteString("\n[")
			sb.WriteString(itoa(i + 1))
			sb.WriteString("] ")
			sb.WriteString(l)
		}
		text = sb.String()
	}
	return text
}

func isQuoteNode(n *xhtml.Node) bool {
	if n.Data == "blockquote" {
		return true
	}
	cls := attr(n, "class")
	for _, c := range strings.Fields(cls) {
		if quoteClasses[c] {
			return true
		}
	}
	return false
}

func attr(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

var wsRun = regexp.MustCompile(`[ \t]+`)

func collapseWhitespace(s string) string {
	// collapse runs of spaces/tabs, collapse >2 blank lines
	s = wsRun.ReplaceAllString(s, " ")
	var out []string
	blank := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, " ")
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func stripTagsFallback(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return collapseWhitespace(html.UnescapeString(b.String()))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
