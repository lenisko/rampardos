package services

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// promoteFlatStyleFiles migrates legacy Styles/<id>.json files into
// the directory layout Styles/<id>/style.json that the current
// renderer expects. Intended to run once at startup as a one-shot
// bridge from the tileserver-gl-era flat layout.
//
// Never overwrites: if <id>/style.json already exists the flat file
// is left in place and a warning is logged so an operator can
// reconcile the conflict. styles.json (the legacy index) and the
// External/ directory are both reserved and skipped.
func promoteFlatStyleFiles(folder string) error {
	entries, err := os.ReadDir(folder)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "styles.json" || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if id == "" {
			continue
		}

		flat := filepath.Join(folder, name)
		targetDir := filepath.Join(folder, id)
		target := filepath.Join(targetDir, "style.json")

		if _, err := os.Stat(target); err == nil {
			slog.Warn("Skipping legacy style promotion: target already exists",
				"flat", flat, "target", target)
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat %s: %w", target, err)
		}

		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", targetDir, err)
		}
		if err := os.Rename(flat, target); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", flat, target, err)
		}
		slog.Info("Promoted legacy style to directory layout", "id", id, "target", target)
	}
	return nil
}
