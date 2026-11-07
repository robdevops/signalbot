package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A document saves and loads whole; nothing saved is not an error; a file
// that doesn't parse is, and is left as it was; one over MaxBytes isn't
// written.
func TestDoc(t *testing.T) {
	dir := t.TempDir()
	d := Open(dir, "notes")
	var got map[string]int
	if found, err := d.Load(&got); found || err != nil {
		t.Fatalf("empty: %v, %v", found, err)
	}
	if err := d.Save(map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if found, err := d.Load(&got); !found || err != nil || got["a"] != 1 || d.Path() != filepath.Join(dir, "notes.json") {
		t.Fatalf("loaded %v, %v, %v", got, found, err)
	}
	if err := d.Save(strings.Repeat("x", MaxBytes-2)); err == nil { // two quotes and a newline: one byte over
		t.Fatal("saved over the cap")
	}
	if err := d.Save(strings.Repeat("x", MaxBytes-3)); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	if fi, _ := os.Stat(d.Path()); fi.Size() != MaxBytes {
		t.Fatalf("%d bytes at the cap", fi.Size())
	}
	d.Save(map[string]int{"a": 1})
	if found, _ := d.Load(&got); !found || got["a"] != 1 {
		t.Fatal("the cap overwrote the file")
	}
	os.WriteFile(d.Path(), []byte("{not json"), 0o600)
	if _, err := d.Load(&got); err == nil || !strings.Contains(err.Error(), d.Path()+" can't be read") {
		t.Fatalf("corrupt: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left %v", entries)
	}
}

// STATE_DIRECTORY's first directory is used; unset, the working directory.
func TestDir(t *testing.T) {
	for env, want := range map[string]string{"": ".", "/a": "/a", "/a:/b": "/a"} {
		if got := Dir(env); got != want {
			t.Errorf("%q: %q", env, got)
		}
	}
}

// A directory the bot can't list and write says how to fix it: in Docker,
// the host directory's owner; elsewhere, the unit's StateDirectory. A
// usable one is left as it was. (Tests may run as root, which file modes
// don't stop, so the directories here are missing rather than read-only.)
func TestCheck(t *testing.T) {
	const fix = "add StateDirectory=signalbot to the unit's [Service] section"
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if err := Check(missing); err == nil || !strings.Contains(err.Error(), "its state directory "+missing+" (") || !strings.Contains(err.Error(), fix) {
		t.Fatalf("got %v", err)
	}
	old := DockerDir
	t.Cleanup(func() { DockerDir = old })
	DockerDir = missing
	if err := Check(missing); err == nil || !strings.Contains(err.Error(), "chown 65532 the directory mounted at "+missing+", such as /var/lib/private/signalbot") {
		t.Fatalf("docker: %v", err)
	}
	gone := filepath.Join(dir, "gone")
	os.Mkdir(gone, 0o700)
	t.Chdir(gone)
	os.Remove(gone)
	if err := Check("."); err == nil || !strings.Contains(err.Error(), "the working directory, as STATE_DIRECTORY isn't set") || !strings.Contains(err.Error(), fix) {
		t.Fatalf("working directory: %v", err)
	}
	if err := Check(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
}

// A name is a safe file name, and the core's is refused.
func TestCheckName(t *testing.T) {
	for name, ok := range map[string]bool{"notes": true, "a-b_c9": true, "budget": false, "../x": false, "Notes": false, "": false, strings.Repeat("a", 33): false} {
		if err := CheckName(name); (err == nil) != ok {
			t.Errorf("%q: %v", name, err)
		}
	}
}
