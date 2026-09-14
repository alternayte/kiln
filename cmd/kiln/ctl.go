package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alternayte/kiln/client"
)

// cmdCtl is the CLI client for the HTTP API. It reads config.json for the
// control address and the bearer token.
func cmdCtl(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("ctl needs a command: health, templates, sandboxes, events, exec, destroy")
	}
	root := os.Getenv("KILN_ROOT")
	if root == "" {
		root = defaultRoot
	}
	cfg, err := readConfig(filepath.Join(root, "config.json"))
	if err != nil {
		return err
	}
	base := cfg.ControlAddr
	if base == "" {
		base = "127.0.0.1:8080"
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	cli := client.New(base, cfg.BearerToken)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "health":
		v, err := cli.Health(ctx)
		if err != nil {
			return err
		}
		return printJSON(v)
	case "templates":
		if len(args) > 1 {
			v, err := cli.Template(ctx, args[1])
			if err != nil {
				return err
			}
			return printJSON(v)
		}
		list, err := cli.Templates(ctx)
		if err != nil {
			return err
		}
		return printJSON(list)
	case "sandboxes":
		if len(args) > 1 {
			v, err := cli.Sandbox(ctx, args[1])
			if err != nil {
				return err
			}
			return printJSON(v)
		}
		list, err := cli.Sandboxes(ctx)
		if err != nil {
			return err
		}
		return printJSON(list)
	case "events":
		sandboxID := ""
		if len(args) > 1 {
			sandboxID = args[1]
		}
		return cli.Events(ctx, 0, sandboxID, func(e client.Event) error { return printJSON(e) })
	case "exec":
		return ctlExec(ctx, cli, args[1:])
	case "destroy":
		if len(args) != 2 {
			return fmt.Errorf("ctl destroy needs one sandbox id")
		}
		return cli.DeleteSandbox(ctx, args[1])
	}
	return fmt.Errorf("ctl: unknown command %q", args[0])
}

// ctlExec runs one command and writes its streams through.
func ctlExec(ctx context.Context, cli *client.Client, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("ctl exec needs a sandbox id and a command")
	}
	id, rest := args[0], args[1:]
	cwd := ""
	if len(rest) >= 2 && rest[0] == "--cwd" {
		cwd = rest[1]
		rest = rest[2:]
	}
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return fmt.Errorf("ctl exec needs a command")
	}
	res, err := cli.Exec(ctx, id, client.ExecRequest{Cmd: rest, Cwd: cwd, TimeoutSeconds: 300})
	if err != nil {
		return err
	}
	if _, err := os.Stdout.WriteString(res.Stdout); err != nil {
		return err
	}
	if _, err := os.Stderr.WriteString(res.Stderr); err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("ctl exec: exit code %d", res.ExitCode)
	}
	return nil
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(b))
	return err
}
