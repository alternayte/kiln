// Package metrics writes the Prometheus text format of the host and the
// gateway. No value carries a sandbox, tenant or template, so the series count
// is fixed by the code.
package metrics

import (
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// buckets is the one list every duration uses, in seconds, so a value compares
// across versions.
var buckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 60, 120, 300}

// Histogram counts durations into buckets.
type Histogram struct {
	mu     sync.Mutex
	counts []uint64
	count  uint64
	sum    float64
}

// Observe adds one duration.
func (h *Histogram) Observe(d time.Duration) {
	seconds := d.Seconds()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.counts == nil {
		h.counts = make([]uint64, len(buckets))
	}
	for i, limit := range buckets {
		if seconds <= limit {
			h.counts[i]++
		}
	}
	h.count++
	h.sum += seconds
}

// Write prints the histogram with its help line.
func (h *Histogram) Write(w io.Writer, name, help string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for i, limit := range buckets {
		var n uint64
		if h.counts != nil {
			n = h.counts[i]
		}
		fmt.Fprintf(w, "%s_bucket{le=\"%s\"} %d\n", name, strconv.FormatFloat(limit, 'f', -1, 64), n)
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, h.count)
	fmt.Fprintf(w, "%s_sum %s\n", name, strconv.FormatFloat(h.sum, 'f', -1, 64))
	fmt.Fprintf(w, "%s_count %d\n", name, h.count)
}

// WriteCounter prints one counter with no labels.
func WriteCounter(w io.Writer, name, help string, value uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, value)
}

// WriteGauge prints one gauge with no labels.
func WriteGauge(w io.Writer, name, help string, value uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, value)
}

// Host holds what the host counts between two scrapes. The sandbox states and
// the free space come from the store and the filesystem at each scrape.
type Host struct {
	Restore  Histogram
	Fork     Histogram
	Snapshot Histogram

	Sleeps          atomic.Uint64
	Wakes           atomic.Uint64
	BuildFailures   atomic.Uint64
	GuardRestarts   atomic.Uint64
	EgressDropped   atomic.Uint64
	RequestsDropped atomic.Uint64
}

// Write prints every value the host counts.
func (h *Host) Write(w io.Writer) {
	h.Restore.Write(w, "kiln_restore_duration_seconds", "Time to restore one sandbox from a snapshot.")
	h.Fork.Write(w, "kiln_fork_duration_seconds", "Time of one fork call, with its snapshot of the origin.")
	h.Snapshot.Write(w, "kiln_snapshot_duration_seconds", "Time of one snapshot call.")
	WriteCounter(w, "kiln_sandbox_sleeps_total", "Sandboxes put to sleep.", h.Sleeps.Load())
	WriteCounter(w, "kiln_sandbox_wakes_total", "Sandboxes woken.", h.Wakes.Load())
	WriteCounter(w, "kiln_template_build_failures_total", "Template builds that failed.", h.BuildFailures.Load())
	WriteCounter(w, "kiln_egress_guard_restarts_total", "Egress guards that exited and need a new process.", h.GuardRestarts.Load())
	WriteCounter(w, "kiln_egress_records_dropped_total", "Egress records with no room, so the record has a gap.", h.EgressDropped.Load())
	WriteCounter(w, "kiln_egress_requests_dropped_total", "Request log rows with no room, so the log has a gap.", h.RequestsDropped.Load())
}
