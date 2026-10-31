// Command kiln is the client for a Kiln gateway or host, for people and for
// agents. kilnd is the host.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/alternayte/kiln/internal/apispec"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// cli is one invocation.
type cli struct {
	ctx    context.Context
	in     io.Reader
	out    io.Writer
	errOut io.Writer
	json   bool
}

// failure is a command that ended badly. It carries its exit code, and next
// names the command that recovers.
type failure struct {
	exit    int
	code    string
	message string
	next    string
}

func (f *failure) Error() string { return f.code + ": " + f.message }

func usagef(next, format string, args ...any) *failure {
	return &failure{exit: apispec.ExitUsage, code: "usage", message: fmt.Sprintf(format, args...), next: next}
}

func run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	c := &cli{ctx: ctx, in: in, out: out, errOut: errOut}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" {
			c.json = true
		}
	}
	code, err := c.dispatch(args)
	if err != nil {
		return c.report(err)
	}
	return code
}

// dispatch finds the command. Every operation in apispec has one.
func (c *cli) dispatch(args []string) (int, error) {
	if len(args) == 0 {
		c.help()
		return apispec.ExitUsage, nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		c.help()
		return apispec.ExitOK, nil
	case "version", "-v", "--version":
		fmt.Fprintln(c.out, apispec.Version)
		return apispec.ExitOK, nil
	case "init":
		return apispec.ExitOK, c.cmdInit(args[1:])
	case "preview":
		return apispec.ExitOK, c.cmdPreview(args[1:])
	}
	for _, op := range apispec.Operations() {
		words := strings.Fields(op.Command)
		if len(args) < len(words) || strings.Join(args[:len(words)], " ") != op.Command {
			continue
		}
		return c.runOperation(op, args[len(words):])
	}
	return apispec.ExitUsage, usagef("kiln help", "unknown command %q", strings.Join(args, " "))
}

// report prints a failure and returns its exit code.
func (c *cli) report(err error) int {
	f, ok := err.(*failure)
	if !ok {
		f = &failure{exit: apispec.ExitFailure, code: "failed", message: err.Error()}
	}
	if c.json {
		body := map[string]any{"error": map[string]any{"code": f.code, "message": f.message}}
		if f.next != "" {
			body["next"] = f.next
		}
		printJSON(c.out, body)
		return f.exit
	}
	fmt.Fprintf(c.errOut, "kiln: %s: %s\n", f.code, f.message)
	if f.next != "" {
		fmt.Fprintf(c.errOut, "next: %s\n", f.next)
	}
	return f.exit
}

func (c *cli) help() {
	var names []string
	for _, op := range apispec.Operations() {
		names = append(names, "  kiln "+op.Command)
	}
	sort.Strings(names)
	fmt.Fprintf(c.errOut, `usage: kiln <command> [args] [--json]

Reads KILN_URL and KILN_API_KEY. With no KILN_URL, it reads config.json under
KILN_ROOT when that file is readable, which serves an operator on the host.

  kiln init [dir] [--force]
  kiln preview up --image X --port N
  kiln preview down
  kiln version
%s

kiln <command> --help names the flags of one command.

Exit codes:
%s`, strings.Join(names, "\n"), apispec.ExitCodeTable())
}
