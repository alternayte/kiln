//go:build !linux

// Non-Linux values keep the package compiling for the laptop checks. Kiln
// runs on Linux, so these are never used.
package network

const (
	soBindToDevice = 0x19
	soMark         = 0x24
)
