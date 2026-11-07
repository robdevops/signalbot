package arch_test

import (
	"go/parser"
	"go/token"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/arch"
)

const (
	root   = "../.."
	module = "github.com/robdevops/signalbot/"
)

// goFiles is every Go file in the module, by its path from the root.
func goFiles(t *testing.T) []string {
	t.Helper()
	files, err := arch.Files(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// under reports whether p is dir or inside it.
func under(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// Only the main package, at the module root, imports features and the chat
// network (internal/signal); a feature imports only internal/feature, kit
// and its own packages, so never internal/bot; internal/feature imports only
// its own packages and kit; internal/signal imports only those,
// internal/config and internal/bot (for Received: the network depends on
// the core, never the reverse); internal/config and kit import no other
// internal package.
func TestImports(t *testing.T) {
	for _, p := range goFiles(t) {
		f, err := parser.ParseFile(token.NewFileSet(), root+"/"+p, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		dir := path.Dir(p)
		own := dir // a feature's own packages: everything under internal/features/<name>
		if parts := strings.Split(dir, "/"); len(parts) >= 3 && parts[1] == "features" {
			own = strings.Join(parts[:3], "/")
		}
		for _, spec := range f.Imports {
			imp, _ := strconv.Unquote(spec.Path.Value)
			if !strings.HasPrefix(imp, module) {
				continue
			}
			imp = strings.TrimPrefix(imp, module)
			var ok bool
			switch {
			case under(dir, "internal/features"):
				ok = under(imp, "internal/feature") || under(imp, "internal/kit") || under(imp, own)
			case under(dir, "internal/feature"):
				ok = under(imp, "internal/feature") || under(imp, "internal/kit")
			case under(dir, "internal/signal"):
				ok = under(imp, "internal/feature") || under(imp, "internal/kit") || imp == "internal/config" || imp == "internal/bot"
			case under(dir, "internal/kit"), dir == "internal/config":
				ok = under(imp, "internal/kit")
			default:
				ok = dir == "." || !under(imp, "internal/features") && !under(imp, "internal/signal")
			}
			if !ok {
				t.Errorf("%s imports %s", p, imp)
			}
		}
	}
}

// featureWords are the names of features and words only they use.
var featureWords = regexp.MustCompile(`(?i)chart|donut|\bpies?\b|mcp|groupctx|group.?context|websearch|web.?search|\bvision\b|/intro|/schedule`)

// The core never mentions a feature, so deleting one leaves it unchanged.
// The scenarios at the module root test the whole bot, features included.
func TestNoFeatureWordsInTheCore(t *testing.T) {
	for _, p := range goFiles(t) {
		core := under(p, "internal") || path.Dir(p) == "."
		scenario := strings.HasPrefix(p, "scenario_")
		if !core || scenario || under(p, "internal/features") || under(p, "internal/arch") || p == "features.go" {
			continue
		}
		data, err := os.ReadFile(root + "/" + p)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if w := featureWords.FindString(line); w != "" {
				t.Errorf("%s:%d mentions %q", p, i+1, w)
			}
		}
	}
	if w := featureWords.FindString("make_pie_chart, mcp, web search, Vision, /schedule"); w == "" {
		t.Error("the pattern finds nothing")
	}
}

// Every setting the bot reads is in the README's configuration table: each
// one its code names to Env, the core's (and number's, in config) and each
// feature's.
func TestSettingsAreDocumented(t *testing.T) {
	var settings []string
	env := regexp.MustCompile(`\b(?:Env|number)\("([A-Z0-9_]+)"`)
	for _, p := range goFiles(t) {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		data, err := os.ReadFile(root + "/" + p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range env.FindAllStringSubmatch(string(data), -1) {
			settings = append(settings, m[1])
		}
	}
	readme, err := os.ReadFile(root + "/README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"MAX_CONCURRENCY", "SIGNAL_DM_USER_IDS", "MCP_CONFIG"} { // one each from config, signal and a feature
		if !slices.Contains(settings, s) {
			t.Fatalf("%s not found in %v", s, settings)
		}
	}
	for _, s := range settings {
		if !strings.Contains(string(readme), "\n| `"+s+"` |") {
			t.Errorf("%s is not in the README's configuration table", s)
		}
	}
}

// fileWrites are the calls that change files.
var fileWrites = regexp.MustCompile(`\bos\.(WriteFile|Create|CreateTemp|OpenFile|Mkdir|MkdirAll|MkdirTemp|Rename|Remove|RemoveAll|Chmod|Chown|Symlink|Link|Truncate)\(|\bfsx\.|kit/state"`)

// A feature saves only through Host.Store, so every file the bot writes is
// a named document in the state directory, listed in PRIVACY.md.
func TestFeaturesSaveOnlyThroughTheStore(t *testing.T) {
	for _, p := range goFiles(t) {
		if !under(p, "internal/features") || strings.HasSuffix(p, "_test.go") {
			continue
		}
		data, err := os.ReadFile(root + "/" + p)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if w := fileWrites.FindString(line); w != "" {
				t.Errorf("%s:%d writes files itself (%s): use Host.Store", p, i+1, w)
			}
		}
	}
	for _, line := range []string{`os.WriteFile(p, b, 0o600)`, `fsx.WriteAtomic(p, b)`, `"github.com/robdevops/signalbot/internal/kit/state"`, `os.Remove(name)`} {
		if fileWrites.FindString(line) == "" {
			t.Errorf("not caught: %s", line)
		}
	}
	if fileWrites.FindString(`data, err := os.ReadFile(path)`) != "" {
		t.Error("reading a file is allowed")
	}
}
