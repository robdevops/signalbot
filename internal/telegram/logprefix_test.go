package telegram

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every line the package logs goes through its own log, which prefixes it
// with the network, and none adds the network again in its own words.
func TestLogsOnlyThroughThePackageLog(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	levels := regexp.MustCompile(`^(Crit|Error|Warn|Notice|Info|Debug)f$|^Recover$`)
	again := regexp.MustCompile(`(?i)^(signal|telegram|discord|slack)\b[^-]`)
	fset := gotoken.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !levels.MatchString(sel.Sel.Name) {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "logx" {
				t.Errorf("%s: logx.%s, not the package's log", fset.Position(call.Pos()), sel.Sel.Name)
			}
			if len(call.Args) > 0 {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == gotoken.STRING {
					if s, _ := strconv.Unquote(lit.Value); again.MatchString(s) {
						t.Errorf("%s: %q names the network the prefix already does", fset.Position(call.Pos()), s)
					}
				}
			}
			return true
		})
	}
}
