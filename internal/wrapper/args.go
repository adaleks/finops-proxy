package wrapper

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"io"
)

// ParseArgs parses finops-run's own flags out of args (os.Args[1:]) and returns
// the resolved config, the child binary, and its passthrough arguments.
//
// finops-run flags must precede the binary name: stdlib flag stops parsing at
// the first non-flag argument, so every token after <binary> is passed through
// untouched — even tokens that collide with finops-run flag names.
func ParseArgs(args []string) (cfg Config, bin string, passthrough []string, err error) {
	cfg = DefaultConfig()

	fs := flag.NewFlagSet("finops-run", flag.ContinueOnError)
	fs.StringVar(&cfg.AgentID, "agent-id", "", "agent id for history scoping (default: derived from the child command)")
	fs.StringVar(&cfg.ProxyURL, "proxy-url", cfg.ProxyURL, "FinOps proxy base URL")
	fs.IntVar(&cfg.MaxRestarts, "max-restarts", cfg.MaxRestarts, "max automatic restarts (-1 unlimited, 0 none)")
	fs.StringVar(&cfg.RecoveryPrompt, "recovery-prompt", cfg.RecoveryPrompt, "prompt delivered to the child on restart")
	fs.BoolVar(&cfg.RetryOnNonzero, "retry-on-nonzero", cfg.RetryOnNonzero, "treat a non-zero child exit without a loop marker as recoverable")
	fs.BoolVar(&cfg.Watch, "watch", cfg.Watch, "render a live status bar from the proxy /v1/metrics")
	fs.BoolVar(&cfg.Watch, "stats", cfg.Watch, "alias for -watch")
	fs.DurationVar(&cfg.WatchInterval, "watch-interval", cfg.WatchInterval, "status bar refresh interval")
	fs.BoolVar(&cfg.Version, "version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return cfg, "", nil, err
	}

	if cfg.Version {
		return cfg, "", nil, nil
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return cfg, "", nil, errors.New("missing child binary (usage: finops-run [flags] <binary> [args...])")
	}

	bin = rest[0]
	passthrough = rest[1:]

	if cfg.AgentID == "" {
		cfg.AgentID = DefaultAgentID(bin, passthrough)
	}
	return cfg, bin, passthrough, nil
}

// DefaultAgentID derives a stable 16-hex-char id from the full child command
// line so restarts of the same command hit the same history bucket. Two
// concurrent runs of the identical command collide — a documented limitation;
// use --agent-id to disambiguate.
func DefaultAgentID(bin string, args []string) string {
	h := sha256.New()
	io.WriteString(h, bin)
	h.Write([]byte{0})
	for _, a := range args {
		io.WriteString(h, a)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
