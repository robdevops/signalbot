// Package state keeps the bot's saved state: one JSON document per name in
// the state directory, each written whole and atomically.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/robdevops/signalbot/internal/kit/fsx"
)

// MaxBytes caps one saved document, its final newline included, so a
// feature can't fill the disk: 4 MiB.
const MaxBytes = 4 << 20

// Reserved is the core's own saved state, the daily budget, which no
// feature may open.
const Reserved = "budget"

// namePattern is what a saved state may be called: it names a file.
var namePattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// CheckName fails unless name is a feature's to open: lowercase letters,
// digits, - and _, up to 32, and not Reserved.
func CheckName(name string) error {
	switch {
	case !namePattern.MatchString(name):
		return fmt.Errorf("a saved state can't be called %q: use lowercase letters, digits, - and _, up to 32", clamp(name))
	case name == Reserved:
		return fmt.Errorf("a saved state can't be called %q: it is the core's", name)
	}
	return nil
}

// clamp keeps a bad name short in an error.
func clamp(s string) string {
	if r := []rune(s); len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}

// DockerDir is the Docker image's STATE_DIRECTORY, a bind mount whose owner
// the bot can't fix itself, so its message says how to fix the host's.
var DockerDir = "/data"

// Dir is the state directory from STATE_DIRECTORY's value: its first
// directory (systemd may list several), or the working directory when it
// isn't set.
func Dir(env string) string {
	dir, _, _ := strings.Cut(env, ":")
	if dir == "" {
		return "."
	}
	return dir
}

// Check fails, saying how to fix it, unless dir can be listed and written.
func Check(dir string) error {
	err := usable(dir)
	switch {
	case err == nil:
		return nil
	case filepath.Clean(dir) == DockerDir:
		return fmt.Errorf("%s must be writable by the bot's user (65532): on the host, chown 65532 "+
			"the directory mounted at %s, such as /var/lib/private/signalbot (%v)", dir, DockerDir, err)
	}
	where := "its state directory " + dir
	if dir == "." {
		where = "the working directory, as STATE_DIRECTORY isn't set"
	}
	return fmt.Errorf("the bot can't list and write %s (%v); "+
		"under systemd, add StateDirectory=signalbot to the unit's [Service] section, "+
		"or set STATE_DIRECTORY to a directory the bot can write", where, err)
}

// usable checks that dir can be listed and written to, leaving nothing.
func usable(dir string) error {
	if _, err := os.ReadDir(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".check-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// Doc is one saved document, <name>.json in its directory.
type Doc struct {
	mu   sync.Mutex
	path string
}

// Open is the document called name in dir; nothing is read until Load.
func Open(dir, name string) *Doc { return &Doc{path: filepath.Join(dir, name+".json")} }

// Path is where the document is saved.
func (d *Doc) Path() string { return d.path }

// Load reads the document into v; found is false, with no error, when
// nothing is saved yet. A file that exists but can't be read or parsed is
// an error, so the caller can stop rather than overwrite it.
func (d *Doc) Load(v any) (found bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	data, err := os.ReadFile(d.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err == nil {
		err = json.Unmarshal(data, v)
	}
	if err != nil {
		return false, fmt.Errorf("%s can't be read: %v", d.path, err)
	}
	return true, nil
}

// Save replaces the document with v, written to a temporary file beside it
// and renamed over it, so a crash never leaves half of it. Over MaxBytes,
// nothing is written.
func (d *Doc) Save(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > MaxBytes { // with its newline
		return fmt.Errorf("%s would be %d bytes, over the %d a saved state may have", d.path, len(data), MaxBytes)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return fsx.WriteAtomic(d.path, append(data, '\n'))
}
