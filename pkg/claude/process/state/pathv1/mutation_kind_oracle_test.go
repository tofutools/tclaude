package pathv1

import (
	"embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

//go:embed *.go
var pathV1SourceFiles embed.FS

// These two sets deliberately classify the declared path-v1 command kinds
// independently of mutationCommandHandlers. A new declaration must be placed
// in exactly one set, and mutation kinds must also have complete dispatch.
var expectedMutationCommandKinds = map[string]CommandKindV1{
	"CommandRoutePaths":                CommandRoutePaths,
	"CommandActivateGeneration":        CommandActivateGeneration,
	"CommandPropagateCandidateClosure": CommandPropagateCandidateClosure,
	"CommandSettleDetachedSink":        CommandSettleDetachedSink,
	"CommandInternDetachmentSet":       CommandInternDetachmentSet,
}

var expectedNonMutationCommandKinds = map[string]CommandKindV1{
	"CommandInitializeRouting": CommandInitializeRouting,
	"CommandPerformAttempt":    CommandPerformAttempt,
	"CommandSettleAttempt":     CommandSettleAttempt,
	"CommandCompleteRun":       CommandCompleteRun,
}

func TestMutationCommandKindCompleteness(t *testing.T) {
	declared, err := declaredCommandKindNames()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCommandKindPartition(declared); err != nil {
		t.Fatal(err)
	}

	expectedHandlers := make(map[CommandKindV1]string, len(expectedMutationCommandKinds))
	for name, kind := range expectedMutationCommandKinds {
		expectedHandlers[kind] = name
	}
	for kind, name := range expectedHandlers {
		handler, ok := mutationCommandHandlers[kind]
		if !ok {
			t.Errorf("declared mutation kind %s (%q) lacks dispatch", name, kind)
			continue
		}
		if handler.validate == nil || handler.replay == nil {
			t.Errorf("declared mutation kind %s (%q) has incomplete validate/replay dispatch", name, kind)
		}
	}
	for kind := range mutationCommandHandlers {
		if _, ok := expectedHandlers[kind]; !ok {
			t.Errorf("mutation dispatch kind %q is absent from the independent expected-kind oracle", kind)
		}
	}
}

func TestMutationCommandKindCompletenessRejectsUnclassifiedDeclaration(t *testing.T) {
	declared, err := declaredCommandKindNames()
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "future.go", `package pathv1

const CommandFutureMutation = CommandRoutePaths + "_future"
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	collectDeclaredCommandKindNames([]*ast.File{file}, declared)
	if _, ok := declared["CommandFutureMutation"]; !ok {
		t.Fatal("future CommandKindV1 declaration was not discovered")
	}
	if err := validateCommandKindPartition(declared); err == nil || !strings.Contains(err.Error(), "unclassified") {
		t.Fatalf("future declaration oracle error = %v, want unclassified declaration failure", err)
	}
}

func validateCommandKindPartition(declared map[string]struct{}) error {
	classified := make(map[string]struct{}, len(expectedMutationCommandKinds)+len(expectedNonMutationCommandKinds))
	classifiedValues := make(map[CommandKindV1]string, len(expectedMutationCommandKinds)+len(expectedNonMutationCommandKinds))
	for name, kind := range expectedMutationCommandKinds {
		if previous, duplicate := classifiedValues[kind]; duplicate {
			return fmt.Errorf("command kinds %s and %s share value %q", previous, name, kind)
		}
		classified[name] = struct{}{}
		classifiedValues[kind] = name
	}
	for name, kind := range expectedNonMutationCommandKinds {
		if _, duplicate := classified[name]; duplicate {
			return fmt.Errorf("command kind %s is classified as both mutation and non-mutation", name)
		}
		if previous, duplicate := classifiedValues[kind]; duplicate {
			return fmt.Errorf("command kinds %s and %s share value %q", previous, name, kind)
		}
		classified[name] = struct{}{}
		classifiedValues[kind] = name
	}
	for kind, name := range classifiedValues {
		if !kind.Valid() {
			return fmt.Errorf("declared command kind %s (%q) is omitted from CommandKindV1.Valid", name, kind)
		}
	}

	var unclassified, undeclared []string
	for name := range declared {
		if _, ok := classified[name]; !ok {
			unclassified = append(unclassified, name)
		}
	}
	for name := range classified {
		if _, ok := declared[name]; !ok {
			undeclared = append(undeclared, name)
		}
	}
	slices.Sort(unclassified)
	slices.Sort(undeclared)
	if len(unclassified) != 0 || len(undeclared) != 0 {
		return fmt.Errorf("command kind classification mismatch: unclassified declarations=%v, classified but undeclared=%v", unclassified, undeclared)
	}
	return nil
}

func declaredCommandKindNames() (map[string]struct{}, error) {
	entries, err := pathV1SourceFiles.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read embedded path-v1 package: %w", err)
	}

	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		source, err := pathV1SourceFiles.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read embedded %s: %w", entry.Name(), err)
		}
		file, err := parser.ParseFile(fset, entry.Name(), source, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		files = append(files, file)
	}
	declared := make(map[string]struct{})
	collectDeclaredCommandKindNames(files, declared)
	return declared, nil
}

type commandKindConstCandidate struct {
	name         string
	explicitType bool
	value        ast.Expr
}

func collectDeclaredCommandKindNames(files []*ast.File, declared map[string]struct{}) {
	var candidates []commandKindConstCandidate
	for _, file := range files {
		for _, declaration := range file.Decls {
			group, ok := declaration.(*ast.GenDecl)
			if !ok || group.Tok != token.CONST {
				continue
			}
			var inheritedType ast.Expr
			var inheritedValues []ast.Expr
			for _, rawSpec := range group.Specs {
				spec := rawSpec.(*ast.ValueSpec)
				if spec.Type != nil {
					inheritedType = spec.Type
				} else if len(spec.Values) != 0 {
					inheritedType = nil
				}
				if len(spec.Values) != 0 {
					inheritedValues = spec.Values
				}
				for index, name := range spec.Names {
					var value ast.Expr
					if index < len(inheritedValues) {
						value = inheritedValues[index]
					}
					candidates = append(candidates, commandKindConstCandidate{
						name:         name.Name,
						explicitType: isCommandKindType(inheritedType),
						value:        value,
					})
				}
			}
		}
	}

	for changed := true; changed; {
		changed = false
		for _, candidate := range candidates {
			if _, ok := declared[candidate.name]; ok {
				continue
			}
			if !candidate.explicitType && !isCommandKindExpression(candidate.value, declared) {
				continue
			}
			declared[candidate.name] = struct{}{}
			changed = true
		}
	}
}

func isCommandKindType(expr ast.Expr) bool {
	name, ok := expr.(*ast.Ident)
	return ok && name.Name == "CommandKindV1"
}

func isCommandKindExpression(expr ast.Expr, declared map[string]struct{}) bool {
	switch value := expr.(type) {
	case *ast.Ident:
		_, ok := declared[value.Name]
		return ok
	case *ast.ParenExpr:
		return isCommandKindExpression(value.X, declared)
	case *ast.UnaryExpr:
		return isCommandKindExpression(value.X, declared)
	case *ast.BinaryExpr:
		return isCommandKindExpression(value.X, declared) || isCommandKindExpression(value.Y, declared)
	case *ast.CallExpr:
		return isCommandKindType(value.Fun)
	default:
		return false
	}
}
