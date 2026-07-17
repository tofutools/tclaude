package pathv1

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

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

const CommandFutureMutation CommandKindV1 = "future_mutation_v1"
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	collectDeclaredCommandKindNames(file, declared)
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
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("locate mutation kind oracle source")
	}
	dir := filepath.Dir(currentFile)
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read path-v1 package: %w", err)
	}

	declared := make(map[string]struct{})
	fset := token.NewFileSet()
	for _, entry := range files {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		collectDeclaredCommandKindNames(file, declared)
	}
	return declared, nil
}

func collectDeclaredCommandKindNames(file *ast.File, declared map[string]struct{}) {
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
			if !isCommandKindType(inheritedType) && !declaresCommandKindByConversion(inheritedValues) {
				continue
			}
			for _, name := range spec.Names {
				declared[name.Name] = struct{}{}
			}
		}
	}
}

func isCommandKindType(expr ast.Expr) bool {
	name, ok := expr.(*ast.Ident)
	return ok && name.Name == "CommandKindV1"
}

func declaresCommandKindByConversion(values []ast.Expr) bool {
	for _, value := range values {
		call, ok := value.(*ast.CallExpr)
		if ok && isCommandKindType(call.Fun) {
			return true
		}
	}
	return false
}
