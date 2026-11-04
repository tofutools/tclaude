package agentd

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

// The launch-resolution series (tcl-a2e791) ends with one resolver that every
// spawn and relaunch path calls, and no per-path copy of the tier walk
// (operator, 2026-11-04). This guard holds that line while the slices land:
// the files that walk profile tiers or build a tier stack are listed below,
// and the list may only shrink.
//
// A "tier walk" is a call to applyDefaultProfile or to any function taking
// []launchProfileTier; a "tier stack" is a launchProfileTier composite literal.
// Both are found by syntax, so a new tier-walk helper is covered as soon as it
// exists.
//
// When a slice moves a path onto the pipeline, delete its file here. A file
// that newly needs the tier walk should call the pipeline instead; listing it
// here is reintroducing the copy this guard exists to stop.
var launchTierWalkFiles = map[string]bool{
	"agent_bundle.go":                   true,
	"launch_value_fields.go":            true, // the resolver itself
	"lifecycle.go":                      true,
	"sandbox_default_implementation.go": true,
	"spawn_launch_defaults.go":          true,
	"templates.go":                      true,
	"triggers.go":                       true,
}

func TestLaunchTierWalkStaysInListedFiles(t *testing.T) {
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = f
	}

	walkers := map[string]bool{"applyDefaultProfile": true}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			for _, field := range fn.Type.Params.List {
				if arr, ok := field.Type.(*ast.ArrayType); ok {
					if id, ok := arr.Elt.(*ast.Ident); ok && id.Name == "launchProfileTier" {
						walkers[fn.Name.Name] = true
					}
				}
			}
		}
	}

	using := map[string][]string{}
	for name, f := range files {
		seen := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && walkers[id.Name] && !seen[id.Name] {
					seen[id.Name] = true
					using[name] = append(using[name], id.Name+"()")
				}
			case *ast.CompositeLit:
				if id, ok := x.Type.(*ast.Ident); ok && id.Name == "launchProfileTier" && !seen["{}"] {
					seen["{}"] = true
					using[name] = append(using[name], "launchProfileTier{}")
				}
			}
			return true
		})
	}

	var names []string
	for name := range using {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !launchTierWalkFiles[name] {
			t.Errorf("%s walks launch profile tiers (%s); call the shared launch resolver instead of adding another copy",
				name, strings.Join(using[name], ", "))
		}
	}
	for name := range launchTierWalkFiles {
		if _, ok := using[name]; !ok {
			t.Errorf("%s no longer walks launch profile tiers: remove it from launchTierWalkFiles", name)
		}
	}
}
