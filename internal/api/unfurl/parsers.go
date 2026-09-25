package unfurl

import (
	"net/url"
	"strings"
	"unicode"
)

// Custom URL parsers for services that embed document titles in their URLs
// rather than in HTML metadata. Port of crates/unfurl/src/domain/url_parsers.rs.

func parseNotionTitle(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || !strings.Contains(u.Hostname(), "notion.so") {
		return "", false
	}
	segments := splitPath(u.Path)
	// Find the last segment ending in a 32-hex-char UUID.
	var titleSegment string
	for i := len(segments) - 1; i >= 0; i-- {
		s := segments[i]
		if len(s) > 32 && allHex(s[len(s)-32:]) {
			titleSegment = s
			break
		}
	}
	if titleSegment == "" || len(titleSegment) <= 33 {
		return "", false
	}
	titlePart := titleSegment[:len(titleSegment)-33]
	if t := titleCase(strings.ReplaceAll(titlePart, "-", " ")); t != "" {
		return t, true
	}
	return "", false
}

func parseFigmaTitle(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || !strings.Contains(u.Hostname(), "figma.com") {
		return "", false
	}
	segments := splitPath(u.Path)
	if len(segments) >= 3 && segments[0] == "design" {
		if t := titleCase(strings.ReplaceAll(segments[2], "-", " ")); t != "" {
			return t, true
		}
	}
	return "", false
}

func parseLinearTitle(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || !strings.Contains(u.Hostname(), "linear.app") {
		return "", false
	}
	segments := splitPath(u.Path)
	if len(segments) >= 4 && segments[1] == "issue" {
		if t := titleCase(strings.ReplaceAll(segments[3], "-", " ")); t != "" {
			return t, true
		}
	}
	return "", false
}

// ParseCustomTitle tries the custom parsers; on a recognized host it falls
// back to the service name.
func ParseCustomTitle(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	host := u.Hostname()
	switch {
	case strings.Contains(host, "notion.so"):
		if t, ok := parseNotionTitle(rawURL); ok {
			return t, true
		}
		return "Notion", true
	case strings.Contains(host, "figma.com"):
		if t, ok := parseFigmaTitle(rawURL); ok {
			return t, true
		}
		return "Figma", true
	case strings.Contains(host, "linear.app"):
		if t, ok := parseLinearTitle(rawURL); ok {
			return t, true
		}
		return "Linear", true
	}
	return "", false
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func allHex(s string) bool {
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// titleCase replaces each '-'/whitespace separated word's first char with its
// uppercase form — mirrors capitalize_first + split_whitespace + join(" ").
func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		r := []rune(w)
		if len(r) > 0 {
			r[0] = unicode.ToUpper(r[0])
		}
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}
