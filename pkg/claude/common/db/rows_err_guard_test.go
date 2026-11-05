package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryRowsLoopChecksErr fails when a function in this package iterates
// `for x.Next()` without calling x.Err(). database/sql reports an error that
// ends iteration only through Rows.Err: Close does not return it. A loop that
// skips Err silently returns, deletes or updates from a truncated list.
// Migrations (migrate*.go) are skipped: they are frozen history, run once on
// old databases and not edited after release.
func TestEveryRowsLoopChecksErr(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var missing []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "migrate") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, rows := range rowsWithoutErr(fn.Body) {
				missing = append(missing, name+":"+fn.Name.Name+" ("+rows+")")
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("these functions loop over %s.Next() without checking %s.Err(); add `if err := rows.Err(); err != nil { return ... }` after the loop:\n  %s",
			"rows", "rows", strings.Join(missing, "\n  "))
	}
}

// rowsWithoutErr names each identifier looped over with `for x.Next()` in
// body more times than body calls x.Err(). Counting, not just presence,
// catches a function that reuses one rows variable for two queries and
// checks only one of them.
func rowsWithoutErr(body *ast.BlockStmt) []string {
	looped := map[string]int{}
	checked := map[string]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ForStmt:
			if id := methodReceiver(n.Cond, "Next"); id != "" {
				looped[id]++
			}
		case *ast.CallExpr:
			if id := methodReceiver(n, "Err"); id != "" {
				checked[id]++
			}
		}
		return true
	})
	var out []string
	for id, n := range looped {
		if checked[id] < n {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// methodReceiver returns x for a call expression x.<method>() on a plain
// identifier, else "".
func methodReceiver(e ast.Expr, method string) string {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}
