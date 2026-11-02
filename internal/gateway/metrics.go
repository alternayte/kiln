package gateway

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/alternayte/kiln/internal/apispec"
	"github.com/alternayte/kiln/internal/metrics"
)

// requestKey is one series. The route is a template, because a raw path holds
// sandbox ids.
type requestKey struct {
	route  string
	status int
}

// requestCounts counts requests by route template and status.
type requestCounts struct {
	mu     sync.Mutex
	counts map[requestKey]uint64
}

func (c *requestCounts) add(route string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[requestKey]uint64{}
	}
	c.counts[requestKey{route, status}]++
}

func (c *requestCounts) write(out *bytes.Buffer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]requestKey, 0, len(c.counts))
	for key := range c.counts {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b requestKey) int {
		if a.route != b.route {
			if a.route < b.route {
				return -1
			}
			return 1
		}
		return a.status - b.status
	})
	out.WriteString("# HELP kiln_gateway_requests_total Requests by route template and status.\n# TYPE kiln_gateway_requests_total counter\n")
	for _, key := range keys {
		fmt.Fprintf(out, "kiln_gateway_requests_total{route=%q,status=\"%s\"} %d\n", key.route, strconv.Itoa(key.status), c.counts[key])
	}
}

// measured counts each request under the template of its route. The host routes
// all share one pattern on this mux, so the operation list names them.
func (s *Server) measured(mux *http.ServeMux) http.Handler {
	operations := http.NewServeMux()
	for _, op := range apispec.Operations() {
		operations.HandleFunc(op.Route(), func(http.ResponseWriter, *http.Request) {})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, route := operations.Handler(r)
		if route == "" {
			_, route = mux.Handler(r)
		}
		if route == "" {
			route = "other"
		}
		recorder := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(recorder, r)
		s.requests.add(route, recorder.status)
	})
}

// metricsPage serves the Prometheus text. It takes the token of the host, which
// is the operator token and no tenant holds.
func (s *Server) metricsPage(w http.ResponseWriter, r *http.Request) {
	want := sha256.Sum256([]byte("Bearer " + s.Host.Token))
	got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	if s.Host.Token == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid", "missing or invalid operator token")
		return
	}
	var out bytes.Buffer
	s.requests.write(&out)
	metrics.WriteCounter(&out, "kiln_gateway_host_connection_failures_total", "Calls that did not reach the host.", s.Host.failures.Load())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(out.Bytes())
}

// countFailures counts a call that did not reach the host. A caller that went
// away is not one.
type countFailures struct {
	next     http.RoundTripper
	failures *atomic.Uint64
}

func (t countFailures) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil && r.Context().Err() == nil {
		t.failures.Add(1)
	}
	return resp, err
}
