package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alternayte/kiln/internal/apispec"
)

//go:embed skill/SKILL.md
var skillTemplate string

const (
	sectionBegin = "<!-- kiln:begin -->"
	sectionEnd   = "<!-- kiln:end -->"
	section      = sectionBegin + `
## Kiln

This repository runs on Kiln sandboxes. Read ` + "`.claude/skills/kiln/SKILL.md`" + ` before you use them.

- Preview a change: ` + "`kiln preview up --image <image> --port <port>`" + `
- Read a failure: ` + "`kiln logs <id>`" + `
- End it: ` + "`kiln preview down`" + `
- Commands read ` + "`KILN_URL`" + ` and ` + "`KILN_API_KEY`" + ` from the environment. Never write the key to a file.
` + sectionEnd + "\n"
)

// skillStamp ends the skill file. It holds a hash of the text above it, so a
// later run can tell an unchanged older skill from one a person edited.
var skillStamp = regexp.MustCompile(`(?m)^<!-- kiln skill sha256:([0-9a-f]{64}) -->\n?\z`)

// skillText is the skill of this binary, so the skill matches the commands.
func skillText() string {
	body := strings.ReplaceAll(skillTemplate, "{{EXIT_CODES}}", apispec.ExitCodeTable())
	body = strings.ReplaceAll(body, "{{VERSION}}", apispec.Version)
	return body + stampFor(body)
}

func stampFor(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "<!-- kiln skill sha256:" + hex.EncodeToString(sum[:]) + " -->\n"
}

// skillUnchanged says whether a skill file is one this tool wrote.
func skillUnchanged(text string) bool {
	match := skillStamp.FindStringSubmatchIndex(text)
	if match == nil {
		return false
	}
	sum := sha256.Sum256([]byte(text[:match[0]]))
	return hex.EncodeToString(sum[:]) == text[match[2]:match[3]]
}

// cmdInit gives a repository the Kiln skill, an AGENTS.md section and an MCP
// entry. It writes no credential: the MCP entry names environment variables.
func (c *cli) cmdInit(args []string) error {
	dir, force := ".", false
	for _, arg := range args {
		switch {
		case arg == "--force":
			force = true
		case arg == "--json":
		case strings.HasPrefix(arg, "-"):
			return usagef("kiln init [dir] [--force]", "unknown flag %s", arg)
		default:
			dir = arg
		}
	}
	wrote := map[string]string{}
	steps := []struct {
		path string
		do   func(string, bool) (string, error)
	}{
		{filepath.Join(dir, ".claude", "skills", "kiln", "SKILL.md"), initSkill},
		{filepath.Join(dir, "AGENTS.md"), initAgents},
		{filepath.Join(dir, ".mcp.json"), initMCP},
	}
	for _, step := range steps {
		result, err := step.do(step.path, force)
		if err != nil {
			return &failure{exit: apispec.ExitFailure, code: "failed", message: fmt.Sprintf("%s: %v", step.path, err)}
		}
		wrote[step.path] = result
	}
	if c.json {
		printJSON(c.out, map[string]any{"files": wrote, "next": "kiln preview up --image <image> --port <port>"})
		return nil
	}
	for _, step := range steps {
		fmt.Fprintf(c.out, "%s: %s\n", step.path, wrote[step.path])
	}
	fmt.Fprintln(c.errOut, "next: kiln preview up --image <image> --port <port>")
	return nil
}

func initSkill(path string, force bool) (string, error) {
	want := skillText()
	have, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	switch {
	case err != nil:
		return "wrote", writeFile(path, want)
	case string(have) == want:
		return "unchanged", nil
	case !force && !skillUnchanged(string(have)):
		return "kept, it has local changes; --force replaces it", nil
	}
	return "updated", writeFile(path, want)
}

// initAgents writes the marked section and keeps every other line.
func initAgents(path string, _ bool) (string, error) {
	have, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	text := string(have)
	begin := strings.Index(text, sectionBegin)
	end := strings.Index(text, sectionEnd)
	var next string
	switch {
	case begin >= 0 && end > begin:
		tail := strings.TrimPrefix(text[end+len(sectionEnd):], "\n")
		next = text[:begin] + section + tail
	case begin >= 0 || end >= 0:
		return "", fmt.Errorf("it holds only one of the two Kiln markers; remove the marker and run again")
	case text == "":
		next = section
	default:
		next = strings.TrimRight(text, "\n") + "\n\n" + section
	}
	if next == text {
		return "unchanged", nil
	}
	return "wrote the Kiln section", writeFile(path, next)
}

// initMCP adds the kiln server and keeps every other key. A changed kiln entry
// stays unless --force is set.
func initMCP(path string, force bool) (string, error) {
	doc := map[string]any{}
	have, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(have, &doc); err != nil {
			return "", fmt.Errorf("it is not JSON, so it stays as it is: %v", err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	servers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		if _, present := doc["mcpServers"]; present {
			return "", errors.New("its mcpServers is not an object, so it stays as it is")
		}
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	want := map[string]any{
		"type":    "http",
		"url":     "${KILN_URL}/mcp",
		"headers": map[string]any{"Authorization": "Bearer ${KILN_API_KEY}"},
	}
	wantJSON, _ := json.Marshal(want)
	if current, exists := servers["kiln"]; exists {
		currentJSON, _ := json.Marshal(current)
		if string(currentJSON) == string(wantJSON) {
			return "unchanged", nil
		}
		if !force {
			return "kept, its kiln entry has local changes; --force replaces it", nil
		}
	}
	servers["kiln"] = want
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return "wrote the kiln entry", writeFile(path, string(out)+"\n")
}

// writeFile replaces a file in one step, so a failure leaves the old one.
func writeFile(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kiln-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
