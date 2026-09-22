package filetype

import "testing"

func TestMIMEType(t *testing.T) {
	cases := map[string]string{
		"pdf":         "application/pdf",
		".PDF":        "application/pdf",
		"docx":        "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"md":          "text/markdown",
		"png":         "image/png",
		"spreadsheet": "application/x-macro-spreadsheet",
		"canvas":      "application/x-macro-canvas",
		"zip":         "application/zip",
	}
	for ext, want := range cases {
		if got := MIMEType(ext); got != want {
			t.Errorf("MIMEType(%q) = %q, want %q", ext, got, want)
		}
	}
	if got := MIMEType("definitelynotreal"); got != "" {
		t.Errorf("expected empty for unknown ext, got %q", got)
	}
}
