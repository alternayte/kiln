package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alternayte/kiln/internal/runtime"
)

// The decision one egress record carries.
const (
	DecisionAllowed  = "allowed"
	DecisionDenied   = "denied"
	DecisionInjected = "injected"
	DecisionObserved = "observed"
)

// guardStart bounds how long a new guard takes to answer on its socket.
const guardStart = 10 * time.Second

// guardAnswer bounds one question to a running guard.
const guardAnswer = 5 * time.Second

// Policy is what one sandbox may reach. With Observe set, any TLS name passes
// and Allow waits for the next change.
type Policy struct {
	Allow   []string
	Observe bool
	// Bindings and Leaves are the credentials of this sandbox and the certificate
	// of each host they name.
	Bindings []Binding
	Leaves   []Leaf
}

// Record is a summary of one destination over some period. A later record for
// the same destination adds to it.
type Record struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Decision string `json:"decision"`
	Count    int64  `json:"count"`
	BytesOut int64  `json:"bytes_out"`
	BytesIn  int64  `json:"bytes_in"`
	First    int64  `json:"first"`
	Last     int64  `json:"last"`
	// Binding names the credential binding of an injected row.
	Binding string `json:"binding,omitempty"`
}

// guardCommand is one line from the daemon to a guard.
type guardCommand struct {
	Seq     int64    `json:"seq,omitempty"`
	Allow   []string `json:"allow,omitempty"`
	Observe bool     `json:"observe,omitempty"`
	Cut     bool     `json:"cut,omitempty"`
	// Bindings and Leaves replace what the guard holds. CutHosts ends each open
	// connection to a host whose binding appeared or went away.
	Bindings []Binding `json:"bindings,omitempty"`
	Leaves   []Leaf    `json:"leaves,omitempty"`
	CutHosts []string  `json:"cut_hosts,omitempty"`
	// Batch acknowledges the records of one report.
	Batch int64 `json:"batch,omitempty"`
	// Stop asks for the last report and an exit.
	Stop bool `json:"stop,omitempty"`
}

// guardReport is one line from a guard to the daemon.
type guardReport struct {
	PID     int      `json:"pid,omitempty"`
	Port    int      `json:"port,omitempty"`
	Seq     int64    `json:"seq,omitempty"`
	Batch   int64    `json:"batch,omitempty"`
	Records []Record `json:"records,omitempty"`
	Dropped int64    `json:"dropped,omitempty"`
	// Requests is the request log of bound hosts since the last report.
	Requests        []RequestRecord `json:"requests,omitempty"`
	RequestsDropped int64           `json:"requests_dropped,omitempty"`
	// Replaced tells a daemon that a newer one took this guard.
	Replaced bool `json:"replaced,omitempty"`
}

// guardLink is the daemon's connection to one guard.
type guardLink struct {
	conn net.Conn
	pid  int
	port int

	writeMu sync.Mutex
	enc     *json.Encoder

	seqMu sync.Mutex
	seq   int64
	waits map[int64]chan struct{}

	// closed ends when the guard is gone. replaced says a newer daemon holds
	// it, so its loss is not a death.
	closed   chan struct{}
	replaced bool
}

func (l *guardLink) send(cmd guardCommand) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	_ = l.conn.SetWriteDeadline(time.Now().Add(guardAnswer))
	return l.enc.Encode(cmd)
}

// apply sends one policy and returns once the guard holds it. A guard that dies
// on the way is no failure: egress fails closed and its successor reads the
// policy from the attachment.
func (l *guardLink) apply(ctx context.Context, policy Policy, cut bool, cutHosts ...string) error {
	l.seqMu.Lock()
	l.seq++
	seq := l.seq
	done := make(chan struct{})
	l.waits[seq] = done
	l.seqMu.Unlock()
	if err := l.send(guardCommand{
		Seq: seq, Allow: policy.Allow, Observe: policy.Observe, Cut: cut,
		Bindings: policy.Bindings, Leaves: policy.Leaves, CutHosts: cutHosts,
	}); err != nil {
		select {
		case <-l.closed:
			return nil
		case <-time.After(guardAnswer):
			return fmt.Errorf("network: egress guard: %w", err)
		}
	}
	select {
	case <-done:
		return nil
	case <-l.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(guardAnswer):
		return errors.New("network: egress guard did not answer")
	}
}

// guardSocket is where one sandbox's guard listens for its daemon.
func (m *Manager) guardSocket(short string) string {
	return GuardSocket(m.root, short)
}

// GuardSocket returns the socket path of the guard with one short id.
func GuardSocket(root, short string) string {
	return filepath.Join(root, "egress", short+".sock")
}

// startGuard runs a new guard and holds its link. The caller holds a.mu or owns
// the attachment alone.
func (a *Attachment) startGuard(ctx context.Context) error {
	if a.m.binary == "" {
		return errors.New("network: no kilnd binary for the egress guard")
	}
	sock := a.m.guardSocket(a.short)
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	_ = os.Remove(sock)
	// The process outlives this call and the daemon: the sandbox owns it, so
	// a restart of the daemon does not cut an open connection.
	cmd := exec.Command(a.m.binary, "egressguard", "--sock", sock, "--tap", a.TAPName, "--mark", strconv.Itoa(a.mark))
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("network: start egress guard: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	deadline := time.Now().Add(guardStart)
	for {
		conn, err := net.DialTimeout("unix", sock, time.Second)
		if err == nil {
			if err := a.linkGuard(ctx, conn); err != nil {
				_ = runtime.KillProcess(cmd.Process.Pid)
				return err
			}
			return nil
		}
		select {
		case <-exited:
			return errors.New("network: the egress guard exited before it listened")
		case <-ctx.Done():
			_ = runtime.KillProcess(cmd.Process.Pid)
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = runtime.KillProcess(cmd.Process.Pid)
			return fmt.Errorf("network: the egress guard did not listen on %s", sock)
		}
	}
}

// linkGuard reads the first report of a guard, starts reading the rest and
// gives it the policy.
func (a *Attachment) linkGuard(ctx context.Context, conn net.Conn) error {
	dec := json.NewDecoder(conn)
	var hello guardReport
	_ = conn.SetReadDeadline(time.Now().Add(guardAnswer))
	if err := dec.Decode(&hello); err != nil {
		conn.Close()
		return fmt.Errorf("network: egress guard: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	link := &guardLink{
		conn: conn, pid: hello.PID, port: hello.Port,
		enc: json.NewEncoder(conn), waits: map[int64]chan struct{}{}, closed: make(chan struct{}),
	}
	a.guard = link
	go a.readGuard(link, dec)
	return link.apply(ctx, a.policy, false)
}

// readGuard takes reports until the link ends.
func (a *Attachment) readGuard(link *guardLink, dec *json.Decoder) {
	for {
		var report guardReport
		if err := dec.Decode(&report); err != nil {
			break
		}
		if report.Replaced {
			link.replaced = true
			break
		}
		if report.Seq > 0 {
			link.seqMu.Lock()
			if done := link.waits[report.Seq]; done != nil {
				close(done)
				delete(link.waits, report.Seq)
			}
			link.seqMu.Unlock()
		}
		if report.Batch > 0 {
			a.m.add(a.ID, report.Records, report.Dropped)
			a.m.addRequests(a.ID, report.Requests, report.RequestsDropped)
			_ = link.send(guardCommand{Batch: report.Batch})
		}
	}
	link.conn.Close()
	close(link.closed)
	if !link.replaced {
		a.guardLost(link)
	}
}

// guardLost starts a new guard after one died. Until the rules name the new
// port, a guest connection to port 443 finds nothing that listens, so egress
// fails closed.
func (a *Attachment) guardLost(link *guardLink) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done || a.guard != link {
		return
	}
	a.guard = nil
	_ = runtime.KillProcess(link.pid)
	// The reconciler removes the link of a sandbox it failed, and its guard with
	// it. That sandbox has nothing left to guard.
	if !a.tapExists() {
		return
	}
	log.Printf("network: %s: the egress guard exited", a.ID)
	if a.m.OnGuardExit != nil {
		a.m.OnGuardExit(a.ID)
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), guardStart+guardAnswer)
		err := a.startGuard(ctx)
		if err == nil {
			if err = a.rewriteDNS(ctx); err != nil {
				// The next attempt starts its own guard.
				a.stopGuard()
			}
		}
		cancel()
		if err == nil {
			return
		}
		log.Printf("network: %s: restart the egress guard: %v", a.ID, err)
		a.mu.Unlock()
		time.Sleep(2 * time.Second)
		a.mu.Lock()
		if a.done || !a.tapExists() {
			return
		}
	}
}

func (a *Attachment) tapExists() bool {
	_, err := os.Stat("/sys/class/net/" + a.TAPName)
	return err == nil
}

// stopGuard asks the guard for its last report, then ends it.
func (a *Attachment) stopGuard() {
	link := a.guard
	a.guard = nil
	if link == nil {
		return
	}
	if err := link.send(guardCommand{Stop: true}); err == nil {
		select {
		case <-link.closed:
		case <-time.After(2 * time.Second):
		}
	}
	link.conn.Close()
	if err := runtime.KillProcess(link.pid); err != nil {
		log.Printf("network: %s: stop the egress guard: %v", a.ID, err)
	}
	_ = os.Remove(a.m.guardSocket(a.short))
}

// usesGuard reports whether a live attachment sends port 443 to a guard. A
// sandbox from before the guard has no such rule and keeps its address set
// until its next wake.
func usesGuard(ctx context.Context, chain string) (bool, error) {
	out, err := run(ctx, "nft", "list", "chain", "inet", Table, chain)
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "tcp dport 443"), nil
}

// reportBatch bounds the destinations the daemon holds for all guards between
// two writes. A record past it counts as dropped.
const reportBatch = 65536

// reportInterval is how often held records reach the store.
const reportInterval = time.Second

// add holds records until the next write, so a hostile guest cannot turn each
// refused packet into a write.
func (m *Manager) add(id string, records []Record, dropped int64) {
	m.reportMu.Lock()
	defer m.reportMu.Unlock()
	held := m.held[id]
	if held == nil {
		held = map[recordKey]*Record{}
		m.held[id] = held
	}
	for _, rec := range records {
		key := recordKey{rec.Host, rec.Port, rec.Decision, rec.Binding}
		_, known := held[key]
		if !merge(held, key, rec, reportBatch-m.heldCount) {
			dropped += max(rec.Count, 1)
			continue
		}
		if !known {
			m.heldCount++
		}
	}
	if dropped > 0 {
		m.heldDropped[id] += dropped
	}
}

// recordKey names one destination.
type recordKey struct {
	host     string
	port     int
	decision string
	binding  string
}

// addRequests holds the request log of one sandbox until the next write. The
// bound on the reports of a guard also bounds what is held here for each.
func (m *Manager) addRequests(id string, rows []RequestRecord, dropped int64) {
	if len(rows) == 0 && dropped == 0 {
		return
	}
	m.reportMu.Lock()
	defer m.reportMu.Unlock()
	if room := reportBatch - len(m.heldRequests[id]); len(rows) > room {
		dropped += int64(len(rows) - room)
		rows = rows[:room]
	}
	m.heldRequests[id] = append(m.heldRequests[id], rows...)
	m.heldRequestsDropped[id] += dropped
}

// merge adds a record to a map that takes room more keys. It reports false when
// the record has no room.
func merge(into map[recordKey]*Record, key recordKey, rec Record, room int) bool {
	cur := into[key]
	if cur == nil {
		if room <= 0 {
			return false
		}
		copied := rec
		into[key] = &copied
		return true
	}
	cur.Count += rec.Count
	cur.BytesOut += rec.BytesOut
	cur.BytesIn += rec.BytesIn
	if rec.Last > cur.Last {
		cur.Last = rec.Last
	}
	if rec.First != 0 && (cur.First == 0 || rec.First < cur.First) {
		cur.First = rec.First
	}
	return true
}

// runReports writes held records at each interval. It runs for the life of the
// daemon.
func (m *Manager) runReports() {
	for range time.Tick(reportInterval) {
		m.Flush()
	}
}

// Flush hands every held record to OnRecords.
func (m *Manager) Flush() {
	m.reportMu.Lock()
	held, dropped := m.held, m.heldDropped
	m.held, m.heldDropped = map[string]map[recordKey]*Record{}, map[string]int64{}
	m.heldCount = 0
	requests, requestsDropped := m.heldRequests, m.heldRequestsDropped
	m.heldRequests, m.heldRequestsDropped = map[string][]RequestRecord{}, map[string]int64{}
	m.reportMu.Unlock()
	if m.OnRequests != nil {
		for id, rows := range requests {
			m.OnRequests(id, rows, requestsDropped[id])
			delete(requestsDropped, id)
		}
		for id, n := range requestsDropped {
			m.OnRequests(id, nil, n)
		}
	}
	if m.OnRecords == nil {
		return
	}
	for id, records := range held {
		rows := make([]Record, 0, len(records))
		for _, rec := range records {
			rows = append(rows, *rec)
		}
		m.OnRecords(id, rows, dropped[id])
		delete(dropped, id)
	}
	for id, n := range dropped {
		m.OnRecords(id, nil, n)
	}
}

// refusalPrefix starts the kernel log line of a connection the forward chain
// refused. The short id follows it.
const refusalPrefix = "kiln_egress_"

// watchRefusals reads the refused connections the forward chains log. Only
// connections by address reach this log: a name goes through the resolver or
// the guard.
func (m *Manager) watchRefusals() {
	kmsg, err := os.Open("/dev/kmsg")
	if err != nil {
		log.Printf("network: refused addresses are not recorded: %v", err)
		return
	}
	defer kmsg.Close()
	// The log before this daemon started belongs to the daemon before it.
	if _, err := kmsg.Seek(0, 2); err != nil {
		log.Printf("network: refused addresses are not recorded: %v", err)
		return
	}
	buf := make([]byte, 8192)
	for {
		// One read returns one record. EPIPE means the ring moved past this
		// reader, which then continues at the oldest record.
		n, err := kmsg.Read(buf)
		if err != nil {
			if errors.Is(err, errRingOverrun) {
				continue
			}
			log.Printf("network: refused addresses: %v", err)
			return
		}
		short, host, port, ok := parseRefusal(string(buf[:n]))
		if !ok {
			continue
		}
		m.reportMu.Lock()
		id := m.shorts[short]
		m.reportMu.Unlock()
		if id == "" {
			continue
		}
		now := time.Now().Unix()
		m.add(id, []Record{{Host: host, Port: port, Decision: DecisionDenied, Count: 1, First: now, Last: now}}, 0)
	}
}

// parseRefusal reads one kernel log record of the form
// "6,1,2,-;kiln_egress_abc IN=kiln-abc OUT=eth0 DST=1.2.3.4 PROTO=TCP DPT=80".
func parseRefusal(line string) (short, host string, port int, ok bool) {
	_, text, found := strings.Cut(line, ";")
	if !found || !strings.HasPrefix(text, refusalPrefix) {
		return "", "", 0, false
	}
	fields := strings.Fields(strings.TrimPrefix(text, refusalPrefix))
	if len(fields) == 0 {
		return "", "", 0, false
	}
	short = fields[0]
	for _, field := range fields[1:] {
		if v, is := strings.CutPrefix(field, "DST="); is {
			host = v
		}
		if v, is := strings.CutPrefix(field, "DPT="); is {
			port, _ = strconv.Atoi(v)
		}
	}
	return short, host, port, host != ""
}
