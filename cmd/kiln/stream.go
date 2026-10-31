package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/coder/websocket"
	"golang.org/x/term"

	"github.com/alternayte/kiln/internal/apispec"
)

// asExecFailure marks a failure of Kiln itself during kiln exec, so a caller
// tells it apart from the exit code of the command.
func asExecFailure(err error) error {
	if f, ok := err.(*failure); ok && f.exit != apispec.ExitUsage {
		f.exit = apispec.ExitExecFailed
	}
	return err
}

// execCode turns the end of a command into the exit code of kiln exec.
func execCode(exitCode int, timedOut bool) int {
	switch {
	case timedOut:
		return apispec.ExitExecTimedOut
	case exitCode < 0:
		return apispec.ExitExecFailed
	}
	return exitCode
}

// runExec runs one command in a sandbox. It streams the output as the command
// runs, and with --json it prints the whole answer at the end.
func (c *cli) runExec(op apispec.Operation, p *parsed) (int, error) {
	if len(p.positional) > 0 && len(p.rest) == 0 {
		p.rest = p.positional[1:]
		p.positional = p.positional[:1]
	}
	if len(p.positional) != 1 || len(p.rest) == 0 {
		return apispec.ExitUsage, usagef("kiln exec <id> -- <command>", "kiln exec needs a sandbox id and a command")
	}
	path, err := buildPath(op, p)
	if err != nil {
		return apispec.ExitUsage, err
	}
	body := p.body
	cmd := make([]any, len(p.rest))
	for i, word := range p.rest {
		cmd[i] = word
	}
	body["cmd"] = cmd
	if c.json {
		var answer struct {
			ExitCode int  `json:"exit_code"`
			TimedOut bool `json:"timed_out"`
		}
		raw, err := c.call(c.ctx, op.Method, path, body, &answer)
		if err != nil {
			return apispec.ExitOK, asExecFailure(err)
		}
		c.printAnswer(op, raw, "")
		return execCode(answer.ExitCode, answer.TimedOut), nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return apispec.ExitUsage, usagef("", "%v", err)
	}
	resp, err := c.request(c.ctx, op.Method, path, "text/event-stream", strings.NewReader(string(encoded)))
	if err != nil {
		return apispec.ExitOK, asExecFailure(err)
	}
	defer resp.Body.Close()
	return c.readExecStream(resp.Body)
}

// readExecStream writes the stream through and returns the exit code.
func (c *cli) readExecStream(r io.Reader) (int, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	event := ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			switch event {
			case "stdout", "stderr":
				var text string
				if err := json.Unmarshal([]byte(data), &text); err != nil {
					continue
				}
				if event == "stdout" {
					fmt.Fprint(c.out, text)
				} else {
					fmt.Fprint(c.errOut, text)
				}
			case "exit":
				var end struct {
					ExitCode int  `json:"exit_code"`
					TimedOut bool `json:"timed_out"`
				}
				if err := json.Unmarshal([]byte(data), &end); err != nil {
					return apispec.ExitOK, &failure{exit: apispec.ExitExecFailed, code: "failed", message: err.Error()}
				}
				return execCode(end.ExitCode, end.TimedOut), nil
			case "error":
				var text string
				_ = json.Unmarshal([]byte(data), &text)
				return apispec.ExitOK, &failure{exit: apispec.ExitExecFailed, code: "failed", message: text}
			}
		}
	}
	message := "the stream ended before the command did"
	if err := scanner.Err(); err != nil {
		message = err.Error()
	}
	return apispec.ExitOK, &failure{exit: apispec.ExitExecFailed, code: "network", message: message, next: "kiln health"}
}

// runEvents prints the event stream, one JSON object per line.
func (c *cli) runEvents(op apispec.Operation, p *parsed) error {
	path, err := buildPath(op, p)
	if err != nil {
		return err
	}
	resp, err := c.request(c.ctx, "GET", path, "text/event-stream", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		if data, ok := strings.CutPrefix(scanner.Text(), "data:"); ok {
			fmt.Fprintln(c.out, strings.TrimSpace(data))
		}
	}
	// An interrupt is how a person leaves a stream that never ends.
	if err := scanner.Err(); err != nil && c.ctx.Err() == nil {
		return &failure{exit: apispec.ExitFailure, code: "network", message: err.Error(), next: "kiln health"}
	}
	return nil
}

func (c *cli) runReadFile(op apispec.Operation, p *parsed) error {
	path, err := buildPath(op, p)
	if err != nil {
		return err
	}
	resp, err := c.request(c.ctx, "GET", path, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(c.out, resp.Body); err != nil {
		return &failure{exit: apispec.ExitFailure, code: "network", message: err.Error()}
	}
	return nil
}

// runWriteFile sends stdin, or the file --from names.
func (c *cli) runWriteFile(op apispec.Operation, p *parsed) error {
	path, err := buildPath(op, p)
	if err != nil {
		return err
	}
	source := c.in
	if p.from != "" {
		f, err := os.Open(p.from)
		if err != nil {
			return usagef("kiln files write --help", "--from: %v", err)
		}
		defer f.Close()
		source = f
	}
	resp, err := c.request(c.ctx, "PUT", path, "", source)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// runTerminal opens an interactive terminal in a sandbox.
func (c *cli) runTerminal(op apispec.Operation, p *parsed) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return usagef("kiln exec <id> -- <command>", "kiln terminal needs a terminal on stdin and stdout")
	}
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		cols, rows = 80, 24
	}
	if !p.query.Has("cols") {
		p.query.Set("cols", fmt.Sprint(cols))
		p.query.Set("rows", fmt.Sprint(rows))
	}
	path, err := buildPath(op, p)
	if err != nil {
		return err
	}
	base, token, err := connection()
	if err != nil {
		return err
	}
	target, err := url.Parse(base + path)
	if err != nil {
		return usagef("", "%v", err)
	}
	target.Scheme = strings.Replace(target.Scheme, "http", "ws", 1)
	conn, resp, err := websocket.Dial(c.ctx, target.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		if resp != nil && resp.Body != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			return apiFailure(resp.StatusCode, b, path)
		}
		return &failure{exit: apispec.ExitFailure, code: "network", message: err.Error(), next: "check KILN_URL, then run kiln health"}
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)

	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return usagef("", "%v", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), state)

	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	go func() {
		for range resize {
			if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
				b, _ := json.Marshal(map[string]int{"cols": w, "rows": h})
				_ = conn.Write(c.ctx, websocket.MessageText, b)
			}
		}
	}()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && conn.Write(c.ctx, websocket.MessageBinary, buf[:n]) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		_, data, err := conn.Read(c.ctx)
		if err != nil {
			return nil
		}
		if _, err := os.Stdout.Write(data); err != nil {
			return nil
		}
	}
}
