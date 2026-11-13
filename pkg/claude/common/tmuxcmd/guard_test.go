package tmuxcmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The typed verbs are only a seam if nothing goes around them. This test
// reads every non-test Go file in the module and fails on the ways code could
// talk to tmux without them:
//
//   - starting a tmux process itself (exec.Command("tmux", …));
//   - calling the transport directly (clcommon.Default.Run / .AttachArgv,
//     clcommon.RunTmuxClient), which is tmuxcmd's job alone;
//   - building a tmux command line as shell text ("tmux -u -L …").
//
// Each rule has a short list of files that are allowed to, with the reason.
// Adding a file to a list is a design decision: a command that goes around
// the verbs is a command a remote runner will not carry.

const commonImportPath = "github.com/tofutools/tclaude/pkg/claude/common"

// transportFiles may start tmux and call the transport: they are the
// transport and the verbs on top of it.
var transportFiles = []string{
	"pkg/claude/common/tmux.go",
	"pkg/claude/common/tmux_control.go",
	"pkg/claude/common/tmux_route.go",
	"pkg/claude/common/pane_cwd.go",
	"pkg/claude/common/tmuxcmd/",
	"pkg/claude/common/tmuxctl/", // the control-mode connection is a tmux client process
	"pkg/claude/common/tmuxtest/",
	"pkg/testharness/", // the simulator implements the transport
}

// shellTextFiles may spell a tmux command line as shell text.
var shellTextFiles = map[string]string{
	"pkg/claude/agentd/dashboard_term.go": "the command a dashboard terminal's PTY runs: an interactive client, like AttachCommand",
	"pkg/claude/agentd/tui_http.go":       "the command the console prints for the operator to run by hand",
	"pkg/claude/session/exit_callback.go": "the pane-died hook's watchdog: shell text tmux itself runs, on the host tmux is on",
}

// transportCalls are the clcommon names that reach tmux without a verb.
var transportCalls = map[string]bool{"RunTmuxClient": true}

func TestNothingTalksToTmuxAroundTheVerbs(t *testing.T) {
	root := moduleRoot(t)
	var violations []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" || name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		isTransport := hasAnyPrefix(rel, transportFiles)
		commonName, execName := importNames(file)
		report := func(pos token.Pos, what string) {
			violations = append(violations, rel+":"+strconv.Itoa(fset.Position(pos).Line)+": "+what)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if !isTransport && startsTmuxProcess(node, execName) {
					report(node.Pos(), "starts a tmux process; use a tmuxcmd verb (AttachCommand for an interactive client)")
				}
			case *ast.SelectorExpr:
				if isTransport || commonName == "" {
					return true
				}
				if pkg, ok := node.X.(*ast.Ident); ok && pkg.Name == commonName && transportCalls[node.Sel.Name] {
					report(node.Pos(), "uses "+commonName+"."+node.Sel.Name+", the raw tmux transport; use a tmuxcmd verb")
				}
				// clcommon.Default.Run / clcommon.Default.AttachArgv
				if inner, ok := node.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Default" {
					if pkg, ok := inner.X.(*ast.Ident); ok && pkg.Name == commonName &&
						(node.Sel.Name == "Run" || node.Sel.Name == "AttachArgv") {
						report(node.Pos(), "calls "+commonName+".Default."+node.Sel.Name+", the raw tmux transport; use a tmuxcmd verb")
					}
				}
			case *ast.BasicLit:
				if node.Kind != token.STRING || isTransport {
					return true
				}
				if _, allowed := shellTextFiles[rel]; allowed {
					return true
				}
				if text, err := strconv.Unquote(node.Value); err == nil && tmuxCommandLine(text) {
					report(node.Pos(), "spells a tmux command line as shell text; use a tmuxcmd verb")
				}
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, violations, "tmux reached around package tmuxcmd:\n  %s", strings.Join(violations, "\n  "))

	for file := range shellTextFiles {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(file)))
		require.NoError(t, err, "shellTextFiles lists a file that no longer exists: %s", file)
	}
}

// The scanner's own rules, so a change to them cannot quietly stop matching.
func TestGuardRecognisesWhatItBans(t *testing.T) {
	src := `package x

import (
	"context"
	"os/exec"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

func f(ctx context.Context) {
	_ = exec.Command("tmux", "kill-server")
	_ = exec.CommandContext(ctx, "tmux", "ls")
	_ = exec.Command("git", "status")
	_, _ = clcommon.Default.Run(ctx, []string{"kill-server"})
	_, _ = clcommon.Default.ListSessions()
	_, _ = clcommon.RunTmuxClient(ctx, nil)
	_ = clcommon.LiveTmux{}
	_ = "tmux -u -L tclaude attach"
	_ = "tmux is required but not installed"
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
	require.NoError(t, err)
	commonName, execName := importNames(file)
	require.Equal(t, "clcommon", commonName)
	require.Equal(t, "exec", execName)
	var processes, transport, shell int
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if startsTmuxProcess(node, execName) {
				processes++
			}
		case *ast.SelectorExpr:
			if pkg, ok := node.X.(*ast.Ident); ok && pkg.Name == commonName && transportCalls[node.Sel.Name] {
				transport++
			}
			if inner, ok := node.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Default" &&
				(node.Sel.Name == "Run" || node.Sel.Name == "AttachArgv") {
				transport++
			}
		case *ast.BasicLit:
			if text, err := strconv.Unquote(node.Value); err == nil && node.Kind == token.STRING && tmuxCommandLine(text) {
				shell++
			}
		}
		return true
	})
	require.Equal(t, 2, processes, "exec.Command and exec.CommandContext of tmux, not of git")
	require.Equal(t, 2, transport, "Default.Run and RunTmuxClient, not Default.ListSessions or the LiveTmux type")
	require.Equal(t, 1, shell, "a command line, not a sentence about tmux")
}

// startsTmuxProcess reports exec.Command("tmux", …) and
// exec.CommandContext(ctx, "tmux", …).
func startsTmuxProcess(call *ast.CallExpr, execName string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || execName == "" {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != execName {
		return false
	}
	program := 0
	switch sel.Sel.Name {
	case "Command":
	case "CommandContext":
		program = 1
	default:
		return false
	}
	if len(call.Args) <= program {
		return false
	}
	lit, ok := call.Args[program].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	name, err := strconv.Unquote(lit.Value)
	return err == nil && filepath.Base(name) == "tmux"
}

// tmuxCommandLine reports text that is a tmux invocation rather than prose:
// the program name followed by a flag.
func tmuxCommandLine(text string) bool {
	return strings.HasPrefix(text, "tmux -") || strings.Contains(text, " tmux -u ") || strings.Contains(text, " tmux -L ")
}

// importNames returns the names a file imports clcommon and os/exec under.
func importNames(file *ast.File) (commonName, execName string) {
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		name := filepath.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch path {
		case commonImportPath:
			commonName = name
		case "os/exec":
			execName = name
		}
	}
	return commonName, execName
}

func hasAnyPrefix(rel string, prefixes []string) bool {
	for _, p := range prefixes {
		if rel == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(rel, p)) {
			return true
		}
	}
	return false
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "go.mod not found above the test's directory")
		dir = parent
	}
}
