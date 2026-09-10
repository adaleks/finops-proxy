package wrapper

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Child wraps a running child process with the group-kill machinery that
// guarantees a graceful SIGTERM, a SIGKILL escalation, and reaping without
// zombies (see WRAPPER_PLAN.md §6).
type Child struct {
	cmd      *exec.Cmd
	grace    time.Duration
	done     chan struct{} // closed once Wait has reaped the child
	doneOnce sync.Once
}

// startChild configures and starts the child process. It returns once Start has
// succeeded; the caller is responsible for calling Wait exactly once.
//
// The child is placed in its own process group (Setpgid) so that sigGroup can
// reach the whole tree. exec's Cancel sends a graceful group SIGTERM on context
// cancellation; a watchdog escalates to a group SIGKILL after grace; WaitDelay
// is the final backstop that bounds Wait even if a descendant survives holding
// a pipe open.
func startChild(ctx context.Context, cfg Config, bin string, args []string,
	env []string, stdin io.Reader, stdout, stderr io.Writer,
	fire func()) (*Child, error) {

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = newLineTee(stdout, cfg.Marker, fire)
	cmd.Stderr = newLineTee(stderr, cfg.Marker, fire)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// exec calls this when the context is done. Return os.ErrProcessDone
		// when the process is already gone so Wait reports the real exit status.
		return sigGroup(cmd, syscall.SIGTERM)
	}
	cmd.WaitDelay = cfg.KillGrace + 3*time.Second

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	c := &Child{cmd: cmd, grace: cfg.KillGrace, done: make(chan struct{})}

	go func() {
		select {
		case <-ctx.Done():
		case <-c.done:
			return
		}
		select {
		case <-time.After(c.grace):
			// Re-check: never signal a group that has already been reaped.
			select {
			case <-c.done:
				return
			default:
			}
			_ = sigGroup(cmd, syscall.SIGKILL) // ESRCH is ignored
		case <-c.done:
		}
	}()

	return c, nil
}

// Wait blocks until the child exits and returns its exit error (nil on exit 0).
// It must be called exactly once per Child; it also stops the watchdog.
func (c *Child) Wait() error {
	err := c.cmd.Wait()
	c.doneOnce.Do(func() { close(c.done) })
	return err
}

// ExitCode returns the child's exit status suitable for os.Exit: 128+signal for
// signal-terminated children, otherwise the process exit code.
func (c *Child) ExitCode() int {
	if c.cmd.ProcessState == nil {
		return 1
	}
	if ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return c.cmd.ProcessState.ExitCode()
}

// sigGroup signals the child's whole process group, treating an absent or
// already-gone group (ESRCH) as os.ErrProcessDone so Cancel reports it
// correctly. It must not read cmd.ProcessState: that field is written by Wait
// and is only safe to read after Wait returns.
func sigGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
