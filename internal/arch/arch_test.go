package arch

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/robdevops/signalbot"

// root is the module root, two levels up from this package.
var root = filepath.Join("..", "..")

// oversized lists the files and packages over the limits from before the
// recode. An entry goes when its file or package is split; none may be added.
var oversized = map[string]bool{
	"bot.go": true, "chart.go": true, "mcp.go": true, "signal.go": true, "openrouter.go": true,
	"fixes_test.go": true, "bot_test.go": true, "images_test.go": true, "chart_test.go": true,
	"mcp_test.go": true, "toolloop_test.go": true, ".": true,
}

func TestFileSizes(t *testing.T) {
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if oversized[f.Path] {
			continue
		}
		if f.Lines > FileLimit {
			t.Errorf("%s has %d lines, over the %d limit: split it by role", f.Path, f.Lines, FileLimit)
		}
	}
}

func TestPackageSizes(t *testing.T) {
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	for dir, n := range Packages(files) {
		if oversized[dir] {
			continue
		}
		if n > PackageLimit {
			t.Errorf("package %s has %d lines, over the %d limit: split it into sub-packages", dir, n, PackageLimit)
		}
	}
}

// The core knows no feature, kit knows nothing internal, and a feature
// knows only the ports, kit and the standard library.
func TestImports(t *testing.T) {
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		dir := filepath.ToSlash(filepath.Dir(f.Path))
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, f.Path), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(p, module+"/") {
				continue
			}
			if why := forbidden(dir, strings.TrimPrefix(p, module+"/")); why != "" {
				t.Errorf("%s imports %s: %s", f.Path, p, why)
			}
		}
	}
}

// forbidden says why package dir may not import target, or "" if it may.
func forbidden(dir, target string) string {
	feature := func(p string) string { // the feature a path belongs to, or ""
		rest, ok := strings.CutPrefix(p+"/", "internal/features/")
		if !ok {
			return ""
		}
		return strings.SplitN(rest, "/", 2)[0]
	}
	switch {
	case dir == "cmd/signalbot" || strings.HasPrefix(dir, "internal/arch"):
		return ""
	case strings.HasPrefix(dir, "internal/kit/"):
		if !strings.HasPrefix(target, "internal/kit/") {
			return "kit imports nothing internal but other kit"
		}
	case feature(dir) != "":
		if f := feature(target); f != "" && f != feature(dir) {
			return "a feature never imports another feature"
		}
		if feature(target) == "" && target != "internal/feature" && !strings.HasPrefix(target, "internal/kit/") {
			return "a feature imports only internal/feature, kit and its own packages"
		}
	case feature(target) != "":
		return "the core knows no feature: only cmd/signalbot/features.go lists them"
	}
	return ""
}

func TestForbidden(t *testing.T) {
	for _, c := range []struct {
		dir, target string
		ok          bool
	}{
		{"internal/bot", "internal/features/charts", false},
		{"internal/bot", "internal/llm", true},
		{"internal/features/charts/line", "internal/features/charts/offer", true},
		{"internal/features/charts", "internal/features/mcp", false},
		{"internal/features/charts", "internal/bot", false},
		{"internal/features/charts", "internal/kit/draw", true},
		{"internal/features/mcp", "internal/feature", true},
		{"internal/kit/linechart", "internal/kit/draw", true},
		{"internal/kit/draw", "internal/logx", false},
		{"cmd/signalbot", "internal/features/charts", true},
	} {
		if got := forbidden(c.dir, c.target) == ""; got != c.ok {
			t.Errorf("%s -> %s: allowed %v, want %v", c.dir, c.target, got, c.ok)
		}
	}
}
