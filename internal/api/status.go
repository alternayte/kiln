package api

import (
	"html/template"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/kiln/internal/store"
	"golang.org/x/sys/unix"
)

// statusSandbox is one row of the status page.
type statusSandbox struct {
	ID        string
	Template  string
	State     string
	Lifecycle string
	Age       string
	LastSeen  string
}

// statusTemplate is one template row of the status page.
type statusTemplate struct {
	Name      string
	State     string
	Image     string
	VCPUs     int
	MemoryMB  int
	Sandboxes int
}

// statusData is everything the status page prints.
type statusData struct {
	Sandboxes []statusSandbox
	Templates []statusTemplate
	Live      int
	Running   int
	Sleeping  int
	DiskFree  string
	DiskUsed  string
	Load      string
	Now       time.Time
}

// statusPage renders the read-only control page. It has no controls, no forms
// and no JavaScript. The control listener is localhost only.
func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.Sandboxes.List(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	now := time.Now()
	data := statusData{Now: now}
	for _, row := range rows {
		if row.DestroyedAt != nil {
			continue
		}
		data.Live++
		switch row.State {
		case store.SandboxRunning:
			data.Running++
		case store.SandboxSleeping:
			data.Sleeping++
		}
		data.Sandboxes = append(data.Sandboxes, statusSandbox{
			ID:        row.ID,
			Template:  row.TemplateName,
			State:     row.State,
			Lifecycle: row.Lifecycle,
			Age:       humanAge(now.Sub(row.CreatedAt)),
			LastSeen:  humanAge(now.Sub(row.LastActiveAt)),
		})
	}
	templateRows, err := s.Store.ListTemplates(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	for _, row := range templateRows {
		live, _, err := s.Store.TemplateDependents(ctx, row.Name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
			return
		}
		data.Templates = append(data.Templates, statusTemplate{
			Name:      row.Name,
			State:     row.State,
			Image:     row.ImageRef,
			VCPUs:     row.VCPUs,
			MemoryMB:  row.MemoryMB,
			Sandboxes: live,
		})
	}
	data.DiskFree, data.DiskUsed = diskUse(s.Root)
	data.Load = loadAverage()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = statusTemplateHTML.Execute(w, data)
}

// diskUse reports the free and used bytes of the Kiln root filesystem.
func diskUse(root string) (free, used string) {
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return "unknown", "unknown"
	}
	total := st.Blocks * uint64(st.Bsize)
	available := st.Bavail * uint64(st.Bsize)
	usedBytes := total - st.Bfree*uint64(st.Bsize)
	return humanBytes(available), humanBytes(usedBytes)
}

// loadAverage reads the host load from /proc.
func loadAverage() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unknown"
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return "unknown"
	}
	return strings.Join(fields[:3], " ")
}

// humanAge is a short age without a dependency.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatUint(n/div, 10) + " " + string("KMGTPE"[exp]) + "iB"
}

var statusTemplateHTML = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Kiln</title>
<style>
body { font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; margin: 2rem; color: #222; }
h1 { font-size: 1.2rem; }
table { border-collapse: collapse; margin: 1rem 0 2rem; }
th, td { border: 1px solid #ccc; padding: 0.25rem 0.75rem; text-align: left; }
th { background: #f4f4f4; }
td.num { text-align: right; }
</style>
</head>
<body>
<h1>Kiln</h1>
<p>{{.Now.UTC.Format "2006-01-02 15:04:05 UTC"}} — {{.Live}} live, {{.Running}} running, {{.Sleeping}} sleeping. Load {{.Load}}. Disk {{.DiskUsed}} used, {{.DiskFree}} free.</p>
<h2>Sandboxes</h2>
<table>
<tr><th>ID</th><th>Template</th><th>State</th><th>Lifecycle</th><th>Age</th><th>Last active</th></tr>
{{range .Sandboxes}}<tr><td>{{.ID}}</td><td>{{.Template}}</td><td>{{.State}}</td><td>{{.Lifecycle}}</td><td>{{.Age}}</td><td>{{.LastSeen}} ago</td></tr>
{{else}}<tr><td colspan="6">none</td></tr>
{{end}}</table>
<h2>Templates</h2>
<table>
<tr><th>Name</th><th>State</th><th>Image</th><th>vCPUs</th><th>Memory</th><th>Sandboxes</th></tr>
{{range .Templates}}<tr><td>{{.Name}}</td><td>{{.State}}</td><td>{{.Image}}</td><td class="num">{{.VCPUs}}</td><td class="num">{{.MemoryMB}} MiB</td><td class="num">{{.Sandboxes}}</td></tr>
{{else}}<tr><td colspan="6">none</td></tr>
{{end}}</table>
<p>Read-only. The control API is <code>/v1</code> on this listener.</p>
</body>
</html>
`))
