package console

import (
	"testing"
	"unicode/utf8"
)

// TestTruncate: no panic on n<=0/1, and never produces invalid UTF-8 mid-rune.
func TestTruncate(t *testing.T) {
	if truncate("hello", 0) != "" || truncate("hello", -3) != "" {
		t.Error("n<=0 should yield empty")
	}
	_ = truncate("abc", 1) // must not panic
	for _, s := range []string{"héllo wörld", "😀😀😀😀😀", "日本語のテキスト"} {
		for n := 1; n <= len([]rune(s))+2; n++ {
			got := truncate(s, n)
			if !utf8.ValidString(got) {
				t.Errorf("truncate(%q,%d)=%q is not valid UTF-8", s, n, got)
			}
		}
	}
}
