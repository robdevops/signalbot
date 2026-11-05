// Package fsx saves files so that a crash never leaves one half-written.
package fsx

import (
	"os"
	"path/filepath"
)

// WriteAtomic replaces the file at path with data: written to a temporary
// file beside it, synced, then renamed over it.
func WriteAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // does nothing once renamed
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	return err
}
