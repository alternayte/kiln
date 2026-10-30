//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Offsets number the stream from its start, so a slow reader learns how much
// it lost instead of reading the wrong bytes.
func TestOutputBufferKeepsOffsetsAfterItDropsBytes(t *testing.T) {
	o := &outputBuffer{limit: 4}
	o.Write([]byte("abcdef"))
	data, next, lost, more := o.read(0, 100, false)
	if string(data) != "cdef" || next != 6 || lost != 2 || more {
		t.Fatalf("read = %q next %d lost %d more %t", data, next, lost, more)
	}
	data, next, lost, _ = o.read(6, 100, false)
	if len(data) != 0 || next != 6 || lost != 0 {
		t.Fatalf("read at the end = %q next %d lost %d", data, next, lost)
	}
	data, next, _, more = o.read(2, 1, false)
	if string(data) != "c" || next != 3 || !more {
		t.Fatalf("limited read = %q next %d more %t", data, next, more)
	}
}

// A read must not cut a character, or the next read starts with garbage.
func TestOutputBufferDoesNotCutACharacter(t *testing.T) {
	o := &outputBuffer{limit: 100}
	euro := "\u20ac"
	o.Write([]byte(euro[:2]))
	data, next, _, _ := o.read(0, 100, false)
	if len(data) != 0 || next != 0 {
		t.Fatalf("read = %q next %d, want nothing until the character ends", data, next)
	}
	o.Write([]byte(euro[2:]))
	data, next, _, _ = o.read(0, 100, false)
	if string(data) != euro || next != 3 {
		t.Fatalf("read = %q next %d", data, next)
	}
}

func TestLookupUser(t *testing.T) {
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/sh\nagent:x:1000:1000::/home/agent:/bin/sh\n"), 0o644)
	os.WriteFile(group, []byte("agent:x:1000:\ndocker:x:999:other,agent\nstaff:x:50:other\n"), 0o644)
	for _, spec := range []string{"agent", "1000"} {
		u, err := lookupUser(spec, passwd, group)
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		if u.name != "agent" || u.uid != 1000 || u.gid != 1000 || u.home != "/home/agent" {
			t.Fatalf("%s: %+v", spec, u)
		}
		if len(u.groups) != 2 || u.groups[0] != 1000 || u.groups[1] != 999 {
			t.Fatalf("%s: groups %v", spec, u.groups)
		}
	}
	if _, err := lookupUser("nobody", passwd, group); err == nil || !strings.Contains(err.Error(), "no user") {
		t.Fatalf("a missing user answered %v", err)
	}
}
