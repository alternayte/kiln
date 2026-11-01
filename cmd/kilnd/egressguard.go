package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alternayte/kiln/internal/network"
)

// cmdEgressGuard enforces the egress policy of one sandbox. kilnd serve launches
// it once per sandbox, and it outlives a restart of the daemon.
func cmdEgressGuard(args []string) error {
	fs := flag.NewFlagSet("egressguard", flag.ContinueOnError)
	sock := fs.String("sock", "", "control socket path")
	tap := fs.String("tap", "", "TAP device of the sandbox")
	mark := fs.Int("mark", 0, "firewall mark of the sandbox")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sock == "" || *tap == "" || *mark == 0 {
		return fmt.Errorf("egressguard: --sock, --tap and --mark are required")
	}
	// The daemon that started this process may be gone, and with it the read
	// end of a log pipe. A failed log line must not end the guard.
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return network.ServeGuard(ctx, network.GuardConfig{Sock: *sock, Tap: *tap, Mark: *mark})
}
