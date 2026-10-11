package agentd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPeerViewDashboardRouteClassification(t *testing.T) {
	files, err := filepath.Glob("dashboard*.go")
	if err != nil {
		t.Fatal(err)
	}
	rules := peerViewRules()
	for _, filename := range files {
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			if len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: dashboard route must have an explicit peer classification", filename)
				return true
			}
			pattern, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := rules[pattern]; !ok {
				t.Errorf("%s: unclassified dashboard route %q", filename, pattern)
			}
			return true
		})
	}
	// Constructing the real mux also checks for overlapping route patterns.
	_ = PeerViewHandler("test-peer")
}
