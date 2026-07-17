package pathv1guard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	pathV1Import = "github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	pathV1Dir    = "pkg/claude/process/state/pathv1"
)

// releasedPureFileDeclarations is the small part of exclusive_pure.go used by
// the installed schema-7 executor/store boundary. Everything else exported by
// that file is the low-level pure planning surface and remains dormant.
var releasedPureFileDeclarations = []string{
	"ErrExclusiveUnsupported",
	"ExclusiveObservation",
	"VerifiedExclusiveInput",
	"VerifyExecutionInput",
	"(*VerifiedExclusiveInput).ParallelEnabled",
}

// dormantPureDeclarations is deliberately explicit. The surface-manifest
// check below fails closed when exclusive_pure.go gains an export, forcing a
// reviewer to classify it as released or dormant before production can pass.
var dormantPureDeclarations = []string{
	"ErrExclusiveInputInvalid",
	"ErrExclusiveNotRoutable",
	"ExclusiveCompletionInput",
	"ExclusiveDisposition",
	"ExclusiveProjection",
	"ExclusiveResolvedCancel",
	"ExclusiveResolvedRetry",
	"ExclusiveResolvedSkip",
	"ExclusiveRetryPending",
	"ExclusiveRouteReady",
	"VerifyExclusiveInput",
	"PlanExclusiveRoute",
	"ClassifyExclusiveObservation",
	"ReduceExclusiveRoute",
	"PlanExclusiveDeadPath",
	"ReduceExclusiveDeadPath",
	"PlanExclusiveDeadReservation",
	"ReduceExclusiveDeadReservation",
	"PlanExclusiveActivation",
	"ReduceExclusiveActivation",
	"PlanExclusiveEnd",
	"ReduceExclusiveEnd",
	"PlanExclusiveCompletion",
	"ReduceExclusiveCompletion",
	"(*ExclusiveProjection).Binding",
	"(*ExclusiveProjection).Command",
	"(*ExclusiveProjection).ReplayDisposition",
	"(*ExclusiveProjection).Routing",
}

func TestDormantPureExclusiveBoundary(t *testing.T) {
	root := moduleRoot(t)
	assertPureFileSurfaceClassified(t, root)

	// Allowed dependency direction: production scheduler, engine, executor,
	// store/migration, viewer, command, and API packages may use the released
	// schema-7 execution boundary. They must not call or name the lower-level
	// pure planner/reducer surface. Only its owning pathv1 package may compose
	// that surface internally. Scanning every production Go file outside the
	// owner catches aliases, wrappers, new subpackages, and generated registries
	// without a fragile list of active directories or import roots.
	violations, err := scanProductionSources(root, topLevelNames(dormantPureDeclarations))
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("dormant pure path-v1 API escaped its package boundary:\n%s", strings.Join(violations, "\n"))
	}
}

func TestDormancyScannerRejectsNegativeFixtures(t *testing.T) {
	for _, test := range []struct {
		name       string
		wantFile   string
		wantSymbol string
	}{
		{name: "alias", wantFile: "pkg/claude/process/engine/activate.go", wantSymbol: "PlanExclusiveRoute"},
		{name: "wrapper", wantFile: "internal/bridge/bridge.go", wantSymbol: "VerifyExclusiveInput"},
		{name: "subpackage", wantFile: "pkg/claude/agentd/generated/register.go", wantSymbol: "ReduceExclusiveCompletion"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join("testdata", test.name)
			violations, err := scanProductionSources(root, topLevelNames(dormantPureDeclarations))
			if err != nil {
				t.Fatal(err)
			}
			if len(violations) != 1 {
				t.Fatalf("violations = %v, want one", violations)
			}
			if !strings.Contains(violations[0], test.wantFile) || !strings.Contains(violations[0], test.wantSymbol) {
				t.Fatalf("violation = %q, want file %q and symbol %q", violations[0], test.wantFile, test.wantSymbol)
			}
		})
	}
}

func TestDormancyScannerAllowsReleasedExecutionBoundary(t *testing.T) {
	violations, err := scanProductionSources(filepath.Join("testdata", "released"), topLevelNames(dormantPureDeclarations))
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("released path-v1 boundary was rejected: %v", violations)
	}
}

func assertPureFileSurfaceClassified(t *testing.T, root string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(pathV1Dir), "exclusive_pure.go")
	file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := exportedDeclarations(file)
	want := append(slices.Clone(releasedPureFileDeclarations), dormantPureDeclarations...)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("exclusive_pure.go export classification drifted\n got: %v\nwant: %v\nclassify every new export as released or dormant", got, want)
	}
}

func exportedDeclarations(file *ast.File) []string {
	var names []string
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.GenDecl:
			for _, specification := range declaration.Specs {
				switch specification := specification.(type) {
				case *ast.TypeSpec:
					if specification.Name.IsExported() {
						names = append(names, specification.Name.Name)
					}
				case *ast.ValueSpec:
					for _, name := range specification.Names {
						if name.IsExported() {
							names = append(names, name.Name)
						}
					}
				}
			}
		case *ast.FuncDecl:
			if !declaration.Name.IsExported() {
				continue
			}
			name := declaration.Name.Name
			if declaration.Recv != nil {
				name = receiverName(declaration.Recv.List[0].Type) + "." + name
			}
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func receiverName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.StarExpr:
		return "(*" + receiverName(expression.X) + ")"
	case *ast.IndexExpr:
		return receiverName(expression.X)
	case *ast.IndexListExpr:
		return receiverName(expression.X)
	default:
		return fmt.Sprintf("%T", expression)
	}
}

func topLevelNames(declarations []string) map[string]struct{} {
	names := make(map[string]struct{}, len(declarations))
	for _, declaration := range declarations {
		if strings.Contains(declaration, ".") {
			continue
		}
		names[declaration] = struct{}{}
	}
	return names
}

func scanProductionSources(root string, forbidden map[string]struct{}) ([]string, error) {
	var violations []string
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name != root && (entry.Name() == ".git" || entry.Name() == "vendor" || entry.Name() == "node_modules" || entry.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if filepath.ToSlash(filepath.Dir(rel)) == pathV1Dir {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			return err
		}
		violations = append(violations, forbiddenReferences(rel, file, forbidden)...)
		return nil
	})
	slices.Sort(violations)
	return violations, err
}

func forbiddenReferences(name string, file *ast.File, forbidden map[string]struct{}) []string {
	aliases := map[string]struct{}{}
	dotImport := false
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || path != pathV1Import {
			continue
		}
		alias := "pathv1"
		if imported.Name != nil {
			alias = imported.Name.Name
		}
		switch alias {
		case ".":
			dotImport = true
		case "_":
		default:
			aliases[alias] = struct{}{}
		}
	}
	if len(aliases) == 0 && !dotImport {
		return nil
	}

	seen := map[string]struct{}{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			identifier, ok := node.X.(*ast.Ident)
			if !ok {
				return true
			}
			if _, ok := aliases[identifier.Name]; !ok {
				return true
			}
			if _, ok := forbidden[node.Sel.Name]; ok {
				seen[node.Sel.Name] = struct{}{}
			}
		case *ast.Ident:
			if dotImport {
				if _, ok := forbidden[node.Name]; ok {
					seen[node.Name] = struct{}{}
				}
			}
		}
		return true
	})
	if len(seen) == 0 {
		return nil
	}
	symbols := make([]string, 0, len(seen))
	for symbol := range seen {
		symbols = append(symbols, symbol)
	}
	slices.Sort(symbols)
	violations := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		violations = append(violations, fmt.Sprintf("%s references dormant pathv1.%s", name, symbol))
	}
	return violations
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("go.mod not found above test working directory")
		}
		directory = parent
	}
}
