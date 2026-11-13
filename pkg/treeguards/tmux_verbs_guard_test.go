//ci:whole-tree
//
// This guard parses every production file in the module, so PR CI's package
// selection cannot tell whether a change reached it. The marker runs it on
// every change.

package treeguards

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

// commonDir is package clcommon itself, where those names are used
// unqualified.
const commonDir = "pkg/claude/common/"

type bypass struct {
	pos  token.Pos
	what string
}

// tmuxBypasses returns every way the file at rel (a slash path from the
// module root) reaches tmux around the verbs.
func tmuxBypasses(file *ast.File, rel string) []bypass {
	if hasAnyPrefix(rel, transportFiles) {
		return nil
	}
	commonName, execName := importNames(file)
	inCommon := filepath.ToSlash(filepath.Dir(rel))+"/" == commonDir
	_, shellTextAllowed := shellTextFiles[rel]
	var found []bypass
	report := func(pos token.Pos, what string) { found = append(found, bypass{pos, what}) }
	// isCommon reports an identifier that names package clcommon.
	isCommon := func(x ast.Expr) bool {
		id, ok := x.(*ast.Ident)
		return ok && commonName != "" && id.Name == commonName
	}
	transportMethod := func(name string) bool { return name == "Run" || name == "AttachArgv" }
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if startsTmuxProcess(node, execName) {
				report(node.Pos(), "starts a tmux process; use a tmuxcmd verb (AttachCommand for an interactive client)")
			}
			// Inside package clcommon the transport is called unqualified.
			if id, ok := node.Fun.(*ast.Ident); ok && inCommon && transportCalls[id.Name] {
				report(node.Pos(), "calls "+id.Name+", the raw tmux transport; this file is not part of the transport")
			}
		case *ast.SelectorExpr:
			switch x := node.X.(type) {
			case *ast.Ident:
				if isCommon(x) && transportCalls[node.Sel.Name] {
					report(node.Pos(), "uses "+x.Name+"."+node.Sel.Name+", the raw tmux transport; use a tmuxcmd verb")
				}
				if inCommon && x.Name == "Default" && transportMethod(node.Sel.Name) {
					report(node.Pos(), "calls Default."+node.Sel.Name+", the raw tmux transport; this file is not part of the transport")
				}
			case *ast.SelectorExpr:
				// clcommon.Default.Run / clcommon.Default.AttachArgv
				if isCommon(x.X) && x.Sel.Name == "Default" && transportMethod(node.Sel.Name) {
					report(node.Pos(), "calls "+commonName+".Default."+node.Sel.Name+", the raw tmux transport; use a tmuxcmd verb")
				}
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING || shellTextAllowed {
				return true
			}
			if text, err := strconv.Unquote(node.Value); err == nil && tmuxCommandLine(text) {
				report(node.Pos(), "spells a tmux command line as shell text; use a tmuxcmd verb")
			}
		}
		return true
	})
	return found
}

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
		for _, b := range tmuxBypasses(file, rel) {
			violations = append(violations, rel+":"+strconv.Itoa(fset.Position(b.pos).Line)+": "+b.what)
		}
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, violations, "tmux reached around package tmuxcmd:\n  %s", strings.Join(violations, "\n  "))

	for file := range shellTextFiles {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(file)))
		require.NoError(t, err, "shellTextFiles lists a file that no longer exists: %s", file)
	}
}

// The guard runs the same scanner over small sources here, so a change to
// its rules cannot quietly stop matching what it exists to catch, or start
// flagging what it should leave alone.
func TestGuardRecognisesWhatItBans(t *testing.T) {
	scan := func(rel, src string) []string {
		t.Helper()
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		require.NoError(t, err)
		var lines []string
		for _, b := range tmuxBypasses(file, rel) {
			lines = append(lines, strconv.Itoa(fset.Position(b.pos).Line))
		}
		return lines
	}

	const caller = `package x

import (
	"context"
	osexec "os/exec"

	cc "github.com/tofutools/tclaude/pkg/claude/common"
)

func f(ctx context.Context, other cc.Tmux) {
	_ = osexec.Command("tmux", "kill-server")       // 11
	_ = osexec.CommandContext(ctx, "tmux", "ls")    // 12
	_ = osexec.Command("/usr/bin/tmux", "ls")       // 13
	_ = osexec.Command("git", "status")
	_, _ = cc.Default.Run(ctx, []string{"ls"})      // 15
	_, _ = cc.Default.AttachArgv(nil)               // 16
	_, _ = cc.RunTmuxClient(ctx, nil)               // 17
	_, _ = cc.Default.ListSessions()
	_ = cc.LiveTmux{}
	_ = "tmux -u -L tclaude attach"                 // 20
	_ = "exec tmux -u -L tclaude attach-session"    // 21
	_ = "tmux is required but not installed"
	_ = "run tmux kill-session to stop it"
}
`
	require.Equal(t, []string{"11", "12", "13", "15", "16", "17", "20", "21"},
		scan("pkg/claude/agentd/somewhere.go", caller),
		"aliased imports are followed; prose about tmux and the session listing are left alone")

	require.Empty(t, scan("pkg/claude/common/tmuxcmd/client.go", caller), "the verbs themselves may")
	require.Empty(t, scan("pkg/testharness/tmux_sim.go", caller), "a transport implementation may")
	require.Equal(t, []string{"11", "12", "13", "15", "16", "17"},
		scan("pkg/claude/agentd/dashboard_term.go", caller),
		"a file allowed to spell a command line is still not allowed to run one")

	// Inside package clcommon the transport is called without a qualifier.
	const insideCommon = `package common

import "context"

func f(ctx context.Context) {
	_, _ = Default.Run(ctx, []string{"ls"}) // 6
	_, _ = RunTmuxClient(ctx, nil)          // 7
	_, _ = Default.ListSessions()
}
`
	require.Equal(t, []string{"6", "7"}, scan("pkg/claude/common/new_helper.go", insideCommon))
	require.Empty(t, scan("pkg/claude/common/pane_cwd.go", insideCommon), "an allowlisted transport file")
	require.Empty(t, scan("pkg/claude/session/x.go", insideCommon),
		"an unrelated package's own Default is not clcommon's")
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
