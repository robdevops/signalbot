// Package arch measures the code and holds the tests that keep its shape:
// file and package sizes, and which packages may import which.
package arch

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Size limits. A file over FileLimit or a package over PackageLimit fails
// the tests, unless it is listed in the exceptions there.
const (
	FileLimit    = 1000
	PackageLimit = 1500
)

// File is one Go file's line count.
type File struct {
	Path  string // slash-separated, relative to the module root
	Lines int
	Test  bool
}

// Files counts the lines of every .go file under root.
func Files(root string) ([]File, error) {
	var out []File
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		n, err := lines(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, File{filepath.ToSlash(rel), n, strings.HasSuffix(p, "_test.go")})
		return nil
	})
	return out, err
}

func lines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		n++
	}
	return n, sc.Err()
}

// Utility reports whether a file is a utility rather than the bot's code:
// these tests and tools, and test helpers. Utilities and tests are left out
// of the reported sizes.
func Utility(path string) bool {
	return strings.HasPrefix(path, "internal/arch/") || strings.Contains(path, "test/")
}

// Packages sums the lines of the bot's code (no tests, no utilities) per
// package directory.
func Packages(files []File) map[string]int {
	out := map[string]int{}
	for _, f := range files {
		if f.Test || Utility(f.Path) {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(f.Path))
		out[dir] += f.Lines
	}
	return out
}
