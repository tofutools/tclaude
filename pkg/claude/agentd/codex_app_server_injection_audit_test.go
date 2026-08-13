package agentd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codexAppServerKeystrokeSinkFiles is the reviewed disposition of every
// daemon file that can type into a harness pane. A new sink must either route
// an app-server-selected Codex conversation through its typed control channel,
// or explain why that harness cannot reach it. This is a structural tripwire,
// not a security boundary; its value is making a newly added injection path an
// explicit review decision instead of a silent regression.
var codexAppServerKeystrokeSinkFiles = map[string]string{
	"handlers.go":        "lifecycle commands route selected Codex conversations to typed RPC before pane input",
	"flush.go":           "durable inbox nudges route selected Codex conversations to typed RPC and hold on failure",
	"unread_reminder.go": "unread reminders use the same typed route as their durable inbox nudge",
	"lifecycle.go":       "spawn welcomes route to typed RPC; process exit deliberately remains signal-key driven because app-server cannot end the TUI process",
	"remote_control.go":  "gated by CanRemoteControl; Codex has no remote-control lifecycle command",
	"codex_fast_mode.go": "Codex 0.147 has no stable thread-settings RPC; the exact /fast token remains a TUI command on both drives",
}

func TestEveryKeystrokeSinkIsAccountedForAgainstTheCodexAppServerDrive(t *testing.T) {
	sinks := []string{
		"injectTextAndSubmit",
		"injectBracketedTextAndSubmit",
		"injectTextAndSubmitWithOptions",
		"injectMenuToggle",
		"injectSoftExitTextSerializedBy",
	}
	found := map[string]bool{}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, readErr := os.ReadFile(name)
		require.NoError(t, readErr)
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), name, source, 0)
		require.NoError(t, parseErr)
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if ok && slices.Contains(sinks, ident.Name) {
				found[name] = true
			}
			return true
		})
	}

	for name := range found {
		assert.Contains(t, codexAppServerKeystrokeSinkFiles, name,
			"%s types into a pane but has no reviewed Codex app-server disposition", name)
	}
	for name := range codexAppServerKeystrokeSinkFiles {
		assert.Contains(t, found, name,
			"%s no longer contains a known keystroke sink; update the audit disposition", name)
	}
}
