//go:build !linux

package snapshot

import (
	"context"
	"errors"
)

// Serve is implemented on Linux only. Kiln runs on Linux; this keeps the
// package compiling for the fast checks on a laptop.
func Serve(ctx context.Context, memPath, socketPath string, ready func()) error {
	return errors.New("snapshot: userfaultfd is Linux only")
}
