package session

import "github.com/tofutools/tclaude/pkg/claude/harness"

// ccHookInstaller is the Claude Code HookInstaller — the harness.HookInstaller
// adapter over this package's canonical CC hook logic (InstallHooks /
// CheckHooksInstalled / ClaudeSettingsPath, which write the `hooks` section
// of ~/.claude/settings.json).
//
// The implementation stays here rather than in pkg/claude/harness because
// it predates the seam and has a wide caller base across agentd / setup /
// conv / task that still calls the package functions directly; the adapter
// just exposes those same functions through the harness contract so
// `tclaude setup` can dispatch uniformly per harness. Claude Code needs no
// trust step, so TrustNote is "".
type ccHookInstaller struct{}

func (ccHookInstaller) Install() error                { return InstallHooks() }
func (ccHookInstaller) Check() (bool, []string, bool) { return CheckHooksInstalled() }
func (ccHookInstaller) ConfigTarget() string          { return ClaudeSettingsPath() }
func (ccHookInstaller) TrustNote() string             { return "" }

// init attaches the CC hook installer to the claude harness descriptor.
// harness.init() (which Register's the claude descriptor) runs before this
// init() because this package imports harness, so the descriptor always
// exists by the time we set its Hooks field.
func init() {
	if h, ok := harness.Get(harness.DefaultName); ok {
		h.Hooks = ccHookInstaller{}
	}
}
