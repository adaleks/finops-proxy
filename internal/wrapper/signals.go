package wrapper

import (
	"bytes"
	"io"
	"regexp"
	"sync"
)

// matchWindow is the size of the sliding byte window scanned for a loop marker.
// It comfortably exceeds any realistic single error line, so a marker split
// across successive writes is still caught.
const matchWindow = 4096

var (
	// tooManyRequestsRe matches rate-limit prose a child may print instead of the
	// raw JSON body.
	tooManyRequestsRe = regexp.MustCompile(`(?i)too many requests`)
	// http429Re matches "HTTP 429"-style status prose. A bare "429" is
	// deliberately rejected: token counters print it constantly.
	http429Re = regexp.MustCompile(`(?i)http[^\n]{0,20}429`)
)

// matchLoop reports whether the buffered output window carries a loop signal.
func matchLoop(window []byte, marker string) bool {
	if bytes.Contains(window, []byte(marker)) {
		return true
	}
	s := string(window)
	if tooManyRequestsRe.MatchString(s) {
		return true
	}
	return http429Re.MatchString(s)
}

// lineTee is an io.Writer that passes bytes through to dst unchanged while
// scanning a sliding window for a loop marker. On the first match it calls
// fire exactly once (fire must itself be idempotent).
type lineTee struct {
	mu     sync.Mutex
	dst    io.Writer
	marker string
	tail   []byte
	fire   func()
}

func newLineTee(dst io.Writer, marker string, fire func()) *lineTee {
	return &lineTee{dst: dst, marker: marker, fire: fire}
}

func (t *lineTee) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Pass through unchanged, then slide the match window.
	if _, err := t.dst.Write(p); err != nil {
		return len(p), err
	}
	t.tail = append(t.tail, p...)
	if len(t.tail) > matchWindow {
		t.tail = t.tail[len(t.tail)-matchWindow:]
	}
	if matchLoop(t.tail, t.marker) {
		t.fire()
	}
	return len(p), nil
}
