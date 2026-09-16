// Package guestbin carries the guest agent inside the kiln binary.
//
// A host used to compile the agent at init time, which meant installing Kiln
// needed the Go toolchain and the source. It also meant a host with neither
// kept whatever agent already sat in its bin directory, so the agent and the
// host that installed it could be different versions. A sandbox then refused
// an operation its host offered, and the failure surfaced far from its cause.
package guestbin

import (
	"embed"
	"errors"
	"io/fs"
	"os"
)

// files holds the linux/amd64 guest init. scripts/build-kilninit.sh writes
// it, and it is not committed: a 4MB binary in every commit that touches the
// guest is a repository nobody wants. bin/.gitkeep keeps this pattern
// satisfied on a fresh checkout, the way web/dist/.gitkeep does for the
// bundle.
//
//go:embed all:bin
var files embed.FS

// ErrMissing reports a kiln binary built without the agent. It names the fix,
// because the person who meets it is holding a binary that cannot install a
// host.
var ErrMissing = errors.New("guestbin: this binary carries no guest agent; run scripts/build-kilninit.sh and build again")

// Agent returns the guest init this binary carries.
func Agent() ([]byte, error) {
	agent, err := fs.ReadFile(files, "bin/kilninit")
	if err != nil {
		return nil, ErrMissing
	}
	return agent, nil
}

// Write places the agent at path, mode 0755. A guest init that is not
// executable boots nothing.
func Write(path string) error {
	agent, err := Agent()
	if err != nil {
		return err
	}
	return os.WriteFile(path, agent, 0o755)
}
