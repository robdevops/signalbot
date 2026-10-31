package arch_test

import (
	"go/parser"
	"go/token"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/arch"
	"github.com/robdevops/signalbot/internal/config"
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

// Only cmd/signalbot imports features; a feature imports only
// internal/feature, kit and its own packages; internal/feature imports only
// kit; kit imports no other internal package.
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
			case under(dir, "internal/kit"):
				ok = under(imp, "internal/kit")
			default:
				ok = !under(imp, "internal/features") || dir == "cmd/signalbot"
			}
			if !ok {
				t.Errorf("%s imports %s", p, imp)
			}
		}
	}
}

// featureWords are the names of features and words only they use.
var featureWords = regexp.MustCompile(`(?i)chart|donut|\bpies?\b|mcp|groupctx|group.?context|websearch|web.?search|\bvision\b|/example|/schedule`)

// The core never mentions a feature, so deleting one leaves it unchanged.
func TestNoFeatureWordsInTheCore(t *testing.T) {
	for _, p := range goFiles(t) {
		core := under(p, "internal") || under(p, "cmd/signalbot")
		if !core || under(p, "internal/features") || under(p, "internal/arch") || p == "cmd/signalbot/features.go" {
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

// Every setting the bot reads is in the README's configuration table: the
// core's, recorded as config reads them, and each feature's, as it names
// them to Env.
func TestSettingsAreDocumented(t *testing.T) {
	config.Load() // records the core's settings, whatever it returns
	settings := config.Read()
	env := regexp.MustCompile(`\.Env\("([A-Z0-9_]+)"`)
	for _, p := range goFiles(t) {
		if under(p, "internal/features") {
			data, err := os.ReadFile(root + "/" + p)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range env.FindAllStringSubmatch(string(data), -1) {
				settings = append(settings, m[1])
			}
		}
	}
	readme, err := os.ReadFile(root + "/README.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) < 5 {
		t.Fatalf("only %v read", settings)
	}
	for _, s := range settings {
		if !strings.Contains(string(readme), "\n| `"+s+"` |") {
			t.Errorf("%s is not in the README's configuration table", s)
		}
	}
}
