package template

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	digest "github.com/opencontainers/go-digest"
)

// layerCacheLimit is how much one tenant's cache holds after a pull.
const layerCacheLimit = 4 << 30

// layerCache keeps pulled layers as files named by their digest. The host
// checks the digest before it keeps a file, so the name proves the content.
type layerCache struct {
	dir   string
	limit int64
}

func newLayerCache(dir string, limit int64) (*layerCache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &layerCache{dir: dir, limit: limit}, nil
}

// path refuses a digest that is not one, because the name becomes a file name
// and the manifest comes from a registry.
func (c *layerCache) path(d digest.Digest) (string, error) {
	if err := d.Validate(); err != nil {
		return "", fmt.Errorf("layer digest %q: %w", d, err)
	}
	return filepath.Join(c.dir, d.Algorithm().String()+"-"+d.Encoded()), nil
}

// open returns the file of one layer and marks it as recently used. A file
// that no longer matches its digest is removed and counts as a miss, so a
// corrupt cache pulls again.
func (c *layerCache) open(d digest.Digest) (*os.File, bool, error) {
	path, err := c.path(d)
	if err != nil {
		return nil, false, err
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	verifier := d.Verifier()
	if _, err := io.Copy(verifier, f); err != nil {
		f.Close()
		return nil, false, err
	}
	if !verifier.Verified() {
		f.Close()
		return nil, false, os.Remove(path)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, false, err
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		f.Close()
		return nil, false, err
	}
	return f, true, nil
}

// add writes one layer and keeps it only when it matches its digest. The rename
// means a concurrent build reads a whole file or none.
func (c *layerCache) add(d digest.Digest, src io.Reader) error {
	path, err := c.path(d)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dir, ".pull-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	verifier := d.Verifier()
	if _, err := io.Copy(io.MultiWriter(tmp, verifier), src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !verifier.Verified() {
		return fmt.Errorf("the registry sent bytes that do not match %s", d)
	}
	return os.Rename(tmp.Name(), path)
}

// evict removes the least recently used layers until the cache fits. A build
// that has one of them open keeps reading it, and a build about to open one
// pulls it again. A failure only leaves the cache larger.
func (c *layerCache) evict() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type layerFile struct {
		path string
		size int64
		used time.Time
	}
	var files []layerFile
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || entry.Name()[0] == '.' {
			continue
		}
		files = append(files, layerFile{filepath.Join(c.dir, entry.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if total <= c.limit {
			return
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
