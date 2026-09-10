package circuitbreaker

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeClock is a concurrency-safe, manually advanceable clock used to exercise
// TTL behaviour without any real sleeping. It returns wall-clock times with no
// monotonic component, which is fine for the duration arithmetic the detector
// performs.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t0 time.Time) *fakeClock {
	return &fakeClock{t: t0}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// staticClock returns a fixed time.Time for tests that never advance the clock.
func staticClock(t0 time.Time) func() time.Time {
	return func() time.Time { return t0 }
}

const testTTL = time.Hour

// testAgent is the default (empty) agent id used by tests that don't exercise
// per-agent scoping.
const testAgent AgentID = ""

func TestHashDetectorFirstSightingPasses(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	d.now = staticClock(time.Unix(1_000_000, 0))

	if res := d.Check(testAgent, Fingerprint("once")); res.Block {
		t.Fatalf("first sighting must pass, got %+v", res)
	}
}

func TestHashDetectorTTLExpiry(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	clock := newFakeClock(time.Unix(1_000_000, 0))
	d.now = clock.now

	fp := Fingerprint("fp")

	// First sighting opens a window and passes.
	if res := d.Check(testAgent, fp); res.Block {
		t.Fatalf("first sighting must pass, got %+v", res)
	}
	// Second sighting within the TTL blocks.
	if res := d.Check(testAgent, fp); !res.Block {
		t.Fatalf("second sighting within TTL must block, got %+v", res)
	}

	// Advance past the window end: the next sighting starts a fresh window.
	clock.advance(testTTL + time.Second)
	if res := d.Check(testAgent, fp); res.Block {
		t.Fatalf("sighting after TTL expiry must start a fresh window and pass, got %+v", res)
	}
	// And a follow-up within the new window blocks again.
	if res := d.Check(testAgent, fp); !res.Block {
		t.Fatalf("sighting within the fresh window must block, got %+v", res)
	}
}

func TestHashDetectorThresholdTwo(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	d.now = staticClock(time.Unix(1_000_000, 0))

	fp := Fingerprint("x")
	want := []bool{false, true, true}
	for i, wantBlock := range want {
		res := d.Check(testAgent, fp)
		if res.Block != wantBlock {
			t.Errorf("sighting %d: block=%v, want %v (res=%+v)", i+1, res.Block, wantBlock, res)
		}
		if res.Block {
			if res.RetryAfter <= 0 {
				t.Errorf("sighting %d: RetryAfter must be > 0 when blocked, got %d", i+1, res.RetryAfter)
			}
			if res.Reason != "identical request fingerprint observed within TTL window (possible infinite loop)" {
				t.Errorf("sighting %d: unexpected reason %q", i+1, res.Reason)
			}
		} else if res.Reason != "" {
			t.Errorf("sighting %d: non-blocking result must carry an empty reason, got %q", i+1, res.Reason)
		}
	}
}

// TestHashDetectorFirstBlock asserts that exactly one sighting per TTL window
// carries FirstBlock==true — the transition into the blocked state — and that a
// fresh window (after TTL expiry) re-fires it for the next incident.
func TestHashDetectorFirstBlock(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	clock := newFakeClock(time.Unix(1_000_000, 0))
	d.now = clock.now

	fp := Fingerprint("firstblock")

	// Within one window: first sighting passes (no block), second blocks for the
	// first time (FirstBlock=true), third is a repeat block (FirstBlock=false).
	want := []bool{false, true, false}
	for i, wantFirst := range want {
		res := d.Check(testAgent, fp)
		if res.FirstBlock != wantFirst {
			t.Errorf("sighting %d: FirstBlock = %v, want %v (res=%+v)", i+1, res.FirstBlock, wantFirst, res)
		}
		// The middle sighting is the one that transitions into blocked.
		if i == 1 && !res.Block {
			t.Errorf("sighting %d: must be a block, got %+v", i+1, res)
		}
	}

	// Advance past the window end: the next sighting opens a fresh window and the
	// transition into blocked re-fires FirstBlock=true.
	clock.advance(testTTL + time.Second)
	if res := d.Check(testAgent, fp); res.Block {
		t.Fatalf("sighting after TTL expiry must start a fresh window and pass, got %+v", res)
	}
	res := d.Check(testAgent, fp)
	if !res.Block {
		t.Fatalf("fresh-window second sighting must block, got %+v", res)
	}
	if !res.FirstBlock {
		t.Errorf("fresh-window first block must carry FirstBlock=true, got %+v", res)
	}
	// A third sighting in the fresh window is a repeat block, not first.
	if res := d.Check(testAgent, fp); res.Block && res.FirstBlock {
		t.Errorf("repeat block within the fresh window must not carry FirstBlock=true, got %+v", res)
	}
}

func TestHashDetectorLimitThree(t *testing.T) {
	d := NewHashDetector(testTTL, 3)
	d.now = staticClock(time.Unix(1_000_000, 0))

	fp := Fingerprint("y")
	want := []bool{false, false, true}
	for i, wantBlock := range want {
		if res := d.Check(testAgent, fp); res.Block != wantBlock {
			t.Errorf("sighting %d: block=%v, want %v (res=%+v)", i+1, res.Block, wantBlock, res)
		}
	}
}

func TestHashDetectorDefaultLimit(t *testing.T) {
	// Non-positive limit must default to 2 (first passes, second blocks).
	for _, bad := range []int{0, -5} {
		d := NewHashDetector(testTTL, bad)
		d.now = staticClock(time.Unix(1_000_000, 0))
		fp := Fingerprint("z")
		if res := d.Check(testAgent, fp); res.Block {
			t.Fatalf("limit=%d: first sighting must pass, got %+v", bad, res)
		}
		if res := d.Check(testAgent, fp); !res.Block {
			t.Fatalf("limit=%d: second sighting must block, got %+v", bad, res)
		}
	}
}

func TestHashDetectorRetryAfterIsCeilingOfRemaining(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	clock := newFakeClock(time.Unix(1_000_000, 0))
	d.now = clock.now

	fp := Fingerprint("r")
	d.Check(testAgent, fp) // window ends at t0 + 1h

	// 90 minutes later the window has long expired; instead test a sub-second
	// remainder: create a fresh window and advance to just before expiry.
	clock.advance(2 * testTTL) // force a fresh window on next sighting
	d.Check(testAgent, fp)     // fresh window ends at (now + 1h)
	clock.advance(time.Minute) // 59 minutes remaining
	res := d.Check(testAgent, fp)
	if !res.Block {
		t.Fatalf("expected block, got %+v", res)
	}
	if want := 59 * 60; res.RetryAfter != want {
		t.Errorf("RetryAfter = %d, want %d", res.RetryAfter, want)
	}
}

func TestHashDetectorDistinctFingerprintsDoNotCrossContaminate(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	d.now = staticClock(time.Unix(1_000_000, 0))

	a, b := Fingerprint("a"), Fingerprint("b")

	if res := d.Check(testAgent, a); res.Block {
		t.Fatalf("first sighting of a must pass, got %+v", res)
	}
	if res := d.Check(testAgent, b); res.Block {
		t.Fatalf("first sighting of b must pass, got %+v", res)
	}
	// Both are on their second sighting; both block independently.
	if res := d.Check(testAgent, a); !res.Block {
		t.Fatalf("second sighting of a must block, got %+v", res)
	}
	if res := d.Check(testAgent, b); !res.Block {
		t.Fatalf("second sighting of b must block, got %+v", res)
	}
}

func TestHashDetectorConcurrency(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	d.now = staticClock(time.Unix(1_000_000, 0))

	const n = 200
	results := make(chan CheckResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- d.Check(testAgent, Fingerprint("same"))
		}()
	}
	wg.Wait()
	close(results)

	passed, blocked := 0, 0
	for r := range results {
		if r.Block {
			blocked++
		} else {
			passed++
		}
	}
	if passed != 1 || blocked != n-1 {
		t.Fatalf("with limit=2, exactly 1 goroutine must pass: got passed=%d blocked=%d", passed, blocked)
	}
}

func TestHashDetectorEvictionBoundsMap(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	d.now = staticClock(time.Unix(1_000_000, 0))
	d.max = 10 // shrink the cap so the test stays small

	// Insert far more distinct fingerprints than the cap.
	for i := 0; i < 500; i++ {
		d.Check(testAgent, Fingerprint(strconv.Itoa(i)))
	}

	d.mu.Lock()
	size := len(d.seen)
	d.mu.Unlock()

	if size > d.max {
		t.Fatalf("map grew beyond cap: size=%d max=%d", size, d.max)
	}
	// Also verify the tracker is still live: a brand-new fingerprint passes.
	if res := d.Check(testAgent, Fingerprint("brand-new")); res.Block {
		t.Fatalf("new fingerprint must pass after eviction, got %+v", res)
	}
}

func TestHashDetectorReset(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	d.now = staticClock(time.Unix(1_000_000, 0))

	agent := AgentID("agent-a")
	fp := Fingerprint("fp")

	d.Check(agent, fp) // count = 1
	if res := d.Check(agent, fp); !res.Block {
		t.Fatalf("second sighting must block, got %+v", res)
	}

	if n := d.Reset(agent); n != 1 {
		t.Fatalf("Reset removed %d entries, want 1", n)
	}
	// Idempotent: a second reset finds nothing.
	if n := d.Reset(agent); n != 0 {
		t.Fatalf("second Reset removed %d entries, want 0 (idempotent)", n)
	}

	// After reset the fingerprint opens a fresh window and passes again.
	if res := d.Check(agent, fp); res.Block {
		t.Fatalf("sighting after reset must pass, got %+v", res)
	}
}

func TestHashDetectorAgentIsolation(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	d.now = staticClock(time.Unix(1_000_000, 0))

	fp := Fingerprint("same")
	a, b := AgentID("a"), AgentID("b")

	// Both agents sight fp once; neither blocks.
	if res := d.Check(a, fp); res.Block {
		t.Fatalf("agent a first sighting must pass, got %+v", res)
	}
	if res := d.Check(b, fp); res.Block {
		t.Fatalf("agent b first sighting must pass, got %+v", res)
	}
	// Agent a's second sighting blocks.
	if res := d.Check(a, fp); !res.Block {
		t.Fatalf("agent a second sighting must block, got %+v", res)
	}
	// Resetting a clears only a's history.
	d.Reset(a)
	// a passes again on a fresh window...
	if res := d.Check(a, fp); res.Block {
		t.Fatalf("agent a after reset must pass, got %+v", res)
	}
	// ...while b, never reset, still blocks on its own second sighting.
	if res := d.Check(b, fp); !res.Block {
		t.Fatalf("agent b second sighting must still block after a's reset, got %+v", res)
	}
}

// TestSetLimitRaisesThreshold proves a raised threshold applies to the NEXT
// Check evaluation: with limit 2→3, counts 1 and 2 pass and only count 3 blocks.
func TestSetLimitRaisesThreshold(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	d.now = staticClock(time.Unix(1_000_000, 0))
	fp := Fingerprint("raise")

	if res := d.Check(testAgent, fp); res.Block {
		t.Fatalf("sighting 1 (count=1) must pass, got %+v", res)
	}
	d.SetLimit(3)
	if res := d.Check(testAgent, fp); res.Block {
		t.Fatalf("sighting 2 (count=2 < new limit 3) must pass, got %+v", res)
	}
	if res := d.Check(testAgent, fp); !res.Block {
		t.Fatalf("sighting 3 (count=3) must block, got %+v", res)
	}
}

// TestSetLimitLowersThreshold proves a lowered threshold blocks immediately:
// with limit 3→2 and a count already at 2, the next sighting blocks.
func TestSetLimitLowersThreshold(t *testing.T) {
	d := NewHashDetector(testTTL, 3)
	d.now = staticClock(time.Unix(1_000_000, 0))
	fp := Fingerprint("lower")

	if res := d.Check(testAgent, fp); res.Block {
		t.Fatalf("sighting 1 must pass, got %+v", res)
	}
	if res := d.Check(testAgent, fp); res.Block {
		t.Fatalf("sighting 2 (count=2 < 3) must pass, got %+v", res)
	}
	d.SetLimit(2)
	if res := d.Check(testAgent, fp); !res.Block {
		t.Fatalf("sighting 3 (count=3 >= new limit 2) must block immediately, got %+v", res)
	}
}

// TestSetLimitDoesNotResetCounts proves SetLimit preserves per-fingerprint
// counts: the window's count survives and keeps accumulating.
func TestSetLimitDoesNotResetCounts(t *testing.T) {
	d := NewHashDetector(testTTL, 3)
	d.now = staticClock(time.Unix(1_000_000, 0))
	fp := Fingerprint("counts")

	d.Check(testAgent, fp) // count = 1
	d.SetLimit(3)          // no-op change of the same value
	d.Check(testAgent, fp) // count = 2
	d.Check(testAgent, fp) // count = 3 → block
	if res := d.Check(testAgent, fp); !res.Block {
		t.Fatalf("counts must survive SetLimit; sighting 4 must block, got %+v", res)
	}
}

// TestSetLimitKeepsTTLWindowIntact proves SetLimit does not reset the fixed TTL
// window: after lowering the limit mid-window, the next sighting blocks while
// still inside the original window (not a freshly opened one).
func TestSetLimitKeepsTTLWindowIntact(t *testing.T) {
	d := NewHashDetector(testTTL, 3)
	clock := newFakeClock(time.Unix(1_000_000, 0))
	d.now = clock.now
	fp := Fingerprint("window")

	d.Check(testAgent, fp)          // count=1, window ends at t0+TTL
	d.SetLimit(2)                   // lower the threshold
	clock.advance(30 * time.Minute) // still inside the original window
	if res := d.Check(testAgent, fp); !res.Block {
		t.Fatalf("sighting within the original window must block after SetLimit, got %+v", res)
	}
}

// TestSetLimitFirstBlockTransition proves raising the limit does NOT re-fire
// FirstBlock on an already-blocked window: the transition flag is per-window.
func TestSetLimitFirstBlockTransition(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	d.now = staticClock(time.Unix(1_000_000, 0))
	fp := Fingerprint("firstblock")

	d.Check(testAgent, fp) // count=1, pass
	res := d.Check(testAgent, fp)
	if !res.Block || !res.FirstBlock {
		t.Fatalf("second sighting must be the first block, got %+v", res)
	}
	d.SetLimit(3)                // raise the threshold on an already-blocked window
	res = d.Check(testAgent, fp) // count=3, still >= old limit 2 → blocks
	if !res.Block {
		t.Fatalf("blocked window must keep blocking after SetLimit, got %+v", res)
	}
	if res.FirstBlock {
		t.Fatalf("SetLimit must not re-fire FirstBlock on an already-blocked window, got %+v", res)
	}
}

// TestSetLimitToOne pins D11: the first sighting always passes, so limit=1
// behaves identically to limit=2.
func TestSetLimitToOne(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	d.now = staticClock(time.Unix(1_000_000, 0))
	fp := Fingerprint("one")

	d.SetLimit(1)
	if res := d.Check(testAgent, fp); res.Block {
		t.Fatalf("first sighting under limit=1 must pass, got %+v", res)
	}
	if res := d.Check(testAgent, fp); !res.Block {
		t.Fatalf("second sighting under limit=1 must block, got %+v", res)
	}
}

// TestSetLimitNonPositive proves SetLimit clamps n<1 to 1 (never a lower
// threshold or a broken detector), and that a clamped value still allows the
// mandatory first sighting.
func TestSetLimitNonPositive(t *testing.T) {
	for _, n := range []int{0, -1, -5} {
		d := NewHashDetector(testTTL, 2)
		d.now = staticClock(time.Unix(1_000_000, 0))
		fp := Fingerprint("nonpos")
		d.SetLimit(n)
		if res := d.Check(testAgent, fp); res.Block {
			t.Fatalf("SetLimit(%d): first sighting must pass, got %+v", n, res)
		}
		if res := d.Check(testAgent, fp); !res.Block {
			t.Fatalf("SetLimit(%d): second sighting must block (clamped to 1), got %+v", n, res)
		}
	}
}

// TestSetLimitConcurrentWithCheck churns SetLimit against Check from multiple
// goroutines. The race detector must stay clean and the detector must remain
// usable (a fresh fingerprint still opens a fresh window and passes).
func TestSetLimitConcurrentWithCheck(t *testing.T) {
	d := NewHashDetector(testTTL, 2)
	d.now = staticClock(time.Unix(1_000_000, 0))

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				d.Check(testAgent, Fingerprint("race"))
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				d.SetLimit(1 + j%5)
			}
		}()
	}
	wg.Wait()

	if res := d.Check(testAgent, Fingerprint("after")); res.Block {
		t.Fatalf("fresh fingerprint after churn must pass, got %+v", res)
	}
}
