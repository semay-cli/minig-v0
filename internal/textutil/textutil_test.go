package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStripThink(t *testing.T) {
	clean, thought := StripThink("<think>plan step 1\nplan step 2</think>\n\nFinal result.")
	if clean != "Final result." {
		t.Errorf("clean = %q, want %q", clean, "Final result.")
	}
	if thought != "plan step 1\nplan step 2" {
		t.Errorf("thought = %q", thought)
	}

	// Unclosed think block (e.g., hit max_tokens mid-reasoning)
	clean, thought = StripThink("Prefix <THINK>cut off reasoning")
	if clean != "Prefix" || thought != "cut off reasoning" {
		t.Errorf("unclosed strip = (%q, %q)", clean, thought)
	}
}

func TestTruncateBytesPreservesUTF8(t *testing.T) {
	// Multi-byte UTF-8 characters (3 bytes each in UTF-8)
	input := strings.Repeat("ሰማይ", 20)
	got := TruncateBytes(input, 32)
	if len(got) > 32 {
		t.Fatalf("len(got) = %d, want <= 32", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated string is invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Errorf("expected truncation marker, got %q", got)
	}
}

func TestSliceLines(t *testing.T) {
	src := "line1\nline2\nline3\nline4\nline5\n"
	sliced, total, truncated := SliceLines(src, 2, 2)
	if total != 5 || !truncated || sliced != "line2\nline3" {
		t.Fatalf("SliceLines(2,2) = (%q, %d, %v)", sliced, total, truncated)
	}

	sliced, total, truncated = SliceLines(src, 4, 5)
	if total != 5 || truncated || sliced != "line4\nline5" {
		t.Fatalf("SliceLines(4,5) = (%q, %d, %v)", sliced, total, truncated)
	}
}

func TestCompactWhitespace(t *testing.T) {
	input := "  func main() {   \n\n\n\n\treturn   \n}\n\n"
	got := CompactWhitespace(input)
	want := "  func main() {\n\n\treturn\n}"
	if got != want {
		t.Errorf("CompactWhitespace = %q, want %q", got, want)
	}
}
