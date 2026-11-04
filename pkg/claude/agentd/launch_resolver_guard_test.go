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
	"agent_bundle.go":                      true,
	"awb_ready_poller.go":                  true, // readiness check walks [named, group, global] itself
	"federation_placement_requirements.go": true, // placement walks [group, global] itself
	"launch_value_fields.go":               true, // the resolver itself
	"launch_identity.go":                   true, // the identity and birth-time access resolver
	"managed_launch.go":                    true, // the pipeline for daemon-composed launches
	"lifecycle.go":                         true,
	"sandbox_default_implementation.go":    true,
	"spawn_launch_defaults.go":             true,
	"templates.go":                         true,
}

// launchDefaultProfileDisplayFiles read the global default profile only to
// show which one is set, not to resolve a launch. They may use the loader and
// nothing else from the tier walk.
var launchDefaultProfileDisplayFiles = map[string]bool{
	"dashboard.go":      true,
	"peer_snapshot.go":  true,
	"spawn_profiles.go": true,
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

	// The default-tier loaders are included: reading the group or global
	// default profile directly is how a hand-rolled tier stack starts.
	walkers := map[string]bool{"applyDefaultProfile": true, "groupDefaultProfile": true, "globalDefaultProfile": true}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			for _, field := range fn.Type.Params.List {
				if isTierSlice(field.Type) {
					walkers[fn.Name.Name] = true
				}
			}
		}
	}

	using := map[string][]string{}
	for name, f := range files {
		seen := map[string]bool{}
		note := func(what string) {
			if !seen[what] {
				seen[what] = true
				using[name] = append(using[name], what)
			}
		}
		declared := map[*ast.Ident]bool{}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				declared[fn.Name] = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				// Any reference counts, a call or a function value alike.
				if walkers[x.Name] && !declared[x] {
					note(x.Name)
				}
			case *ast.CompositeLit:
				if isTierElem(x.Type) || isTierSlice(x.Type) {
					note("launchProfileTier literal")
				}
			case *ast.FuncLit:
				for _, field := range x.Type.Params.List {
					if isTierSlice(field.Type) {
						note("func literal over []launchProfileTier")
					}
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
		if launchDefaultProfileDisplayFiles[name] && onlyDisplayUse(using[name]) {
			continue
		}
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
	for name := range launchDefaultProfileDisplayFiles {
		if _, ok := using[name]; !ok {
			t.Errorf("%s no longer reads the default profile: remove it from launchDefaultProfileDisplayFiles", name)
		}
	}
}

func onlyDisplayUse(uses []string) bool {
	for _, u := range uses {
		if u != "globalDefaultProfile" && u != "groupDefaultProfile" {
			return false
		}
	}
	return true
}

// isTierElem matches launchProfileTier and *launchProfileTier.
func isTierElem(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "launchProfileTier"
}

// isTierSlice matches []launchProfileTier, []*launchProfileTier and the
// variadic ...launchProfileTier.
func isTierSlice(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.ArrayType:
		return isTierElem(t.Elt)
	case *ast.Ellipsis:
		return isTierElem(t.Elt)
	}
	return false
}
