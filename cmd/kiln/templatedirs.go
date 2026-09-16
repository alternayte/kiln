package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/alternayte/kiln/internal/store"
)

// moveTemplatesIntoTenants puts the files of a template built before tenants
// under the tenant that owns it.
//
// The old layout was templates/<name>, so two tenants with one template name
// shared a rootfs and a memory image. The layout is templates/<tenant>/<name>
// now, and a host that upgrades moves what it already holds.
func moveTemplatesIntoTenants(root string) error {
	dir := filepath.Join(root, "templates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		old := filepath.Join(dir, entry.Name())
		// A tenant directory holds directories; a template directory holds
		// the manifest of one build.
		if _, err := os.Stat(filepath.Join(old, "manifest.json")); err != nil {
			continue
		}
		target := filepath.Join(dir, store.DefaultTenant, entry.Name())
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		if err := os.Rename(old, target); err != nil {
			return fmt.Errorf("move %s: %w", old, err)
		}
		fmt.Printf("kiln serve: template %s moved to the %s tenant\n", entry.Name(), store.DefaultTenant)
	}
	return nil
}
