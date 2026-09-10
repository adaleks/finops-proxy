package circuitbreaker

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestExtractToolNames(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"messages":[
			{"role":"user","content":"do it"},
			{"role":"assistant","tool_calls":[{"function":{"name":"query_db"}},{"function":{"name":"edit_file"}}]},
			{"role":"assistant","tool_calls":[{"function":{"name":"query_db"}}]}
		],
		"functions":[{"name":"legacy_fn"}],
		"tools":[{"type":"function","function":{"name":"search"}},{"name":"anthropic_tool"}]
	}`)
	var names []string
	extractToolNames(&names, body)
	want := []string{"query_db", "edit_file", "query_db", "legacy_fn", "search", "anthropic_tool"}
	if !slices.Equal(names, want) {
		t.Fatalf("extractToolNames = %v, want %v", names, want)
	}
}

func TestExtractToolNamesNoSurface(t *testing.T) {
	var names []string
	extractToolNames(&names, []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if len(names) != 0 {
		t.Fatalf("expected no tool names, got %v", names)
	}
}

func TestExtractToolNamesMalformed(t *testing.T) {
	var names []string
	extractToolNames(&names, []byte(`{not json`))
	if len(names) != 0 {
		t.Fatalf("malformed body must yield no names, got %v", names)
	}
}

func TestDetectCycle(t *testing.T) {
	if p, ok := detectCycle([]string{"A", "B", "A", "B"}, 8); !ok || p != 2 {
		t.Fatalf("A B A B = period 2, got (p=%d ok=%v)", p, ok)
	}
	if p, ok := detectCycle([]string{"A", "B", "C", "A", "B", "C"}, 8); !ok || p != 3 {
		t.Fatalf("A B C A B C = period 3, got (p=%d ok=%v)", p, ok)
	}
	if _, ok := detectCycle([]string{"A", "B", "A", "C"}, 8); ok {
		t.Fatalf("A B A C must not be a cycle")
	}
	if _, ok := detectCycle([]string{"A"}, 8); ok {
		t.Fatalf("single element must not be a cycle")
	}
	// maxPeriod bounds detection: period 4 with maxPeriod 3 is not reported.
	if _, ok := detectCycle([]string{"A", "B", "C", "D", "A", "B", "C", "D"}, 3); ok {
		t.Fatalf("period 4 must not be reported when maxPeriod=3")
	}
}

func TestToolMatcherBlocksOnCycle(t *testing.T) {
	m := NewToolMatcher(ToolConfig{}) // limit 2, maxPeriod 8
	m.now = staticClock(time.Unix(1_000_000, 0))
	agent := AgentID("a")
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"A"}},{"function":{"name":"B"}}]}]}`)

	if res := m.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
		t.Fatalf("request 1 must pass, got %+v", res)
	}
	if res := m.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
		t.Fatalf("request 2 must pass (first confirmation), got %+v", res)
	}
	res := m.CheckPayload(agent, Fingerprint("fp"), body)
	if !res.Block {
		t.Fatalf("request 3 must block (second confirmation), got %+v", res)
	}
	if res.Reason != ReasonToolCycle {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonToolCycle)
	}
}

func TestToolMatcherNoCyclePasses(t *testing.T) {
	m := NewToolMatcher(ToolConfig{})
	m.now = staticClock(time.Unix(1_000_000, 0))
	agent := AgentID("a")
	for i := 0; i < 10; i++ {
		body := []byte(fmt.Sprintf(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"tool%d"}}]}]}`, i))
		if res := m.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
			t.Fatalf("unique tools must not block at %d, got %+v", i, res)
		}
	}
}

func TestToolMatcherNoToolSurfacePasses(t *testing.T) {
	m := NewToolMatcher(ToolConfig{})
	m.now = staticClock(time.Unix(1_000_000, 0))
	agent := AgentID("a")
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	for i := 0; i < 10; i++ {
		if res := m.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
			t.Fatalf("no tool surface must pass at %d, got %+v", i, res)
		}
	}
}

func TestToolMatcherTTLExpiry(t *testing.T) {
	clock := newFakeClock(time.Unix(1_000_000, 0))
	m := NewToolMatcher(ToolConfig{TTL: time.Minute})
	m.now = clock.now
	agent := AgentID("a")
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"A"}},{"function":{"name":"B"}}]}]}`)

	m.CheckPayload(agent, Fingerprint("fp"), body)
	m.CheckPayload(agent, Fingerprint("fp"), body)
	if res := m.CheckPayload(agent, Fingerprint("fp"), body); !res.Block {
		t.Fatalf("request 3 must block, got %+v", res)
	}

	clock.advance(2 * time.Minute)
	if res := m.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
		t.Fatalf("request after TTL must pass, got %+v", res)
	}
}

func TestToolMatcherAgentIsolation(t *testing.T) {
	m := NewToolMatcher(ToolConfig{})
	m.now = staticClock(time.Unix(1_000_000, 0))
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"A"}},{"function":{"name":"B"}}]}]}`)
	a, b := AgentID("a"), AgentID("b")

	m.CheckPayload(a, Fingerprint("fp"), body)
	m.CheckPayload(a, Fingerprint("fp"), body)
	if res := m.CheckPayload(b, Fingerprint("fp"), body); res.Block {
		t.Fatalf("agent b must not be contaminated by a, got %+v", res)
	}
	if res := m.CheckPayload(a, Fingerprint("fp"), body); !res.Block {
		t.Fatalf("agent a must block on its 3rd request, got %+v", res)
	}
}

func TestToolMatcherReset(t *testing.T) {
	m := NewToolMatcher(ToolConfig{})
	m.now = staticClock(time.Unix(1_000_000, 0))
	agent := AgentID("a")
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"A"}},{"function":{"name":"B"}}]}]}`)

	m.CheckPayload(agent, Fingerprint("fp"), body)
	m.CheckPayload(agent, Fingerprint("fp"), body)
	m.CheckPayload(agent, Fingerprint("fp"), body) // blocks
	if n := m.Reset(agent); n != 1 {
		t.Fatalf("Reset removed %d, want 1", n)
	}
	if res := m.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
		t.Fatalf("after reset must pass, got %+v", res)
	}
}

func TestToolMatcherConcurrency(t *testing.T) {
	m := NewToolMatcher(ToolConfig{})
	m.now = staticClock(time.Unix(1_000_000, 0))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				body := []byte(fmt.Sprintf(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"tool%d"}}]}]}`, j%5))
				m.CheckPayload(AgentID("a"), Fingerprint("fp"), body)
			}
		}()
	}
	wg.Wait()
	if res := m.CheckPayload(AgentID("fresh"), Fingerprint("fp"), []byte(`{"messages":[{"role":"user","content":"hi"}]}`)); res.Block {
		t.Fatalf("fresh agent must pass after churn, got %+v", res)
	}
}
