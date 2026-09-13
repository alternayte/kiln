package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alternayte/kiln/internal/snapshot"
)

// cmdSnapfault serves guest memory pages for one restoring microVM. kiln serve
// launches it once per restore.
func cmdSnapfault(args []string) error {
	fs := flag.NewFlagSet("snapfault", flag.ContinueOnError)
	mem := fs.String("mem", "", "template memory file")
	sock := fs.String("sock", "", "UFFD socket path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mem == "" || *sock == "" {
		return fmt.Errorf("snapfault: --mem and --sock are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := snapshot.Serve(ctx, *mem, *sock, nil); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
