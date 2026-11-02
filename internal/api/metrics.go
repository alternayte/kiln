package api

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/alternayte/kiln/internal/metrics"
	"github.com/alternayte/kiln/internal/store"
	"golang.org/x/sys/unix"
)

// metricsStates are the sandbox states the page reports. Each has a line also at
// zero, so a query finds it.
var metricsStates = []string{
	store.SandboxCreating, store.SandboxRunning, store.SandboxSleeping,
	store.SandboxWaking, store.SandboxStopping, store.SandboxFailed,
}

// metricsPage serves the Prometheus text. The auth wrapper holds the operator
// token, because these counts describe every tenant.
func (s *Server) metricsPage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListSandboxes(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	counts := map[string]int{}
	for _, row := range rows {
		if row.DestroyedAt == nil {
			counts[row.State]++
		}
	}
	var out bytes.Buffer
	out.WriteString("# HELP kiln_sandboxes Live sandboxes by state.\n# TYPE kiln_sandboxes gauge\n")
	for _, state := range metricsStates {
		fmt.Fprintf(&out, "kiln_sandboxes{state=%q} %d\n", state, counts[state])
	}
	var st unix.Statfs_t
	if err := unix.Statfs(s.Root, &st); err == nil {
		metrics.WriteGauge(&out, "kiln_disk_available_bytes", "Disk left under the Kiln root.", uint64(st.Bavail)*uint64(st.Bsize))
	}
	if available, ok := memoryAvailable(); ok {
		metrics.WriteGauge(&out, "kiln_memory_available_bytes", "Memory the host can still give out.", available)
	}
	s.Sandboxes.Metrics().Write(&out)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(out.Bytes())
}

// memoryAvailable reads MemAvailable from /proc.
func memoryAvailable() (uint64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			return kb * 1024, err == nil
		}
	}
	return 0, false
}
