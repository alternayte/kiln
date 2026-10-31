package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/kiln/internal/apispec"
)

var (
	envKey     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	nameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// previewOptions are the inputs of kiln preview up. They are the inputs of
// the preview Action, which calls this command.
type previewOptions struct {
	name, image, start, registryUsername, envFile, visibility string
	port, vcpus, memoryMB, diskMB, ttlSeconds, idleSeconds    int
	publish                                                   []int
	egress, setup                                             []string
	metadata, group                                           map[string]string
	timeout                                                   time.Duration
}

func (c *cli) cmdPreview(args []string) error {
	if len(args) == 0 {
		return usagef("kiln preview up --image X --port N", "kiln preview needs up or down")
	}
	switch args[0] {
	case "up":
		return c.previewUp(args[1:])
	case "down":
		return c.previewDown(args[1:])
	}
	return usagef("kiln preview up --image X --port N", "unknown command %q", "preview "+args[0])
}

const previewUsage = "kiln preview up --image X --port N [--start CMD] [--name NAME] [--publish PORT] [--env-file FILE|-] [--egress-allow HOST] [--setup CMD] [--vcpus N] [--memory-mb N] [--disk-mb N] [--ttl-seconds N] [--idle-seconds N] [--visibility public|team] [--metadata K=V] [--group K=V] [--registry-username NAME] [--timeout D]"

// parsePreview reads the flags of preview up and preview down.
func parsePreview(args []string) (*previewOptions, error) {
	o := &previewOptions{
		vcpus: 1, memoryMB: 1024, diskMB: 4096, ttlSeconds: 604800, idleSeconds: 900,
		visibility: "public", timeout: 30 * time.Minute,
		metadata: map[string]string{}, group: map[string]string{},
	}
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		if !strings.HasPrefix(args[i], "--") {
			return nil, usagef(previewUsage, "unexpected argument %q", args[i])
		}
		if name == "json" {
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return nil, usagef(previewUsage, "--%s needs a value", name)
			}
			i++
			value = args[i]
		}
		number := func(target *int) error {
			n, err := strconv.Atoi(value)
			if err != nil {
				return usagef(previewUsage, "--%s: %v", name, err)
			}
			*target = n
			return nil
		}
		pair := func(target map[string]string) error {
			key, val, ok := strings.Cut(value, "=")
			if !ok {
				return usagef(previewUsage, "--%s wants KEY=value", name)
			}
			target[key] = val
			return nil
		}
		var err error
		switch name {
		case "name":
			o.name = value
		case "image":
			o.image = value
		case "start":
			o.start = value
		case "env-file":
			o.envFile = value
		case "registry-username":
			o.registryUsername = value
		case "visibility":
			o.visibility = value
		case "egress-allow":
			o.egress = append(o.egress, strings.Fields(value)...)
		case "setup":
			o.setup = append(o.setup, value)
		case "publish":
			for _, word := range strings.Fields(value) {
				port, perr := strconv.Atoi(word)
				if perr != nil || port < 1 || port > 65535 {
					return nil, usagef(previewUsage, "publish entry %q is not a port from 1 to 65535", word)
				}
				o.publish = append(o.publish, port)
			}
		case "port":
			err = number(&o.port)
		case "vcpus":
			err = number(&o.vcpus)
		case "memory-mb":
			err = number(&o.memoryMB)
		case "disk-mb":
			err = number(&o.diskMB)
		case "ttl-seconds":
			err = number(&o.ttlSeconds)
		case "idle-seconds":
			err = number(&o.idleSeconds)
		case "metadata":
			err = pair(o.metadata)
		case "group":
			err = pair(o.group)
		case "timeout":
			d, derr := time.ParseDuration(value)
			if derr != nil {
				return nil, usagef(previewUsage, "--timeout: %v", derr)
			}
			o.timeout = d
		default:
			return nil, usagef(previewUsage, "unknown flag --%s", name)
		}
		if err != nil {
			return nil, err
		}
	}
	// A group names the previews that replace each other. The directory is the
	// default, so an agent runs up and down in a repository with no state.
	if len(o.group) == 0 {
		dir, err := os.Getwd()
		if err != nil {
			return nil, usagef("--group KEY=value", "%v", err)
		}
		o.group["preview"] = filepath.Base(dir)
	}
	return o, nil
}

func (c *cli) previewUp(args []string) error {
	o, err := parsePreview(args)
	if err != nil {
		return err
	}
	if o.image == "" || o.port == 0 {
		return usagef(previewUsage, "--image and --port are required")
	}
	for _, port := range o.publish {
		if port == o.port {
			return usagef(previewUsage, "publish entry %d is the same as port", port)
		}
	}
	var start []string
	if o.start != "" {
		if start, err = splitWords(o.start); err != nil {
			return usagef(previewUsage, "--start: %v", err)
		}
	}
	env, err := c.readEnvFile(o.envFile)
	if err != nil {
		return err
	}
	if o.name == "" {
		o.name = previewName(o.group)
	}
	if _, _, err := connection(); err != nil {
		return err
	}

	template := map[string]any{
		"name": o.name, "image": o.image, "vcpus": o.vcpus, "memory_mb": o.memoryMB, "disk_mb": o.diskMB,
		"ttl_seconds": o.ttlSeconds, "egress_allow": nonNil(o.egress), "setup": nonNil(o.setup), "port": o.port,
	}
	if len(start) > 0 {
		template["start"] = start
	}
	// The token comes from the environment, so it reaches no command line.
	if token := os.Getenv("KILN_REGISTRY_TOKEN"); token != "" {
		template["credential"] = map[string]string{"username": o.registryUsername, "token": token}
	}
	fmt.Fprintf(c.errOut, "building template %s from %s\n", o.name, o.image)
	if _, err := c.call(c.ctx, "POST", "/v1/templates", template, nil); err != nil {
		return err
	}
	if _, err := c.waitTemplate(o.name, o.timeout); err != nil {
		c.printEvents(o.name, "")
		c.discardTemplate(o.name)
		return err
	}

	metadata := map[string]any{}
	for key, value := range o.group {
		metadata[key] = value
	}
	for key, value := range o.metadata {
		metadata[key] = value
	}
	request := map[string]any{"template": o.name, "lifecycle": "persistent", "idle_seconds": o.idleSeconds, "metadata": metadata}
	if len(env) > 0 {
		request["env"] = env
	}
	var sandbox struct {
		ID string `json:"id"`
	}
	if _, err := c.call(c.ctx, "POST", "/v1/sandboxes", request, &sandbox); err != nil {
		c.discardTemplate(o.name)
		return err
	}

	// port goes first: the first published port gets the short hostname, and
	// every other port shares its stem.
	urls := map[string]string{}
	first := ""
	for _, port := range append([]int{o.port}, o.publish...) {
		var published struct {
			URL string `json:"url"`
		}
		path := "/v1/sandboxes/" + url.PathEscape(sandbox.ID) + "/publish"
		if _, err := c.call(c.ctx, "POST", path, map[string]any{"port": port, "visibility": o.visibility}, &published); err != nil {
			c.discardSandbox(sandbox.ID, o.name)
			return err
		}
		urls[strconv.Itoa(port)] = published.URL
		if first == "" {
			first = published.URL
		}
	}
	c.printEvents(o.name, sandbox.ID)

	// The previous preview goes last, so no caller is left with a hostname
	// that has already been retired.
	if _, err := c.pruneGroup(o.group, o.name); err != nil {
		fmt.Fprintf(c.errOut, "kiln: the previous preview was not removed: %v\n", err)
	}

	next := "kiln logs " + sandbox.ID
	if c.json {
		printJSON(c.out, map[string]any{"url": first, "urls": urls, "sandbox_id": sandbox.ID, "template": o.name, "next": next})
		return nil
	}
	fmt.Fprintln(c.out, first)
	for _, port := range o.publish {
		fmt.Fprintf(c.errOut, "port %d: %s\n", port, urls[strconv.Itoa(port)])
	}
	fmt.Fprintf(c.errOut, "sandbox: %s\nnext: %s\n", sandbox.ID, next)
	return nil
}

func (c *cli) previewDown(args []string) error {
	o, err := parsePreview(args)
	if err != nil {
		return err
	}
	removed, err := c.pruneGroup(o.group, "")
	if err != nil {
		return err
	}
	if c.json {
		printJSON(c.out, map[string]any{"destroyed": removed})
		return nil
	}
	fmt.Fprintf(c.out, "destroyed %d preview(s)\n", len(removed))
	return nil
}

// pruneGroup destroys the previews of one group except the one from template
// keep, and deletes the templates they came from.
func (c *cli) pruneGroup(group map[string]string, keep string) ([]string, error) {
	var rows []struct {
		ID          string         `json:"id"`
		Template    string         `json:"template"`
		Metadata    map[string]any `json:"metadata"`
		DestroyedAt string         `json:"destroyed_at"`
	}
	if _, err := c.call(c.ctx, "GET", "/v1/sandboxes", nil, &rows); err != nil {
		return nil, err
	}
	removed := []string{}
	for _, row := range rows {
		if row.DestroyedAt != "" || (keep != "" && row.Template == keep) || !inGroup(row.Metadata, group) {
			continue
		}
		fmt.Fprintf(c.errOut, "destroying preview %s\n", row.ID)
		if _, err := c.call(c.ctx, "DELETE", "/v1/sandboxes/"+url.PathEscape(row.ID), nil, nil); err != nil && !isNotFound(err) {
			return removed, err
		}
		removed = append(removed, row.ID)
		if row.Template == "" {
			continue
		}
		// The template holds the snapshot, so it goes once its sandbox has.
		if _, err := c.call(c.ctx, "DELETE", "/v1/templates/"+url.PathEscape(row.Template), nil, nil); err != nil && !isNotFound(err) {
			return removed, err
		}
	}
	return removed, nil
}

func inGroup(metadata map[string]any, group map[string]string) bool {
	for key, want := range group {
		if got, ok := metadata[key]; !ok || fmt.Sprint(got) != want {
			return false
		}
	}
	return true
}

func isNotFound(err error) bool {
	f, ok := err.(*failure)
	return ok && f.exit == apispec.ExitNotFound
}

// discardTemplate removes the template of a failed run. A template with no
// sandbox is invisible to a prune, so otherwise it holds its disk until its TTL.
func (c *cli) discardTemplate(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.call(ctx, "DELETE", "/v1/templates/"+url.PathEscape(name), nil, nil); err != nil && !isNotFound(err) {
		fmt.Fprintf(c.errOut, "kiln: template %s was not removed: %v\n", name, err)
	}
}

func (c *cli) discardSandbox(id, template string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.call(ctx, "DELETE", "/v1/sandboxes/"+url.PathEscape(id), nil, nil); err != nil && !isNotFound(err) {
		fmt.Fprintf(c.errOut, "kiln: sandbox %s was not removed: %v\n", id, err)
	}
	c.discardTemplate(template)
}

// printEvents reports where the time went, one line per phase, oldest first.
// The stream never ends, so a time limit closes it, and a preview that is
// already live does not fail over its own timing.
func (c *cli) printEvents(template, sandboxID string) {
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	resp, err := c.request(ctx, "GET", "/v1/events?after=0", "text/event-stream", nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	c.printPhases(resp.Body, template, sandboxID)
}

func (c *cli) printPhases(r io.Reader, template, sandboxID string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	total := 0
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue
		}
		var event struct {
			Phase      string `json:"phase"`
			Template   string `json:"template"`
			SandboxID  string `json:"sandbox_id"`
			DurationMS int    `json:"duration_ms"`
			Reason     string `json:"reason"`
		}
		if json.Unmarshal([]byte(data), &event) != nil || event.Phase == "" {
			continue
		}
		if event.Template != template && (sandboxID == "" || event.SandboxID != sandboxID) {
			continue
		}
		total += event.DurationMS
		note := ""
		if event.Reason != "" {
			note = " (" + event.Reason + ")"
		}
		fmt.Fprintf(c.errOut, "phase %s: %d ms%s\n", event.Phase, event.DurationMS, note)
	}
	fmt.Fprintf(c.errOut, "phases total: %d ms\n", total)
}

// readEnvFile reads one KEY=value per line, split on the first =. Values stay
// off the command line, so - reads stdin. No error names a value.
func (c *cli) readEnvFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	var source io.Reader = c.in
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, usagef(previewUsage, "--env-file: %v", err)
		}
		defer f.Close()
		source = f
	}
	pairs := map[string]string{}
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for number := 1; scanner.Scan(); number++ {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, usagef(previewUsage, "env line %d has no '='", number)
		}
		key = strings.TrimSpace(key)
		if !envKey.MatchString(key) {
			return nil, usagef(previewUsage, "env line %d: the key must match %s", number, envKey)
		}
		if _, dup := pairs[key]; dup {
			return nil, usagef(previewUsage, "env line %d: key %s appears twice", number, key)
		}
		pairs[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, usagef(previewUsage, "--env-file: %v", err)
	}
	return pairs, nil
}

// previewName is a fresh template name, so a second up in one group does not
// meet the template of the first.
func previewName(group map[string]string) string {
	stem := "preview"
	if value := group["preview"]; value != "" {
		stem = nameUnsafe.ReplaceAllString(value, "-")
		stem = strings.TrimLeft(stem, "-._")
	}
	if stem == "" {
		stem = "preview"
	}
	if len(stem) > 50 {
		stem = stem[:50]
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return stem + "-" + hex.EncodeToString(suffix)
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// splitWords splits one command line as a POSIX shell does: quotes and a
// backslash group words, and nothing expands.
func splitWords(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range line {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == '\\':
			escaped = true
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("the closing quote is missing")
	}
	if escaped {
		return nil, fmt.Errorf("the line ends with a backslash")
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}
