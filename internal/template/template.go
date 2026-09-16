// Package template imports OCI images into ext4 rootfs images. It applies
// layers in order, honours whiteouts, and injects the guest kilninit.
package template

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/platform"
	"github.com/regclient/regclient/types/ref"

	"github.com/alternayte/kiln/internal/runtime"
)

// Builder pulls OCI images and writes ext4 rootfs images.
type Builder struct {
	// KilninitPath is the linux/amd64 guest init placed at /kilninit.
	KilninitPath string
	// Credentials are the registries this build may authenticate to. They
	// belong to the tenant whose template is building, and to no other, so
	// one tenant's token can never pull for another. An empty list pulls
	// anonymously, which a public image still allows.
	Credentials []RegistryCredential
	// Arch selects the image platform. Defaults to amd64.
	Arch string
	// OS selects the image platform. Defaults to linux.
	OS string
}

// RegistryCredential is one registry this build may authenticate to.
type RegistryCredential struct {
	Host     string
	Username string
	Token    string
}

// ImportSpec is one image import.
type ImportSpec struct {
	Ref        string
	RootfsPath string
	SizeMB     int
}

// Imported reports what was pulled.
type Imported struct {
	// Digest is the resolved image digest (or the platform manifest digest
	// for a multi-platform index).
	Digest string
}

// Import pulls the image, applies its layers to a tree, injects kilninit,
// and writes the ext4 rootfs at spec.RootfsPath.
func (b *Builder) Import(ctx context.Context, spec ImportSpec) (Imported, error) {
	if spec.Ref == "" || spec.RootfsPath == "" {
		return Imported{}, fmt.Errorf("template: ref and rootfs path are required")
	}
	if b.KilninitPath == "" {
		return Imported{}, fmt.Errorf("template: kilninit path is required")
	}
	size := spec.SizeMB
	if size == 0 {
		size = 4096
	}
	tree, err := os.MkdirTemp(filepath.Dir(spec.RootfsPath), ".import-")
	if err != nil {
		return Imported{}, err
	}
	defer os.RemoveAll(tree)

	digest, err := b.pull(ctx, spec.Ref, tree)
	if err != nil {
		return Imported{}, err
	}
	if err := injectKilninit(tree, b.KilninitPath); err != nil {
		return Imported{}, err
	}
	if err := writeResolvConf(tree); err != nil {
		return Imported{}, err
	}
	if err := ensureMountpoints(tree); err != nil {
		return Imported{}, err
	}
	cmd := exec.CommandContext(ctx, "mke2fs",
		"-q", "-F", "-t", "ext4", "-d", tree, "-L", "kiln",
		spec.RootfsPath, fmt.Sprintf("%dM", size))
	if out, err := cmd.CombinedOutput(); err != nil {
		return Imported{}, fmt.Errorf("template: mke2fs: %w: %s", err, out)
	}
	return Imported{Digest: digest}, nil
}

// hosts turns this build's credentials into regclient host settings.
func (b *Builder) hosts() []config.Host {
	out := make([]config.Host, 0, len(b.Credentials))
	for _, cred := range b.Credentials {
		host := config.HostNewName(cred.Host)
		host.User = cred.Username
		host.Pass = cred.Token
		out = append(out, *host)
	}
	return out
}

// pull resolves the platform image and applies its layers to root.
func (b *Builder) pull(ctx context.Context, imageRef, root string) (string, error) {
	r, err := ref.New(imageRef)
	if err != nil {
		return "", fmt.Errorf("template: %s: %w", imageRef, err)
	}
	// The operator's own Docker credentials are deliberately not loaded: a
	// tenant must not borrow them to reach a registry the operator can
	// reach.
	rc := regclient.New(regclient.WithConfigHosts(b.hosts()))
	defer rc.Close(ctx, r)

	m, err := rc.ManifestGet(ctx, r)
	if err != nil {
		return "", fmt.Errorf("template: pull %s: %w", imageRef, err)
	}
	if m.IsList() {
		desc, err := manifest.GetPlatformDesc(m, &platform.Platform{OS: b.os(), Architecture: b.arch()})
		if err != nil {
			return "", fmt.Errorf("template: %s: %w", imageRef, err)
		}
		r = r.SetDigest(desc.Digest.String())
		m, err = rc.ManifestGet(ctx, r)
		if err != nil {
			return "", fmt.Errorf("template: pull %s: %w", imageRef, err)
		}
	}
	img, ok := m.(manifest.Imager)
	if !ok {
		return "", fmt.Errorf("template: %s: manifest has no layers", imageRef)
	}
	layers, err := img.GetLayers()
	if err != nil {
		return "", fmt.Errorf("template: %s: %w", imageRef, err)
	}
	for _, layer := range layers {
		br, err := rc.BlobGet(ctx, r, layer)
		if err != nil {
			return "", fmt.Errorf("template: %s: layer %s: %w", imageRef, layer.Digest, err)
		}
		tr, err := br.ToTarReader()
		if err != nil {
			br.Close()
			return "", fmt.Errorf("template: %s: layer %s: %w", imageRef, layer.Digest, err)
		}
		tarReader, err := tr.GetTarReader()
		if err == nil {
			err = applyLayer(root, tarReader)
		}
		// Close the blob reader, not the tar reader: the tar reader wraps the
		// response body, and only the blob reader releases the registry slot.
		if closeErr := br.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return "", fmt.Errorf("template: %s: layer %s: %w", imageRef, layer.Digest, err)
		}
	}
	return m.GetDescriptor().Digest.String(), nil
}

func (b *Builder) arch() string {
	if b.Arch == "" {
		return "amd64"
	}
	return b.Arch
}

func (b *Builder) os() string {
	if b.OS == "" {
		return "linux"
	}
	return b.OS
}

// injectKilninit places the guest init at /kilninit, mode 0755. It replaces
// any existing entry, including a symlink, so nothing is written through it.
func injectKilninit(root, kilninitPath string) error {
	in, err := os.Open(kilninitPath)
	if err != nil {
		return fmt.Errorf("template: kilninit: %w", err)
	}
	defer in.Close()
	target := filepath.Join(root, "kilninit")
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(target, 0o755)
}

// writeResolvConf points every guest at the host resolver on the gateway.
// Only names in the template's allowlist are answered.
func writeResolvConf(root string) error {
	content := "nameserver " + runtime.GuestGateway + "\n"
	return writeTreeFile(root, "etc/resolv.conf", []byte(content), 0o644)
}

// ensureMountpoints creates the directories kilninit mounts over. A template
// that boots read-only cannot create them itself.
func ensureMountpoints(root string) error {
	dirs := []struct {
		path string
		mode os.FileMode
	}{
		{"proc", 0o555},
		{"sys", 0o555},
		{"dev", 0o755},
		{"dev/pts", 0o755},
		{"tmp", 0o1777},
	}
	for _, d := range dirs {
		target := filepath.Join(root, filepath.FromSlash(d.path))
		if fi, err := os.Lstat(target); err == nil {
			if fi.IsDir() {
				continue
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(target, d.mode); err != nil {
			return err
		}
		if err := os.Chmod(target, d.mode); err != nil {
			return err
		}
	}
	return nil
}

// writeTreeFile writes one file into the rootfs. It resolves the parent
// inside the tree, so a symlinked directory cannot redirect the write out of
// the rootfs, and it replaces any existing entry, including a symlink.
func writeTreeFile(root, rel string, data []byte, mode os.FileMode) error {
	dir, err := resolveInRoot(root, path.Dir(rel), 0)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(dir, path.Base(rel))
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if err := os.WriteFile(target, data, mode); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}
