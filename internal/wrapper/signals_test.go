package wrapper

import (
	"bytes"
	"sync"
	"testing"
)

func TestMatchLoop(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`{"error":{"type":"agent_loop_exception"}}`, true},
		{"HTTP 429 Too Many Requests", true},
		{"error: too many requests", true},
		{"token count: 429", false}, // bare 429 must not match
		{"all good", false},
	}
	for _, c := range cases {
		if got := matchLoop([]byte(c.line), "agent_loop_exception"); got != c.want {
			t.Errorf("matchLoop(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestLineTeePassthroughAndFire(t *testing.T) {
	var dst bytes.Buffer
	count := 0
	var once sync.Once
	fire := func() { once.Do(func() { count++ }) }

	tee := newLineTee(&dst, "agent_loop_exception", fire)

	// Split the marker across two writes to exercise the sliding window.
	tee.Write([]byte("prefix agent_loop_"))
	tee.Write([]byte("exception suffix\n"))

	if got := dst.String(); got != "prefix agent_loop_exception suffix\n" {
		t.Fatalf("passthrough = %q", got)
	}
	if count != 1 {
		t.Fatalf("fire count = %d, want 1", count)
	}

	// fire must be idempotent even if a second write matches.
	tee.Write([]byte("another agent_loop_exception\n"))
	if count != 1 {
		t.Fatalf("fire count = %d, want 1 (idempotent)", count)
	}
}
