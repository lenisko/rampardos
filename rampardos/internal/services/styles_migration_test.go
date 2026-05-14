package services

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPromoteFlatStyleFilesBasic covers the simple happy path: a legacy
// flat Styles/<id>.json gets moved into Styles/<id>/style.json.
func TestPromoteFlatStyleFilesBasic(t *testing.T) {
	folder := t.TempDir()
	flat := filepath.Join(folder, "basic.json")
	payload := []byte(`{"version":8,"name":"basic"}`)
	if err := os.WriteFile(flat, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := promoteFlatStyleFiles(folder); err != nil {
		t.Fatalf("promoteFlatStyleFiles: %v", err)
	}

	target := filepath.Join(folder, "basic", "style.json")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("promoted file missing: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("promoted content mismatch: got %q want %q", got, payload)
	}
	if _, err := os.Stat(flat); !os.IsNotExist(err) {
		t.Fatalf("flat file should be gone after promotion: stat err=%v", err)
	}
}

// TestPromoteFlatStyleFilesNoOverwrite pins the critical invariant: if
// <id>/style.json already exists, the legacy flat <id>.json must NOT
// overwrite it. The flat file stays in place so an operator can
// reconcile the conflict manually.
func TestPromoteFlatStyleFilesNoOverwrite(t *testing.T) {
	folder := t.TempDir()
	existingDir := filepath.Join(folder, "clash")
	if err := os.MkdirAll(existingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := []byte(`{"version":8,"name":"existing"}`)
	if err := os.WriteFile(filepath.Join(existingDir, "style.json"), existing, 0o644); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(folder, "clash.json")
	flatPayload := []byte(`{"version":8,"name":"legacy"}`)
	if err := os.WriteFile(flat, flatPayload, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := promoteFlatStyleFiles(folder); err != nil {
		t.Fatalf("promoteFlatStyleFiles: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(existingDir, "style.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(existing) {
		t.Fatalf("existing style.json was overwritten: got %q want %q", got, existing)
	}
	if _, err := os.Stat(flat); err != nil {
		t.Fatalf("flat file should remain when a conflict prevents promotion: %v", err)
	}
}

// TestPromoteFlatStyleFilesEmptyDirFolded covers the legitimate case
// where the <id>/ directory exists (perhaps from a partially-extracted
// bundle) but has no style.json yet. Promotion should fill it in
// rather than skipping.
func TestPromoteFlatStyleFilesEmptyDirFolded(t *testing.T) {
	folder := t.TempDir()
	dir := filepath.Join(folder, "shell")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(folder, "shell.json")
	payload := []byte(`{"version":8,"name":"shell"}`)
	if err := os.WriteFile(flat, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := promoteFlatStyleFiles(folder); err != nil {
		t.Fatalf("promoteFlatStyleFiles: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "style.json"))
	if err != nil {
		t.Fatalf("promoted file missing: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("promoted content mismatch: got %q want %q", got, payload)
	}
	if _, err := os.Stat(flat); !os.IsNotExist(err) {
		t.Fatalf("flat file should be gone after promotion: stat err=%v", err)
	}
}

// TestPromoteFlatStyleFilesSkipsReserved makes sure the migration
// leaves bookkeeping files alone: Styles/styles.json (the legacy
// tileserver-gl index) and the Styles/External directory.
func TestPromoteFlatStyleFilesSkipsReserved(t *testing.T) {
	folder := t.TempDir()

	index := filepath.Join(folder, "styles.json")
	if err := os.WriteFile(index, []byte(`[]`), 0o644); err != nil {
		t.Fatal(err)
	}
	extDir := filepath.Join(folder, "External")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	extFile := filepath.Join(extDir, "styles.json")
	if err := os.WriteFile(extFile, []byte(`[]`), 0o644); err != nil {
		t.Fatal(err)
	}
	notStyle := filepath.Join(folder, "README.md")
	if err := os.WriteFile(notStyle, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := promoteFlatStyleFiles(folder); err != nil {
		t.Fatalf("promoteFlatStyleFiles: %v", err)
	}

	// styles.json index untouched at the expected path, not promoted
	// into a Styles/styles/ directory.
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("top-level styles.json index should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(folder, "styles", "style.json")); !os.IsNotExist(err) {
		t.Fatal("styles.json index must not be promoted into a styles/ subdirectory")
	}
	// External directory untouched.
	if _, err := os.Stat(extFile); err != nil {
		t.Fatalf("External/styles.json should remain: %v", err)
	}
	// Non-JSON file untouched.
	if _, err := os.Stat(notStyle); err != nil {
		t.Fatalf("README.md should remain: %v", err)
	}
}
