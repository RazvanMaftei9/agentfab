package nodehost

import "testing"

func TestProgressSnippet(t *testing.T) {
	short := "planning the layout"
	if got := progressSnippet(short); got != short {
		t.Fatalf("progressSnippet(%q) = %q, want original string", short, got)
	}

	long := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnopqrstuvwxyz"
	got := progressSnippet(long)
	if len(got) != 80 {
		t.Fatalf("snippet length = %d, want 80", len(got))
	}
	if got != long[len(long)-80:] {
		t.Fatalf("snippet = %q, want trailing 80-byte window", got)
	}
}
