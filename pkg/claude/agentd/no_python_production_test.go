//ci:whole-tree
//
// This guard parses every production file in the module, not only what
// agentd imports, so PR CI's package selection cannot tell whether a
// change reached it. The marker runs it on every change.

package agentd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type productionInterpreterAllowance struct {
	Path   string
	Value  string
	Reason string
}

// Every entry is a reviewed exception to TCL-783's production invariant.
// Match the complete unquoted string literal and explain why agentd can never
// execute it. The list is intentionally empty at introduction.
var productionInterpreterAllowlist = []productionInterpreterAllowance{}

var productionInterpreterPattern = regexp.MustCompile(
	`(?i)(^|[^a-z0-9_])python(?:[0-9.]*)?([^a-z0-9_]|$)|` +
		`\.py([^a-z0-9_]|$)|^#![^\n]*python`,
)

func TestNoPythonInterpreterLiteralInProduction(t *testing.T) {
	root := noPythonModuleRoot(t)
	used := make([]bool, len(productionInterpreterAllowlist))
	for index, allowance := range productionInterpreterAllowlist {
		require.NotEmpty(t, allowance.Path, "allowlist entry %d path", index)
		require.NotEmpty(t, allowance.Value, "allowlist entry %d value", index)
		require.NotEmpty(t, allowance.Reason, "allowlist entry %d reason", index)
	}
	var sources []string
	require.NoError(t, filepath.WalkDir(filepath.Join(root, "pkg"), func(
		path string,
		entry os.DirEntry,
		err error,
	) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".go") &&
			!strings.HasSuffix(path, "_test.go") {
			sources = append(sources, path)
		}
		return nil
	}))
	sources = append(sources, filepath.Join(root, "main.go"))

	for _, path := range sources {
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, path, nil, 0)
		require.NoError(t, err)
		relative, err := filepath.Rel(root, path)
		require.NoError(t, err)
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			require.NoError(t, err)
			if !productionInterpreterPattern.MatchString(value) {
				return true
			}
			if allowedProductionInterpreterLiteral(relative, value, used) {
				return true
			}
			t.Errorf(
				"TCL-783 invariant: agentd production paths must not embed "+
					"a Python interpreter or script literal: %s:%d: %q",
				relative,
				set.Position(literal.Pos()).Line,
				value,
			)
			return true
		})
	}
	for index, allowance := range productionInterpreterAllowlist {
		require.Truef(
			t,
			used[index],
			"unused TCL-783 allowlist entry %q (%s)",
			allowance.Path,
			allowance.Reason,
		)
	}
}

func TestNoPythonShebangInProductSources(t *testing.T) {
	root := noPythonModuleRoot(t)
	require.NoError(t, filepath.WalkDir(filepath.Join(root, "pkg"), func(
		path string,
		entry os.DirEntry,
		err error,
	) error {
		if err != nil || entry.IsDir() || strings.HasSuffix(path, ".go") {
			return err
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		buffer := make([]byte, 256)
		n, readErr := file.Read(buffer)
		closeErr := file.Close()
		if readErr != nil && n == 0 {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		firstLine := strings.SplitN(string(buffer[:n]), "\n", 2)[0]
		if strings.HasPrefix(firstLine, "#!") &&
			productionInterpreterPattern.MatchString(firstLine) {
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			t.Errorf(
				"TCL-783 invariant: agentd product sources must not carry "+
					"a Python shebang: %s",
				relative,
			)
		}
		return nil
	}))
}

func allowedProductionInterpreterLiteral(
	path, value string,
	used []bool,
) bool {
	for index, allowance := range productionInterpreterAllowlist {
		if allowance.Path == path && allowance.Value == value {
			used[index] = true
			return true
		}
	}
	return false
}

func noPythonModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "could not find module root")
		dir = parent
	}
}
