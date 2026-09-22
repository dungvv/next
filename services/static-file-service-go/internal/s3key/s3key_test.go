package s3key

import "testing"

func TestFromS3Key(t *testing.T) {
	orig, err := FromS3Key("file/abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if orig.FileID != "abc-123" || orig.TransformKey != "" || orig.String() != "file/abc-123" {
		t.Fatalf("unexpected %+v", orig)
	}
	variant, err := FromS3Key("file/abc-123/format=webp,width=300")
	if err != nil {
		t.Fatal(err)
	}
	if variant.FileID != "abc-123" || variant.TransformKey != "format=webp,width=300" {
		t.Fatalf("unexpected %+v", variant)
	}
	if variant.VariantPrefix() != "file/abc-123/" {
		t.Fatalf("bad prefix %q", variant.VariantPrefix())
	}
	for _, bad := range []string{"abc-123", "file/", "file//x", "file/id/"} {
		if _, err := FromS3Key(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}
