// Package fileutil holds tiny filesystem helpers shared across handlers
// and services. Kept deliberately small so that any package can import
// it without pulling in domain types.
package fileutil

import (
	"os"
	"path/filepath"
)

// AtomicWriteFile writes data to a sibling tempfile then renames it
// over path. os.Rename is atomic on POSIX when the source and
// destination share a filesystem, so concurrent readers never see a
// partially-written file. Matches os.WriteFile's signature otherwise.
//
// The close error is checked — an unflushed write on NFS or a full
// disk can surface there only, and silently dropping it would
// produce a valid-looking file with truncated contents.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
