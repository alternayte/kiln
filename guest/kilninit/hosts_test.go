//go:build linux

package main

import (
	"testing"

	"github.com/alternayte/kiln/internal/guestproto"
)

// TestHostsWithNameReplacesTheHostname pins that each resume names the current
// hostname once, so a fork does not keep the name of its source.
func TestHostsWithNameReplacesTheHostname(t *testing.T) {
	built := guestproto.HostsMarker + "\n127.0.0.1\tlocalhost\n::1\tlocalhost\n"
	first, ok := hostsWithName(built, "template")
	if !ok {
		t.Fatal("a marked file was not updated")
	}
	second, _ := hostsWithName(first, "a1b2c3d4e5f60718")
	want := guestproto.HostsMarker + "\n127.0.0.1\tlocalhost\n::1\tlocalhost\n127.0.1.1\ta1b2c3d4e5f60718\n"
	if second != want {
		t.Errorf("got\n%s\nwant\n%s", second, want)
	}
	if _, ok := hostsWithName("127.0.0.1 localhost\n", "x"); ok {
		t.Error("a file the image holds was updated")
	}
}
