package web

import (
	"os"
	"path/filepath"
)

// writeFileAtomic persists b to path via a temp file + rename so a crash
// mid-write can never leave a truncated store file that fails to load.
// On Windows, os.Rename cannot replace an existing file, so a leftover
// destination is removed before retrying the rename.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		info, statErr := os.Stat(path)
		if statErr != nil || info.IsDir() {
			return err
		}
		if rmErr := os.Remove(path); rmErr != nil {
			return err
		}
		if err := os.Rename(tmpName, path); err != nil {
			return err
		}
	}
	cleanup = false
	return nil
}
