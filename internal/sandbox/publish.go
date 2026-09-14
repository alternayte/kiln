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
	label := ""
	for _, host := range hosts {
		if stem, ok := previewLabel(host.Subdomain); ok {
			label = stem
			break
		}
	}
	now := m.now()
	for attempt := 0; attempt < 8; attempt++ {
		stem := label
		if stem == "" {
			drawn, err := drawLabel()
			if err != nil {
				return store.Published{}, err
			}
			stem = drawn
		}
		// The first published port of a sandbox takes the bare stem; a later
		// port carries the port number, so every hostname of one sandbox
		// shares its random stem.
		subdomain := stem
		if len(hosts) > 0 {
			subdomain = fmt.Sprintf("%s-%d", stem, port)
		}
		published := store.Published{
			SandboxID:  id,
			GuestPort:  port,
			Subdomain:  subdomain,
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

// drawLabel returns 128 fresh bits as 32 hex characters.
func drawLabel() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// previewLabel returns the random stem of a hostname: 32 hex characters,
// with the port number appended after the first published port.
func previewLabel(subdomain string) (string, bool) {
	if len(subdomain) < 32 {
		return "", false
	}
	stem := subdomain[:32]
	if _, err := hex.DecodeString(stem); err != nil {
		return "", false
	}
	if len(subdomain) == 32 {
		return stem, true
	}
	if subdomain[32] != '-' || len(subdomain) == 33 {
		return "", false
	}
	for i := 33; i < len(subdomain); i++ {
		if subdomain[i] < '0' || subdomain[i] > '9' {
			return "", false
		}
	}
	return stem, true
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
