// Package wrapper implements the finops-run CLI loop-interception supervisor.
//
// It runs a child CLI agent, watches its output for a loop signal (the proxy's
// agent_loop_exception 429), and on detection kills the child, clears its loop
// history on the proxy, and restarts it with a recovery prompt.
package wrapper

import "time"

// Config holds the tunables for a supervision run. Flag-exposed values live in
// args.go; the remaining fields are internal knobs with sensible defaults.
type Config struct {
	// AgentID scopes loop history on the proxy. Empty means "derive from the
	// child command line" (resolved in ParseArgs).
	AgentID string

	// ProxyURL is the base URL of the FinOps proxy (default http://localhost:8080).
	ProxyURL string

	// MaxRestarts caps automatic restarts. -1 is unlimited, 0 means never
	// auto-restart (run once and propagate the child's exit).
	MaxRestarts int

	// RecoveryPrompt is delivered to the child on restart via FINOPS_RECOVERY_PROMPT.
	RecoveryPrompt string

	// Marker is the literal substring that signals a loop (default agent_loop_exception).
	Marker string

	// KillGrace is how long the child gets to shut down on SIGTERM before the
	// process group is SIGKILLed.
	KillGrace time.Duration

	// DeleteTimeout is the HTTP client timeout for the cleanup DELETE call.
	DeleteTimeout time.Duration

	// MaxDeleteRetries is how many times the DELETE is retried on transient failure.
	MaxDeleteRetries int

	// RetryOnNonzero treats a child exit with a non-zero code and no loop
	// marker as a recoverable failure worth one restart attempt.
	RetryOnNonzero bool

	// Watch renders a live status bar by polling the proxy's /v1/metrics.
	Watch bool

	// WatchInterval is the status bar refresh interval.
	WatchInterval time.Duration

	// Version is set by the -version flag; when true ParseArgs returns
	// immediately (no child binary required) so the caller can print the build
	// version and exit.
	Version bool
}

// DefaultConfig returns the recommended defaults.
func DefaultConfig() Config {
	return Config{
		ProxyURL:         "http://localhost:8080",
		MaxRestarts:      3,
		RecoveryPrompt:   "Loop detected on previous file, try an alternative solution.",
		Marker:           "agent_loop_exception",
		KillGrace:        5 * time.Second,
		DeleteTimeout:    5 * time.Second,
		MaxDeleteRetries: 3,
		RetryOnNonzero:   true,
		Watch:            false,
		WatchInterval:    time.Second,
	}
}
