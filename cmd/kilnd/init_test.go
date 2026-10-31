package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(path, []byte("kiln\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "ecdcbd502c7c341b7d547760cc09270a43d82db3257b8b0f96e98083e4e5643d"
	if got != want {
		t.Fatalf("fileSHA256 = %s, want %s", got, want)
	}
}

func TestFileSHA256MissingFileFails(t *testing.T) {
	if _, err := fileSHA256(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("fileSHA256 on a missing file returned no error")
	}
}
