package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alternayte/kiln/internal/store"
)

// moveTemplatesIntoTenants puts the files of every template under the tenant
// that owns it.
//
// The old layout was templates/<name>, so two tenants with one template name
// shared a rootfs and a memory image. The layout is templates/<tenant>/<name>
// now. The move follows the rows, because a template of one tenant may sit in
// the flat path, or in the default tenant's directory after an earlier move.
func moveTemplatesIntoTenants(ctx context.Context, st store.Store, root string) error {
	rows, err := st.ListTemplates(ctx)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, "templates")
	for _, row := range rows {
		tenant := row.TenantID
		if tenant == "" {
			tenant = store.DefaultTenant
		}
		want := filepath.Join(dir, tenant, row.Name)
		if _, err := os.Stat(filepath.Join(want, "manifest.json")); err == nil {
			continue
		}
		for _, old := range []string{
			filepath.Join(dir, row.Name),
			filepath.Join(dir, store.DefaultTenant, row.Name),
		} {
			if old == want {
				continue
			}
			if _, err := os.Stat(filepath.Join(old, "manifest.json")); err != nil {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(want), 0o750); err != nil {
				return err
			}
			if err := os.Rename(old, want); err != nil {
				return fmt.Errorf("move %s: %w", old, err)
			}
			fmt.Printf("kiln serve: template %s moved to tenant %s\n", row.Name, tenant)
			break
		}
	}
	return nil
}
