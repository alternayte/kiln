package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alternayte/kiln/internal/apispec"
)

// parsed is the arguments of one operation.
type parsed struct {
	positional []string
	// rest follows a bare --, so it holds a command for the guest.
	rest    []string
	body    map[string]any
	query   url.Values
	rawBody string
	wait    bool
	timeout time.Duration
	from    string
	help    bool
}

// flagName is the flag a field takes.
func flagName(field string) string { return strings.ReplaceAll(field, "_", "-") }

// pathParams returns the parameters the path names, in order. Any other
// parameter of the operation, such as the size of a terminal, is a flag.
func pathParams(op apispec.Operation) []apispec.Field {
	var out []apispec.Field
	for _, p := range op.Params {
		if strings.Contains(op.Path, "{"+p.Name+"}") || strings.Contains(op.Path, "{"+p.Name+"...}") {
			out = append(out, p)
		}
	}
	return out
}

// parseArgs reads the flags apispec describes for this operation.
func parseArgs(op apispec.Operation, args []string) (*parsed, error) {
	p := &parsed{body: map[string]any{}, query: url.Values{}}
	fields := map[string]apispec.Field{}
	inQuery := map[string]bool{}
	for _, f := range op.Body {
		fields[flagName(f.Name)] = f
	}
	for _, f := range op.Query {
		fields[flagName(f.Name)] = f
		inQuery[flagName(f.Name)] = true
	}
	path := pathParams(op)
	for _, f := range op.Params {
		isPath := false
		for _, q := range path {
			isPath = isPath || q.Name == f.Name
		}
		if !isPath {
			fields[flagName(f.Name)] = f
			inQuery[flagName(f.Name)] = true
		}
	}
	usage := "kiln " + op.Command + " --help"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			p.rest = args[i+1:]
			break
		}
		if !strings.HasPrefix(arg, "--") {
			p.positional = append(p.positional, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg[2:], "=")
		take := func() (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", usagef(usage, "--%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		switch name {
		case "json":
			continue
		case "help":
			p.help = true
			continue
		case "wait":
			p.wait = true
			continue
		case "timeout":
			v, err := take()
			if err != nil {
				return nil, err
			}
			d, derr := time.ParseDuration(v)
			if derr != nil {
				return nil, usagef(usage, "--timeout: %v", derr)
			}
			p.timeout = d
			continue
		case "body":
			v, err := take()
			if err != nil {
				return nil, err
			}
			p.rawBody = v
			continue
		case "from":
			v, err := take()
			if err != nil {
				return nil, err
			}
			p.from = v
			continue
		}
		field, ok := fields[name]
		if !ok {
			return nil, usagef(usage, "unknown flag --%s", name)
		}
		if field.Type == "boolean" {
			v := true
			if hasValue {
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, usagef(usage, "--%s: %v", name, err)
				}
				v = b
			}
			if inQuery[name] {
				p.query.Set(field.Name, strconv.FormatBool(v))
			} else {
				p.body[field.Name] = v
			}
			continue
		}
		v, err := take()
		if err != nil {
			return nil, err
		}
		if inQuery[name] {
			p.query.Add(field.Name, v)
			continue
		}
		if err := setField(p.body, field, v); err != nil {
			return nil, usagef(usage, "--%s: %v", name, err)
		}
	}
	return p, nil
}

// setField stores one flag value under the type apispec gives the field.
func setField(body map[string]any, f apispec.Field, v string) error {
	switch f.Type {
	case "integer":
		n, err := strconv.Atoi(v)
		if err != nil {
			return err
		}
		body[f.Name] = n
	case "array":
		list, _ := body[f.Name].([]any)
		if list == nil {
			list = []any{}
		}
		// An empty value names an empty list, which egress_allow needs.
		if v != "" {
			if f.Items == "object" {
				// One JSON object per flag, or one JSON list of them.
				var many []any
				if err := json.Unmarshal([]byte(v), &many); err == nil {
					list = append(list, many...)
				} else {
					var one map[string]any
					if err := json.Unmarshal([]byte(v), &one); err != nil {
						return fmt.Errorf("want a JSON object or a list of them: %v", err)
					}
					list = append(list, one)
				}
			} else if f.Items == "integer" {
				n, err := strconv.Atoi(v)
				if err != nil {
					return err
				}
				list = append(list, n)
			} else {
				list = append(list, v)
			}
		}
		body[f.Name] = list
	case "object":
		if strings.HasPrefix(strings.TrimSpace(v), "{") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(v), &obj); err != nil {
				return err
			}
			body[f.Name] = obj
			return nil
		}
		key, val, ok := strings.Cut(v, "=")
		if !ok {
			return fmt.Errorf("want KEY=value or a JSON object")
		}
		obj, _ := body[f.Name].(map[string]any)
		if obj == nil {
			obj = map[string]any{}
		}
		obj[key] = val
		body[f.Name] = obj
	default:
		body[f.Name] = v
	}
	return nil
}

// buildPath fills the path from the positional arguments.
func buildPath(op apispec.Operation, p *parsed) (string, error) {
	params := pathParams(op)
	usage := "kiln " + op.Command + " --help"
	// Exec takes its command after the id, so it may hold more words.
	if op.ID != "exec" && len(p.positional) != len(params) {
		return "", usagef(usage, "kiln %s takes %d argument(s) and got %d", op.Command, len(params), len(p.positional))
	}
	if len(p.positional) < len(params) {
		return "", usagef(usage, "kiln %s needs %s", op.Command, params[0].Name)
	}
	path := op.Path
	for i, param := range params {
		value := p.positional[i]
		if strings.Contains(path, "{"+param.Name+"...}") {
			segments := strings.Split(strings.TrimPrefix(value, "/"), "/")
			for j, segment := range segments {
				segments[j] = url.PathEscape(segment)
			}
			path = strings.Replace(path, "{"+param.Name+"...}", strings.Join(segments, "/"), 1)
			continue
		}
		path = strings.Replace(path, "{"+param.Name+"}", url.PathEscape(value), 1)
	}
	if len(p.query) > 0 {
		path += "?" + p.query.Encode()
	}
	return path, nil
}

// requestBody returns the JSON body of a call, from --body or from the flags.
// --body takes JSON, a file path, or - for stdin.
func (c *cli) requestBody(op apispec.Operation, p *parsed) (any, error) {
	usage := "kiln " + op.Command + " --help"
	if p.rawBody != "" {
		raw := []byte(p.rawBody)
		switch {
		case p.rawBody == "-":
			b, err := io.ReadAll(c.in)
			if err != nil {
				return nil, usagef(usage, "--body: %v", err)
			}
			raw = b
		case !strings.HasPrefix(strings.TrimSpace(p.rawBody), "{"):
			b, err := os.ReadFile(p.rawBody)
			if err != nil {
				return nil, usagef(usage, "--body: %v", err)
			}
			raw = b
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, usagef(usage, "--body: %v", err)
		}
		return out, nil
	}
	if len(op.Body) == 0 {
		return nil, nil
	}
	for _, f := range op.Body {
		if _, ok := p.body[f.Name]; f.Required && !ok && !(op.ID == "exec" && f.Name == "cmd") {
			return nil, usagef(usage, "--%s is required", flagName(f.Name))
		}
	}
	return p.body, nil
}

// runOperation runs one apispec operation.
func (c *cli) runOperation(op apispec.Operation, args []string) (int, error) {
	p, err := parseArgs(op, args)
	if err != nil {
		return apispec.ExitUsage, err
	}
	if p.help {
		c.operationHelp(op)
		return apispec.ExitOK, nil
	}
	switch op.ID {
	case "exec":
		return c.runExec(op, p)
	case "terminal":
		return apispec.ExitOK, c.runTerminal(op, p)
	case "events":
		return apispec.ExitOK, c.runEvents(op, p)
	case "readFile":
		return apispec.ExitOK, c.runReadFile(op, p)
	case "writeFile":
		return apispec.ExitOK, c.runWriteFile(op, p)
	}
	path, err := buildPath(op, p)
	if err != nil {
		return apispec.ExitUsage, err
	}
	body, err := c.requestBody(op, p)
	if err != nil {
		return apispec.ExitUsage, err
	}
	raw, err := c.call(c.ctx, op.Method, path, body, nil)
	if err != nil {
		return apispec.ExitOK, err
	}
	if op.ID == "createTemplate" && p.wait {
		name, _ := p.body["name"].(string)
		if b, ok := body.(map[string]any); ok && name == "" {
			name, _ = b["name"].(string)
		}
		row, err := c.waitTemplate(name, p.timeout)
		if err != nil {
			return apispec.ExitOK, err
		}
		raw = row
	}
	c.printAnswer(op, raw, nextCommand(op, raw, p))
	return apispec.ExitOK, nil
}

// waitTemplate polls one template until its build ends, and returns its row.
func (c *cli) waitTemplate(name string, timeout time.Duration) ([]byte, error) {
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	path := "/v1/templates/" + url.PathEscape(name)
	for {
		var row struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		raw, err := c.call(c.ctx, "GET", path, nil, &row)
		switch f, isFailure := err.(*failure); {
		case err == nil && row.State != "building":
			if row.State != "ready" {
				return nil, &failure{exit: apispec.ExitFailure, code: "build_failed",
					message: fmt.Sprintf("template %s is %s: %s", name, row.State, row.Error), next: "kiln templates get " + name}
			}
			return raw, nil
		case err != nil && (!isFailure || f.code != "network"):
			// A network fault during a long build is not a verdict on it.
			return nil, err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, &failure{exit: apispec.ExitTimeout, code: "timeout",
				message: fmt.Sprintf("template %s is still building after %s", name, timeout), next: "kiln templates get " + name}
		}
		select {
		case <-c.ctx.Done():
			return nil, &failure{exit: apispec.ExitFailure, code: "interrupted", message: c.ctx.Err().Error()}
		case <-time.After(2 * time.Second):
		}
	}
}

// nextCommand names what a caller usually runs after this answer.
func nextCommand(op apispec.Operation, raw []byte, p *parsed) string {
	var answer struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		State     string `json:"state"`
		Sandboxes []struct {
			ID string `json:"id"`
		} `json:"sandboxes"`
	}
	_ = json.Unmarshal(raw, &answer)
	switch op.ID {
	case "createTemplate":
		if answer.State == "ready" {
			return fmt.Sprintf("kiln sandboxes create --template %s --lifecycle ephemeral --idle-seconds 600", answer.Name)
		}
		return "kiln templates get " + answer.Name
	case "createSandbox":
		return fmt.Sprintf("kiln exec %s -- <command>", answer.ID)
	case "fork", "restore":
		if len(answer.Sandboxes) > 0 {
			return fmt.Sprintf("kiln exec %s -- <command>", answer.Sandboxes[0].ID)
		}
	}
	return ""
}

// printAnswer writes one answer as JSON or as text. With --json, next becomes
// a field of the answer.
func (c *cli) printAnswer(op apispec.Operation, raw []byte, next string) {
	var decoded any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			fmt.Fprintf(c.out, "%s\n", raw)
			return
		}
	}
	if c.json {
		if decoded == nil {
			decoded = map[string]any{"ok": true}
		}
		if obj, ok := decoded.(map[string]any); ok && next != "" {
			obj["next"] = next
		}
		printJSON(c.out, decoded)
		return
	}
	if op.ID == "logs" {
		c.printLogs(decoded)
	} else if decoded != nil {
		printText(c.out, decoded)
	}
	if next != "" {
		fmt.Fprintf(c.errOut, "next: %s\n", next)
	}
}

// printLogs prints the logs answer for a person.
func (c *cli) printLogs(decoded any) {
	obj, _ := decoded.(map[string]any)
	if build, ok := obj["build"].(map[string]any); ok {
		fmt.Fprintf(c.out, "build: %v is %v\n", build["template"], build["state"])
		if e, _ := build["error"].(string); e != "" {
			fmt.Fprintln(c.out, e)
		}
	}
	if note, _ := obj["note"].(string); note != "" {
		fmt.Fprintln(c.out, note)
	}
	services, _ := obj["services"].([]any)
	defer func() {
		for _, entry := range services {
			service, _ := entry.(map[string]any)
			if running, _ := service["running"].(bool); running {
				fmt.Fprintf(c.out, "service %v: running\n", service["name"])
			} else {
				fmt.Fprintf(c.out, "service %v: exited with code %v\n", service["name"], cell(service["exit_code"]))
			}
			if output, _ := service["output"].(string); output != "" {
				fmt.Fprint(c.out, output)
				if !strings.HasSuffix(output, "\n") {
					fmt.Fprintln(c.out)
				}
			}
		}
	}()
	start, ok := obj["start"].(map[string]any)
	if !ok {
		return
	}
	if code, exited := start["exit_code"]; exited {
		fmt.Fprintf(c.out, "start: exited with code %v\n", code)
	} else {
		fmt.Fprintf(c.out, "start: running\n")
	}
	if output, _ := start["output"].(string); output != "" {
		fmt.Fprint(c.out, output)
		if !strings.HasSuffix(output, "\n") {
			fmt.Fprintln(c.out)
		}
	}
}

func (c *cli) operationHelp(op apispec.Operation) {
	line := "kiln " + op.Command
	for _, p := range pathParams(op) {
		line += " <" + p.Name + ">"
	}
	fmt.Fprintf(c.errOut, "usage: %s [flags] [--json]\n\n%s\n\n", line, op.Summary)
	flags := append(append([]apispec.Field{}, op.Body...), op.Query...)
	for _, f := range op.Params {
		if !strings.Contains(op.Path, "{"+f.Name) {
			flags = append(flags, f)
		}
	}
	for _, f := range flags {
		required := ""
		if f.Required {
			required = " (required)"
		}
		fmt.Fprintf(c.errOut, "  --%s %s%s\n      %s\n", flagName(f.Name), f.Type, required, f.Doc)
	}
	if op.ID == "createTemplate" {
		fmt.Fprint(c.errOut, "  --wait\n      Wait for the build to end.\n  --timeout duration\n      Stop waiting after this time. The build continues.\n")
	}
	if len(op.Body) > 0 {
		fmt.Fprint(c.errOut, "  --body json|file|-\n      The whole request body.\n")
	}
}

func printJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// printText writes a list as columns and an object as lines.
func printText(w io.Writer, v any) {
	switch value := v.(type) {
	case []any:
		printRows(w, value)
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(w, "%s: %s\n", key, cell(value[key]))
		}
	default:
		fmt.Fprintln(w, cell(v))
	}
}

// printRows writes the scalar fields of a list of objects as columns.
func printRows(w io.Writer, rows []any) {
	seen := map[string]bool{}
	for _, row := range rows {
		if obj, ok := row.(map[string]any); ok {
			for key, value := range obj {
				switch value.(type) {
				case map[string]any, []any:
				default:
					seen[key] = true
				}
			}
		}
	}
	columns := make([]string, 0, len(seen))
	for key := range seen {
		columns = append(columns, key)
	}
	sort.Slice(columns, func(i, j int) bool {
		a, b := columnRank(columns[i]), columnRank(columns[j])
		if a != b {
			return a < b
		}
		return columns[i] < columns[j]
	})
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(columns, "\t")))
	for _, row := range rows {
		obj, _ := row.(map[string]any)
		cells := make([]string, len(columns))
		for i, column := range columns {
			cells[i] = cell(obj[column])
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
}

// columnRank keeps the identifying columns first.
func columnRank(name string) int {
	switch name {
	case "id", "name":
		return 0
	case "state", "template":
		return 1
	}
	return 2
}

func cell(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case map[string]any, []any:
		b, _ := json.Marshal(value)
		return string(b)
	}
	return fmt.Sprint(v)
}
