// Package template imports OCI images into ext4 rootfs images. It applies
// layers in order, honours whiteouts, and injects the guest kilninit.
package template

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/platform"
	"github.com/regclient/regclient/types/ref"

	"github.com/alternayte/kiln/internal/guestproto"
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
	// LayerCacheDir keeps the layers this tenant has pulled. Empty keeps them
	// only for this import.
	LayerCacheDir string
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
	// Services are the images that unpack beside the application, each into
	// its own root.
	Services []ServiceRequest
	// Phase hears how long each step took, how it ended, and anything worth
	// saying about it. It may be nil.
	Phase func(name string, began time.Time, err error, note string)
}

// Imported reports what was pulled.
type Imported struct {
	// Digest is the resolved image digest (or the platform manifest digest
	// for a multi-platform index).
	Digest string
	// Config is what the application image says about how it runs.
	Config guestproto.ImageConfig
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

	report := func(name string, began time.Time, err error, note string) {
		if spec.Phase != nil {
			spec.Phase(name, began, err, note)
		}
	}
	cacheDir := b.LayerCacheDir
	if cacheDir == "" {
		// One path for every build: a layer is always a file first.
		cacheDir = tree + ".layers"
		defer os.RemoveAll(cacheDir)
	}
	cache, err := newLayerCache(cacheDir, layerCacheLimit)
	if err != nil {
		return Imported{}, err
	}
	began := time.Now()
	digest, config, note, err := b.pull(ctx, spec.Ref, tree, cache)
	report("layers", began, err, note)
	if err != nil {
		return Imported{}, err
	}
	if err := writeImageConfig(tree, guestproto.AppImageConfig, config); err != nil {
		return Imported{}, err
	}
	for _, service := range spec.Services {
		began = time.Now()
		root := filepath.Join(tree, filepath.FromSlash(guestproto.ServiceRoots), service.Name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			return Imported{}, err
		}
		_, serviceConfig, note, err := b.pull(ctx, service.Image, root, cache)
		report("layers_"+service.Name, began, err, note)
		if err != nil {
			return Imported{}, fmt.Errorf("service %s: %w", service.Name, err)
		}
		if len(service.Start) == 0 && len(serviceConfig.Command()) == 0 {
			return Imported{}, fmt.Errorf("service %s: it has no start, and image %s names no command", service.Name, service.Image)
		}
		if err := writeImageConfig(tree, guestproto.ServiceImageConfigs+"/"+service.Name+".json", serviceConfig); err != nil {
			return Imported{}, err
		}
		if err := ensureMountpoints(root); err != nil {
			return Imported{}, err
		}
		if err := writeResolvConf(root); err != nil {
			return Imported{}, err
		}
		if err := writeHosts(root, spec.Services); err != nil {
			return Imported{}, err
		}
	}
	if len(spec.Services) > 0 {
		if err := writeHosts(tree, spec.Services); err != nil {
			return Imported{}, err
		}
	}
	began = time.Now()
	if err := b.writeRootfs(ctx, tree, spec.RootfsPath, size); err != nil {
		report("rootfs", began, err, "")
		return Imported{}, err
	}
	report("rootfs", began, nil, "")
	return Imported{Digest: digest, Config: config}, nil
}

// writeImageConfig stores what one image says about how it runs, for the guest.
func writeImageConfig(root, guestPath string, config guestproto.ImageConfig) error {
	b, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return writeTreeFile(root, strings.TrimPrefix(guestPath, "/"), b, 0o644)
}

// writeHosts names every service on 127.0.0.1, because all of them share one
// network. A recipe reaches a service by name.
func writeHosts(root string, services []ServiceRequest) error {
	names := make([]string, 0, len(services))
	for _, service := range services {
		names = append(names, service.Name)
	}
	content := "127.0.0.1 localhost " + strings.Join(names, " ") + "\n::1 localhost\n"
	return writeTreeFile(root, "etc/hosts", []byte(content), 0o644)
}

// writeRootfs injects what the guest needs and writes the ext4 image.
func (b *Builder) writeRootfs(ctx context.Context, tree, rootfsPath string, size int) error {
	if err := injectKilninit(tree, b.KilninitPath); err != nil {
		return err
	}
	if err := writeResolvConf(tree); err != nil {
		return err
	}
	if err := ensureMountpoints(tree); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "mke2fs",
		"-q", "-F", "-t", "ext4", "-d", tree, "-L", "kiln",
		rootfsPath, fmt.Sprintf("%dM", size))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("template: mke2fs: %w: %s", err, out)
	}
	return nil
}

// withBuildCredential returns the credentials of one build: the one the caller
// sent for this build on the registry of the image, and the stored ones on
// every other registry. A job token must not reach another registry.
func withBuildCredential(stored []RegistryCredential, image string, cred *Credential) ([]RegistryCredential, error) {
	if cred == nil {
		return stored, nil
	}
	r, err := ref.New(image)
	if err != nil {
		return nil, fmt.Errorf("template: %s: %w", image, err)
	}
	out := make([]RegistryCredential, 0, len(stored)+1)
	for _, c := range stored {
		if c.Host != r.Registry {
			out = append(out, c)
		}
	}
	return append(out, RegistryCredential{Host: r.Registry, Username: cred.Username, Token: cred.Token}), nil
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

// pull resolves the platform image and applies its layers to root. The manifest
// always comes from the registry, so a tenant that lost access to an image
// fails here even when the cache holds every layer. It returns the digest and
// a line that says how much the cache saved.
func (b *Builder) pull(ctx context.Context, imageRef, root string, cache *layerCache) (string, guestproto.ImageConfig, string, error) {
	digest, config, cached, total, err := b.pullLayers(ctx, imageRef, root, cache)
	if err != nil {
		return "", guestproto.ImageConfig{}, "", err
	}
	cache.evict()
	return digest, config, fmt.Sprintf("%d of %d layers cached", cached, total), nil
}

func (b *Builder) pullLayers(ctx context.Context, imageRef, root string, cache *layerCache) (string, guestproto.ImageConfig, int, int, error) {
	var none guestproto.ImageConfig
	r, err := ref.New(imageRef)
	if err != nil {
		return "", none, 0, 0, fmt.Errorf("template: %s: %w", imageRef, err)
	}
	// The operator's own Docker credentials are deliberately not loaded: a
	// tenant must not borrow them to reach a registry the operator can
	// reach.
	rc := regclient.New(regclient.WithConfigHosts(b.hosts()))
	defer rc.Close(ctx, r)

	m, err := rc.ManifestGet(ctx, r)
	if err != nil {
		return "", none, 0, 0, fmt.Errorf("template: pull %s: %w", imageRef, err)
	}
	if m.IsList() {
		desc, err := manifest.GetPlatformDesc(m, &platform.Platform{OS: b.os(), Architecture: b.arch()})
		if err != nil {
			return "", none, 0, 0, fmt.Errorf("template: %s: %w", imageRef, err)
		}
		r = r.SetDigest(desc.Digest.String())
		m, err = rc.ManifestGet(ctx, r)
		if err != nil {
			return "", none, 0, 0, fmt.Errorf("template: pull %s: %w", imageRef, err)
		}
	}
	img, ok := m.(manifest.Imager)
	if !ok {
		return "", none, 0, 0, fmt.Errorf("template: %s: manifest has no layers", imageRef)
	}
	layers, err := img.GetLayers()
	if err != nil {
		return "", none, 0, 0, fmt.Errorf("template: %s: %w", imageRef, err)
	}
	configDesc, err := img.GetConfig()
	if err != nil {
		return "", none, 0, 0, fmt.Errorf("template: %s: %w", imageRef, err)
	}
	configBlob, err := rc.BlobGetOCIConfig(ctx, r, configDesc)
	if err != nil {
		return "", none, 0, 0, fmt.Errorf("template: %s: config: %w", imageRef, err)
	}
	runConfig := configBlob.GetConfig().Config
	config := guestproto.ImageConfig{
		Env: runConfig.Env, WorkingDir: runConfig.WorkingDir, User: runConfig.User,
		Prefix: runConfig.Entrypoint, Cmd: runConfig.Cmd,
	}
	cached := 0
	for _, layer := range layers {
		hit, err := applyCachedLayer(ctx, rc, r, layer, root, cache)
		if err != nil {
			return "", none, 0, 0, fmt.Errorf("template: %s: layer %s: %w", imageRef, layer.Digest, err)
		}
		if hit {
			cached++
		}
	}
	return m.GetDescriptor().Digest.String(), config, cached, len(layers), nil
}

// applyCachedLayer applies one layer from its file, pulling it first when the
// cache does not hold it. It reports whether the cache held it.
func applyCachedLayer(ctx context.Context, rc *regclient.RegClient, r ref.Ref, layer descriptor.Descriptor, root string, cache *layerCache) (bool, error) {
	file, hit, err := cache.open(layer.Digest)
	if err != nil {
		return false, err
	}
	if !hit {
		br, err := rc.BlobGet(ctx, r, layer)
		if err != nil {
			return false, err
		}
		err = cache.add(layer.Digest, br)
		// Only the blob reader releases the registry slot.
		if closeErr := br.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return false, err
		}
		if file, _, err = cache.open(layer.Digest); err != nil {
			return false, err
		}
		if file == nil {
			return false, fmt.Errorf("the pulled layer left the cache")
		}
	}
	defer file.Close()
	return hit, applyLayer(root, tar.NewReader(file))
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
