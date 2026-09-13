package template

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Manifest is the record of one template build, stored beside the snapshot.
type Manifest struct {
	Name        string    `json:"name"`
	ImageRef    string    `json:"image_ref"`
	ImageDigest string    `json:"image_digest"`
	VCPUs       int       `json:"vcpus"`
	MemoryMB    int       `json:"memory_mb"`
	DiskMB      int       `json:"disk_mb"`
	EgressAllow []string  `json:"egress_allow"`
	Setup       []string  `json:"setup,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// ReadManifest reads the manifest of a template directory.
func ReadManifest(dir string) (Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("template: manifest.json: %w", err)
	}
	if m.EgressAllow == nil {
		m.EgressAllow = []string{}
	}
	return m, nil
}

// writeManifest writes the manifest. It lands by rename, so a reader never
// sees a partial file.
func writeManifest(dir string, m Manifest) error {
	if m.EgressAllow == nil {
		m.EgressAllow = []string{}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := filepath.Join(dir, "manifest.json.part")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "manifest.json")); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
