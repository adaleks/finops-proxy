package circuitbreaker

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestNormalizePayloadStripsDynamicValues(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"number", `{"n":123}`, `{"n":#}`},
		{"negative-float", `{"n":-1.5}`, `{"n":#}`},
		{"uuid", `{"id":"123e4567-e89b-12d3-a456-426614174000"}`, `{"id":U}`},
		{"timestamp", `{"ts":"2026-09-09T12:34:56Z"}`, `{"ts":T}`},
		{"epoch-seconds", `{"ts":"1725897600"}`, `{"ts":T}`},
		{"epoch-millis", `{"ts":"1725897600123"}`, `{"ts":T}`},
		{"hex", `{"token":"a1b2c3d4e5f6a7b8"}`, `{"token":H}`},
		{"bool-true", `{"ok":true}`, `{"ok":B}`},
		{"bool-false", `{"flag":false}`, `{"flag":B}`},
		{"null", `{"x":null}`, `{"x":N}`},
		{"static-string", `{"q":"hello world"}`, `{"q":"hello world"}`},
		{"short-digit-string", `{"id":"123"}`, `{"id":"123"}`},
		{"all-letter-word", `{"word":"deadbeef"}`, `{"word":"deadbeef"}`},
		{"nested-list", `{"m":{"list":[1,2,3]}}`, `{"m":{"list":[#,#,#]}}`},
		{"whitespace", "{\n  \"a\": 1\n}", `{"a":#}`},
		{"empty-object", `{"obj":{}}`, `{"obj":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := make([]byte, 0, 256)
			normalizePayload(&buf, []byte(tc.in))
			if string(buf) != tc.want {
				t.Fatalf("normalize(%q) = %q, want %q", tc.in, buf, tc.want)
			}
		})
	}
}

func TestClassifyString(t *testing.T) {
	cases := []struct {
		in  string
		tag byte
		dyn bool
	}{
		{"123e4567-e89b-12d3-a456-426614174000", 'U', true},
		{"2026-09-09T12:34:56Z", 'T', true},
		{"1725897600", 'T', true},
		{"1725897600123", 'T', true},
		{"a1b2c3d4e5f6a7b8", 'H', true},
		{"hello", 0, false},
		{"deadbeef", 0, false}, // all letters, no digit → static
		{"", 0, false},
	}
	for _, tc := range cases {
		tag, ok := classifyString([]byte(tc.in))
		if ok != tc.dyn || (ok && tag != tc.tag) {
			t.Fatalf("classifyString(%q) = (%q, %v), want (%q, %v)", tc.in, tag, ok, tc.tag, tc.dyn)
		}
	}
}

func TestJaccardIdenticalIsOne(t *testing.T) {
	a := minHash(norm(`{"q":"what is the meaning of life"}`))
	b := minHash(norm(`{"q":"what is the meaning of life"}`))
	if got := jaccard(a, b); got != 1.0 {
		t.Fatalf("jaccard of identical structures = %v, want 1.0", got)
	}
}

func TestJaccardDistinctIsLow(t *testing.T) {
	a := minHash(norm(`{"q":"the quick brown fox jumps over the lazy dog"}`))
	b := minHash(norm(`{"q":"pack my box with five dozen liquor jugs"}`))
	if got := jaccard(a, b); got >= 0.85 {
		t.Fatalf("jaccard of distinct structures = %v, want < 0.85", got)
	}
}

func TestStructuralHasherBlocksNearDuplicate(t *testing.T) {
	s := NewStructuralHasher(StructuralConfig{}) // limit 3, window 8, threshold 0.85
	s.now = staticClock(time.Unix(1_000_000, 0))
	agent := AgentID("a")
	fp := Fingerprint("fp")

	body := func(attempt int) []byte {
		return []byte(`{"model":"gpt-4o","attempt":` + strconv.Itoa(attempt) +
			`,"request_id":"123e4567-e89b-12d3-a456-426614174000","messages":[{"role":"user","content":"please retry this task"}]}`)
	}

	// limit=3: sightings 1-3 pass, sighting 4 blocks (3 prior matches).
	for i := 0; i < 3; i++ {
		if res := s.CheckPayload(agent, fp, body(i)); res.Block {
			t.Fatalf("sighting %d must pass, got %+v", i+1, res)
		}
	}
	res := s.CheckPayload(agent, fp, body(3))
	if !res.Block {
		t.Fatalf("4th structurally-identical sighting must block, got %+v", res)
	}
	if res.Reason != ReasonStructural {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonStructural)
	}
	if !res.FirstBlock {
		t.Fatalf("4th sighting must be the first block, got %+v", res)
	}
	// A 5th sighting is a repeat block, not first.
	if res := s.CheckPayload(agent, fp, body(4)); res.Block && res.FirstBlock {
		t.Fatalf("repeat block must not carry FirstBlock=true, got %+v", res)
	}
}

func TestStructuralHasherDistinctStructuresPass(t *testing.T) {
	s := NewStructuralHasher(StructuralConfig{})
	s.now = staticClock(time.Unix(1_000_000, 0))
	agent := AgentID("a")

	sentences := []string{
		"the quick brown fox jumps over the lazy dog",
		"pack my box with five dozen liquor jugs",
		"how vexingly quick daft zebras jump",
		"sphinx of black quartz judge my vow",
		"the five boxing wizards jump quickly",
		"two driven jocks help fax my big quiz",
		"my girl wove six dozen plaid jackets before she quit",
		"a wizard job is to vex chumps quickly in fog",
	}
	for i, snt := range sentences {
		body := []byte(`{"q":"` + snt + `"}`)
		if res := s.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
			t.Fatalf("distinct structure %d must pass, got %+v", i, res)
		}
	}
}

func TestStructuralHasherTTLExpiry(t *testing.T) {
	clock := newFakeClock(time.Unix(1_000_000, 0))
	s := NewStructuralHasher(StructuralConfig{TTL: time.Minute, Limit: 2})
	s.now = clock.now
	agent := AgentID("a")
	body := []byte(`{"q":"hello","n":1}`)

	if res := s.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
		t.Fatalf("sighting 1 must pass, got %+v", res)
	}
	if res := s.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
		t.Fatalf("sighting 2 must pass (1 match < limit 2), got %+v", res)
	}
	if res := s.CheckPayload(agent, Fingerprint("fp"), body); !res.Block {
		t.Fatalf("sighting 3 must block (2 matches), got %+v", res)
	}

	clock.advance(2 * time.Minute)
	if res := s.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
		t.Fatalf("sighting after TTL must start a fresh window and pass, got %+v", res)
	}
}

func TestStructuralHasherAgentIsolation(t *testing.T) {
	s := NewStructuralHasher(StructuralConfig{Limit: 2})
	s.now = staticClock(time.Unix(1_000_000, 0))
	body := []byte(`{"q":"same structure","n":7}`)

	a, b := AgentID("a"), AgentID("b")
	s.CheckPayload(a, Fingerprint("fp"), body)
	s.CheckPayload(a, Fingerprint("fp"), body) // a: 1 match
	if res := s.CheckPayload(b, Fingerprint("fp"), body); res.Block {
		t.Fatalf("agent b's first structural sighting must not be contaminated by a, got %+v", res)
	}
	if res := s.CheckPayload(a, Fingerprint("fp"), body); !res.Block {
		t.Fatalf("agent a's 3rd sighting must block (2 matches), got %+v", res)
	}
}

func TestStructuralHasherReset(t *testing.T) {
	s := NewStructuralHasher(StructuralConfig{Limit: 2})
	s.now = staticClock(time.Unix(1_000_000, 0))
	agent := AgentID("a")
	body := []byte(`{"q":"hello","n":1}`)

	s.CheckPayload(agent, Fingerprint("fp"), body)
	s.CheckPayload(agent, Fingerprint("fp"), body)
	if res := s.CheckPayload(agent, Fingerprint("fp"), body); !res.Block {
		t.Fatalf("3rd sighting must block, got %+v", res)
	}

	if n := s.Reset(agent); n != 1 {
		t.Fatalf("Reset removed %d entries, want 1", n)
	}
	if res := s.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
		t.Fatalf("sighting after reset must pass, got %+v", res)
	}
}

func TestStructuralHasherEviction(t *testing.T) {
	s := NewStructuralHasher(StructuralConfig{MaxEntries: 5})
	s.now = staticClock(time.Unix(1_000_000, 0))
	for i := 0; i < 100; i++ {
		s.CheckPayload(AgentID("agent"+strconv.Itoa(i)), Fingerprint("fp"), []byte(`{"q":"hello world"}`))
	}
	s.mu.Lock()
	size := len(s.seen)
	s.mu.Unlock()
	if size > 5 {
		t.Fatalf("map grew beyond cap: size=%d max=5", size)
	}
}

func TestStructuralHasherConcurrency(t *testing.T) {
	s := NewStructuralHasher(StructuralConfig{})
	s.now = staticClock(time.Unix(1_000_000, 0))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				body := []byte(`{"q":"content ` + strconv.Itoa(j%10) + `"}`)
				s.CheckPayload(AgentID("a"), Fingerprint("fp"), body)
			}
		}()
	}
	wg.Wait()
	if res := s.CheckPayload(AgentID("fresh"), Fingerprint("fp"), []byte(`{"q":"fresh content"}`)); res.Block {
		t.Fatalf("fresh agent must pass after churn, got %+v", res)
	}
}

// norm normalizes a string into a fresh structural-form buffer (test helper).
func norm(s string) []byte {
	buf := make([]byte, 0, 256)
	normalizePayload(&buf, []byte(s))
	return buf
}
