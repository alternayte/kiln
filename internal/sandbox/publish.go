package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/alternayte/kiln/internal/store"
)

// Publish serves one guest port on a hostname. Republishing the same port
// returns the same hostname. Publishing does not wake a sleeping sandbox;
// the ingress wakes it on the first request.
func (m *Manager) Publish(ctx context.Context, id string, port int, visibility string) (store.Published, error) {
	if port < 1 || port > 65535 {
		return store.Published{}, invalidf("port must be between 1 and 65535")
	}
	if visibility != store.VisibilityPublic && visibility != store.VisibilityTeam {
		return store.Published{}, invalidf("visibility must be %q or %q", store.VisibilityPublic, store.VisibilityTeam)
	}
	if m.cfg.Zone == "" {
		return store.Published{}, invalidf("no preview zone is configured; run kiln init --zone")
	}
	unlock := m.lockOps(id)
	defer unlock()
	row, err := m.cfg.Store.GetSandbox(ctx, id)
	if err != nil {
		return store.Published{}, err
	}
	switch {
	case row.DestroyedAt != nil || row.State == store.SandboxDestroyed:
		return store.Published{}, store.ErrNotFound
	case row.State == store.SandboxFailed:
		return store.Published{}, store.ErrConflict
	}
	existing, err := m.cfg.Store.GetPublished(ctx, id, port)
	switch {
	case err == nil:
		if existing.Visibility != visibility {
			return store.Published{}, store.ErrConflict
		}
		return existing, nil
	case !errors.Is(err, store.ErrNotFound):
		return store.Published{}, err
	}
	hosts, err := m.cfg.Store.ListPublished(ctx, id)
	if err != nil {
		return store.Published{}, err
	}
	now := m.now()
	for attempt := 0; attempt < 8; attempt++ {
		label, err := newSubdomain(len(hosts) == 0, port)
		if err != nil {
			return store.Published{}, err
		}
		published := store.Published{
			SandboxID:  id,
			GuestPort:  port,
			Subdomain:  label,
			Visibility: visibility,
			CreatedAt:  now,
		}
		if err := m.cfg.Store.PublishSandbox(ctx, published); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return store.Published{}, err
		}
		m.touch(ctx, row)
		return published, nil
	}
	return store.Published{}, fmt.Errorf("sandbox: no preview hostname was free after 8 draws")
}

// newSubdomain draws 128 bits from the CSPRNG. The first hostname of a
// sandbox is <random>; a later port carries the port number. The value is
// never derived from the sandbox id, a branch name or a commit.
func newSubdomain(first bool, port int) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	label := hex.EncodeToString(b)
	if !first {
		label = fmt.Sprintf("%s-%d", label, port)
	}
	return label, nil
}

// Published returns every hostname of one sandbox, oldest first.
func (m *Manager) Published(ctx context.Context, id string) ([]store.Published, error) {
	return m.cfg.Store.ListPublished(ctx, id)
}

// Retire removes one published hostname. It 404s from then on and is never
// handed out again.
func (m *Manager) Retire(ctx context.Context, id string, port int) error {
	unlock := m.lockOps(id)
	defer unlock()
	return m.cfg.Store.RetirePublished(ctx, id, port)
}

// PublishedURL is the public URL of one published port.
func (m *Manager) PublishedURL(row store.Published) string {
	return "https://" + row.Subdomain + "." + m.cfg.Zone + "/"
}
