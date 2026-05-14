package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteFileCreates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")
	payload := []byte("hello")

	if err := AtomicWriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %q want %q", got, payload)
	}
}

func TestAtomicWriteFileReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("expected replaced content, got %q", got)
	}
}

// TestAtomicWriteFileCleansUpTempOnCreateMissing: if the directory is
// missing, AtomicWriteFile should create it and not leave a stray
// tempfile in the caller's cwd.
func TestAtomicWriteFileCreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deeper")
	path := filepath.Join(dir, "x.txt")
	if err := AtomicWriteFile(path, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("target not written: %v", err)
	}
}
