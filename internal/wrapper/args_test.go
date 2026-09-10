package wrapper

import (
	"strings"
	"testing"
)

func TestParseArgsFlagSplit(t *testing.T) {
	cfg, bin, args, err := ParseArgs([]string{"--agent-id", "x", "claude", "--prompt", "p"})
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if bin != "claude" {
		t.Errorf("bin = %q, want claude", bin)
	}
	if len(args) != 2 || args[0] != "--prompt" || args[1] != "p" {
		t.Errorf("args = %v, want [--prompt p]", args)
	}
	if cfg.AgentID != "x" {
		t.Errorf("agent id = %q, want x", cfg.AgentID)
	}
}

func TestParseArgsPassthroughFlagLike(t *testing.T) {
	// A flag-like token after the binary must go to the child, not finops-run.
	cfg, bin, args, err := ParseArgs([]string{"claude", "--agent-id", "y"})
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if bin != "claude" {
		t.Errorf("bin = %q, want claude", bin)
	}
	if len(args) != 2 || args[0] != "--agent-id" || args[1] != "y" {
		t.Errorf("args = %v, want [--agent-id y]", args)
	}
	if cfg.AgentID == "y" {
		t.Error("agent id must be derived, not taken from child args")
	}
}

func TestParseArgsMissingBinary(t *testing.T) {
	if _, _, _, err := ParseArgs(nil); err == nil {
		t.Fatal("ParseArgs with no args must fail")
	}
}

func TestDefaultAgentIDStable(t *testing.T) {
	a := DefaultAgentID("claude", []string{"--prompt", "x"})
	b := DefaultAgentID("claude", []string{"--prompt", "x"})
	if a != b {
		t.Fatalf("DefaultAgentID not stable: %q != %q", a, b)
	}
	if a == DefaultAgentID("claude", []string{"--prompt", "y"}) {
		t.Fatal("different args must yield different ids")
	}
	if len(a) != 16 || strings.ContainsAny(a, " \n/") {
		t.Fatalf("agent id %q is not a clean 16-hex token", a)
	}
}
