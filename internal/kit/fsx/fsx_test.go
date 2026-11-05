package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// The file is replaced whole and no temporary file is left; a directory
// that doesn't exist is an error.
func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	for _, want := range []string{"one", "two"} {
		if err := WriteAtomic(p, []byte(want)); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); string(got) != want {
			t.Fatalf("got %q", got)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("%d files left", len(entries))
	}
	if WriteAtomic(filepath.Join(dir, "missing", "x"), nil) == nil {
		t.Fatal("no error for a missing directory")
	}
}
