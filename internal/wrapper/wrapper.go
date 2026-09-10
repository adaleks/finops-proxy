package wrapper

import (
	"context"
	"io"
	"log"
	"os"
	"sync"
)

// Run executes the child binary under the loop-interception supervision loop
// and returns the process exit code suitable for os.Exit.
//
// The state machine (WRAPPER_PLAN.md §8): on a loop marker the child is killed,
// its history cleared on the proxy, and it is restarted with a recovery prompt
// up to MaxRestarts times. A clean exit 0 returns 0; a non-zero exit without a
// marker is optionally retried; an external interrupt returns 130.
func Run(ctx context.Context, cfg Config, bin string, args []string,
	stdin io.Reader, stdout, stderr io.Writer) int {

	client := NewProxyClient(cfg.ProxyURL, cfg.DeleteTimeout, cfg.MaxDeleteRetries)
	logger := log.New(stderr, "finops-run: ", log.LstdFlags)

	// Live status bar (optional). Renders to stderr only when it is a terminal;
	// the derived context is cancelled when Run returns so the poller never leaks.
	var bar *statusBar
	if cfg.Watch {
		bar = newStatusBar(client, stderr, cfg.WatchInterval, isTerminal(stderr))
		barCtx, barCancel := context.WithCancel(ctx)
		bar.start(barCtx)
		defer barCancel()
	}

	for attempt := 0; ; attempt++ {
		env := childEnv(cfg, attempt > 0)

		matched := make(chan struct{})
		var once sync.Once
		fire := func() { once.Do(func() { close(matched) }) }

		attemptCtx, cancel := context.WithCancel(ctx)
		child, err := startChild(attemptCtx, cfg, bin, args, env, stdin, stdout, stderr, fire)
		if err != nil {
			logger.Printf("failed to start %q: %v", bin, err)
			cancel()
			return 1
		}

		type result struct {
			err  error
			code int
		}
		exitCh := make(chan result, 1)
		go func() {
			err := child.Wait()
			exitCh <- result{err: err, code: child.ExitCode()}
		}()

		loopDetected := false
		var res result

		select {
		case <-matched:
			// Loop signal seen: kill the child (graceful escalation) and reap.
			loopDetected = true
			cancel()
			res = <-exitCh
		case res = <-exitCh:
			// Child exited on its own; a marker that arrived in the same
			// instant still counts as a loop.
			select {
			case <-matched:
				loopDetected = true
			default:
			}
		case <-ctx.Done():
			// External SIGINT/SIGTERM: the child is killed via context
			// cancellation (attemptCtx derives from ctx). Reap and exit.
			cancel()
			<-exitCh
			return 130
		}
		cancel() // release the attempt context on every path

		if loopDetected {
			if bar != nil {
				bar.loopDetected()
			}
			if err := client.DeleteHistory(ctx, cfg.AgentID); err != nil {
				logger.Printf("delete history for %q: %v (fail-open, continuing)", cfg.AgentID, err)
			}
			if cfg.MaxRestarts >= 0 && attempt >= cfg.MaxRestarts {
				logger.Printf("loop detected; restart limit %d reached, giving up", cfg.MaxRestarts)
				return 42
			}
			logger.Printf("loop detected; restarting with recovery prompt (restart %d)", attempt+1)
			continue
		}

		if res.err == nil {
			return 0
		}
		if cfg.RetryOnNonzero && (cfg.MaxRestarts < 0 || attempt < cfg.MaxRestarts) {
			logger.Printf("child exited with code %d (no loop marker); restarting (restart %d)", res.code, attempt+1)
			continue
		}
		return res.code
	}
}

// childEnv builds the child environment, always exporting the agent id and, on
// a restart, the recovery prompt.
func childEnv(cfg Config, recovery bool) []string {
	env := os.Environ()
	env = append(env, "FINOPS_AGENT_ID="+cfg.AgentID)
	if recovery {
		env = append(env, "FINOPS_RECOVERY_PROMPT="+cfg.RecoveryPrompt)
	}
	return env
}
