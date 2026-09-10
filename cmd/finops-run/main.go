// Command finops-run runs a CLI agent as a child, watching its output for a
// loop signal, and on detection kills it, clears its proxy history, and
// restarts it with a recovery prompt.
//
// Usage: finops-run [flags] <binary> [args...]
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/adaleks/finops-proxy/internal/wrapper"
)

// version is the finops-run build version, overridable at build time via
// -ldflags "-X main.version=<semver>" — the GoReleaser release workflow does
// exactly that.
var version = "finops-run core dev"

func main() {
	cfg, bin, args, err := wrapper.ParseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "finops-run:", err)
		os.Exit(2)
	}
	if cfg.Version {
		fmt.Fprintln(os.Stdout, version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(wrapper.Run(ctx, cfg, bin, args, os.Stdin, os.Stdout, os.Stderr))
}
