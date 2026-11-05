//go:build linux

package main

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
)

// TestExecStopsWhenTheHostDrops pins that a dropped exec kills its command at
// once. Before, a command that wrote nothing more ran until its timeout, and
// one that wrote more blocked on a pipe nobody read.
func TestExecStopsWhenTheHostDrops(t *testing.T) {
	host, guest := net.Pipe()
	marker := t.TempDir() + "/survived"
	done := make(chan struct{})
	go func() {
		defer close(done)
		runExec(guest, &guestproto.Request{
			Op:             guestproto.OpExec,
			Cmd:            []string{"sh", "-c", "echo started; sleep 5; touch " + marker},
			Cwd:            "/",
			TimeoutSeconds: 60,
		})
		guest.Close()
	}()
	typ, payload, err := guestproto.ReadFrame(host)
	if err != nil || typ != guestproto.FrameStdout || string(payload) != "started\n" {
		t.Fatalf("first frame %q %q %v", typ, payload, err)
	}
	host.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the exec still runs after the host dropped the connection")
	}
	time.Sleep(6 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command outlived the dropped connection")
	}
}
