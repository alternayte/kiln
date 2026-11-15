package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alternayte/kiln/internal/store"
)

// streamTouchEvery bounds how often traffic on one stream moves the idle
// deadline. Each touch is a store write.
const streamTouchEvery = 10 * time.Second

// ErrNoListener reports that nothing accepts on the guest port of a stream.
var ErrNoListener = errors.New("nothing accepts a connection on that guest port")

// Stream is one raw TCP connection to a guest port. It does not hold the
// sandbox lock: traffic in either direction counts as activity, and a sleep,
// a destroy or a rewind closes it first, because a TCP connection does not
// survive a memory restore.
type Stream struct {
	net.Conn
	m  *Manager
	id string

	lastTouch atomic.Int64
	stopped   atomic.Bool
	closeOnce sync.Once
}

// Stopped reports whether the host closed the stream because the sandbox
// stopped, not because either end closed it.
func (s *Stream) Stopped() bool { return s.stopped.Load() }

func (s *Stream) Read(b []byte) (int, error) {
	n, err := s.Conn.Read(b)
	if n > 0 {
		s.activity()
	}
	return n, err
}

func (s *Stream) Write(b []byte) (int, error) {
	n, err := s.Conn.Write(b)
	if n > 0 {
		s.activity()
	}
	return n, err
}

// Close closes the guest connection and forgets the stream.
func (s *Stream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.m.forgetStream(s)
		err = s.Conn.Close()
	})
	return err
}

func (s *Stream) stop() {
	s.stopped.Store(true)
	_ = s.Close()
}

// activity touches the sandbox at most once per streamTouchEvery.
func (s *Stream) activity() {
	now := s.m.now().UnixNano()
	last := s.lastTouch.Load()
	if now-last < int64(streamTouchEvery) || !s.lastTouch.CompareAndSwap(last, now) {
		return
	}
	ctx := context.Background()
	row, err := s.m.cfg.Store.GetSandbox(ctx, s.id)
	if err != nil {
		log.Printf("sandbox: %s: stream activity: %v", s.id, err)
		return
	}
	if row.State != store.SandboxRunning {
		return
	}
	s.m.touch(ctx, row)
}

// OpenStream dials one guest port of a sandbox. A sleeping sandbox wakes
// first. The stream is registered under the sandbox lock, so a sleep that
// follows always sees it.
func (m *Manager) OpenStream(ctx context.Context, id string, port int) (*Stream, error) {
	if port < 1 || port > 65535 {
		return nil, invalidf("port %d is outside 1..65535", port)
	}
	row, unlock, err := m.acquireRunning(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	m.mu.Lock()
	att := m.attrs[id]
	m.mu.Unlock()
	if att == nil {
		return nil, store.ErrConflict
	}
	conn, err := att.DialGuest(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("%w: port %d of sandbox %s: %v", ErrNoListener, port, id, err)
	}
	s := &Stream{Conn: conn, m: m, id: id}
	s.lastTouch.Store(m.now().UnixNano())
	m.streamMu.Lock()
	if m.streams[id] == nil {
		m.streams[id] = map[*Stream]struct{}{}
	}
	m.streams[id][s] = struct{}{}
	m.streamMu.Unlock()
	m.touch(ctx, row)
	return s, nil
}

func (m *Manager) forgetStream(s *Stream) {
	m.streamMu.Lock()
	defer m.streamMu.Unlock()
	delete(m.streams[s.id], s)
	if len(m.streams[s.id]) == 0 {
		delete(m.streams, s.id)
	}
}

// closeStreams ends every open stream of one sandbox. Every path that stops
// or replaces the VM calls it.
func (m *Manager) closeStreams(id string) {
	m.streamMu.Lock()
	open := make([]*Stream, 0, len(m.streams[id]))
	for s := range m.streams[id] {
		open = append(open, s)
	}
	m.streamMu.Unlock()
	for _, s := range open {
		s.stop()
	}
}
