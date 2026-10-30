package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
)

func TestLayerCacheKeepsOnlyWhatMatchesItsDigest(t *testing.T) {
	c, err := newLayerCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	good := digest.FromString("layer")
	if err := c.add(good, strings.NewReader("tampered")); err == nil {
		t.Fatal("the cache kept bytes that do not match their digest")
	}
	if _, hit, err := c.open(good); hit || err != nil {
		t.Fatalf("after a refused add: hit %t, err %v", hit, err)
	}
	if err := c.add(good, strings.NewReader("layer")); err != nil {
		t.Fatal(err)
	}
	f, hit, err := c.open(good)
	if err != nil || !hit {
		t.Fatalf("open: hit %t, err %v", hit, err)
	}
	f.Close()

	// A file corrupted on disk counts as a miss, and goes.
	path, _ := c.path(good)
	if err := os.WriteFile(path, []byte("rotten"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := c.open(good); hit || err != nil {
		t.Fatalf("corrupt file: hit %t, err %v", hit, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the corrupt file stayed: %v", err)
	}
}

// A manifest comes from a registry, so its digest must not name a path.
func TestLayerCacheRefusesADigestThatIsAPath(t *testing.T) {
	c, err := newLayerCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.open(digest.Digest("sha256:../../etc/passwd")); err == nil {
		t.Fatal("the cache accepted a digest that is a path")
	}
}

func TestLayerCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	c, err := newLayerCache(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	old, recent := digest.FromString("aaaaaa"), digest.FromString("bbbbbb")
	for _, l := range []struct {
		d       digest.Digest
		content string
		age     time.Duration
	}{{old, "aaaaaa", time.Hour}, {recent, "bbbbbb", time.Minute}} {
		if err := c.add(l.d, strings.NewReader(l.content)); err != nil {
			t.Fatal(err)
		}
		path, _ := c.path(l.d)
		at := time.Now().Add(-l.age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	c.evict()
	names, _ := filepath.Glob(filepath.Join(dir, "*"))
	recentPath, _ := c.path(recent)
	if len(names) != 1 || names[0] != recentPath {
		t.Fatalf("the cache holds %v, want only %s", names, recentPath)
	}
}
