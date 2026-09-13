// Package p2gate holds the P2 template gate. The gate itself is behind the
// kvm build tag; the OCI layout writer stays portable so it compiles in the
// fast checks.
package p2gate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layerEntry is one file in a test image layer. A name ending in "/" is a
// directory; a set Link makes a symlink.
type layerEntry struct {
	Name string
	Data string
	Link string
	Mode int64
}

// writeOCILayout writes an OCI image layout that regclient can read through
// its ocidir scheme.
func writeOCILayout(t *testing.T, dir, tag string, layers [][]layerEntry) {
	t.Helper()
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	type platform struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	}
	type descriptor struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations,omitempty"`
		Platform    *platform         `json:"platform,omitempty"`
	}

	var (
		layerDescs []descriptor
		diffIDs    []string
	)
	for _, entries := range layers {
		raw := tarBytes(t, entries)
		diffIDs = append(diffIDs, "sha256:"+digest(raw))
		compressed := gzipBytes(t, raw)
		writeBlob(t, dir, compressed)
		layerDescs = append(layerDescs, descriptor{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    "sha256:" + digest(compressed),
			Size:      int64(len(compressed)),
		})
	}

	config, err := json.Marshal(map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"rootfs":       map[string]any{"type": "layers", "diff_ids": diffIDs},
		"config":       map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeBlob(t, dir, config)
	configDesc := descriptor{
		MediaType: "application/vnd.oci.image.config.v1+json",
		Digest:    "sha256:" + digest(config),
		Size:      int64(len(config)),
	}

	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        configDesc,
		"layers":        layerDescs,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeBlob(t, dir, manifest)
	manifestDesc := descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    "sha256:" + digest(manifest),
		Size:      int64(len(manifest)),
		Platform:  &platform{Architecture: "amd64", OS: "linux"},
		Annotations: map[string]string{
			"org.opencontainers.image.ref.name": tag,
		},
	}

	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []descriptor{manifestDesc},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), index, 0o644); err != nil {
		t.Fatal(err)
	}
}

func tarBytes(t *testing.T, entries []layerEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.Mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.Name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(e.Data))}
		switch {
		case strings.HasSuffix(e.Name, "/"):
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
			hdr.Size = 0
		case e.Link != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.Link
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg && e.Data != "" {
			if _, err := tw.Write([]byte(e.Data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeBlob(t *testing.T, dir string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", digest(b)), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
