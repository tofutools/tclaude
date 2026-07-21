package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Claude Code cannot express an allowlist-shaped read policy: sandbox.filesystem
// offers only allowRead/denyRead/allowWrite/denyWrite (+ two managed-tier keys),
// so read always resolves denylist-shaped. TCL-609 requires a typed refusal
// rather than a launch that silently keeps today's broad baseline.
func TestClaudeRejectsMinimalReadBaselineWithTypedCapabilityError(t *testing.T) {
	for _, mode := range []string{ClaudeSandboxOn, ClaudeSandboxInherit, ClaudeSandboxOff} {
		err := ValidateSandboxReadBaseline(DefaultName, mode, sandboxpolicy.ReadBaselineMinimal)
		require.Error(t, err, "mode %q", mode)
		var capErr *SandboxCapabilityError
		require.True(t, errors.As(err, &capErr))
		assert.Equal(t, SandboxCapabilityReadBaseline, capErr.Kind)
		assert.Contains(t, capErr.Message, "denylist-shaped",
			"the refusal must explain WHY, so an operator is not left guessing")
		assert.Contains(t, capErr.Message, "Codex", "and must point at the harness that can do it")
	}
}

func TestCodexAcceptsMinimalReadBaselineOnlyInManagedProfileMode(t *testing.T) {
	require.NoError(t, ValidateSandboxReadBaseline(CodexName, SandboxManagedProfile, sandboxpolicy.ReadBaselineMinimal))
	for _, mode := range []string{SandboxWorkspaceWrite, SandboxReadOnly, SandboxDangerFull} {
		err := ValidateSandboxReadBaseline(CodexName, mode, sandboxpolicy.ReadBaselineMinimal)
		require.Error(t, err, "mode %q", mode)
	}
}

// Omitting the baseline must never trip a capability gate on any harness.
func TestOmittedReadBaselineIsAlwaysAccepted(t *testing.T) {
	for _, h := range []string{DefaultName, CodexName, "some-future-harness"} {
		for _, mode := range []string{"", ClaudeSandboxOn, SandboxManagedProfile, SandboxDangerFull} {
			require.NoError(t, ValidateSandboxReadBaseline(h, mode, sandboxpolicy.ReadBaselineDefault),
				"harness %q mode %q", h, mode)
		}
	}
}

func TestBreakGlassCapabilityRequiresPolicyRenderingModes(t *testing.T) {
	grants := []sandboxpolicy.BreakGlassGrant{{Path: "/home/u/.tclaude/data", Access: sandboxpolicy.AccessRead}}

	require.NoError(t, ValidateSandboxBreakGlass(CodexName, SandboxManagedProfile, grants))
	require.NoError(t, ValidateSandboxBreakGlass(DefaultName, ClaudeSandboxOn, grants))

	// Claude only emits its protected denies under sandbox `on`; in any other
	// mode there is nothing to re-open and no guarantee about the policy.
	err := ValidateSandboxBreakGlass(DefaultName, ClaudeSandboxInherit, grants)
	require.Error(t, err)
	var capErr *SandboxCapabilityError
	require.True(t, errors.As(err, &capErr))
	assert.Equal(t, SandboxCapabilityBreakGlass, capErr.Kind)

	require.Error(t, ValidateSandboxBreakGlass(CodexName, SandboxWorkspaceWrite, grants))
	require.Error(t, ValidateSandboxBreakGlass("some-future-harness", "whatever", grants))

	// No grants is always fine — this is the omitted-field compatibility path.
	require.NoError(t, ValidateSandboxBreakGlass("some-future-harness", "whatever", nil))
}

// Codex resolves an extends-less permission profile to a deny-all filesystem
// baseline, so dropping `extends = ":workspace"` (whose resolved policy makes
// the filesystem root readable) is what actually implements minimal.
func TestCodexMinimalProfileDropsWorkspaceExtendsAndEnumeratesRuntime(t *testing.T) {
	content, err := codexAgentProfileContentForRules("tclaude-agent-test", "/run/agentd.sock", "/home/u/.tclaude/data",
		CodexSandboxRules{
			WriteDirs:    []string{"/home/u/project"},
			ReadBaseline: sandboxpolicy.ReadBaselineMinimal,
		}, sandboxpolicy.NetworkAccessInherit, "linux")
	require.NoError(t, err)

	assert.NotContains(t, content, `extends = ":workspace"`,
		"the whole point of minimal is losing :workspace's readable filesystem root")
	assert.NotContains(t, content, `":root"`, "minimal must not restore a readable root by another spelling")
	// ":minimal" is Codex's purpose-built runtime baseline (/bin /etc /lib
	// /lib64 /sbin /usr). Without it an extends-less profile cannot even exec.
	assert.Contains(t, content, `":minimal" = "read"`)
	assert.Contains(t, content, `":slash_tmp" = "write"`)
	assert.Contains(t, content, `":tmpdir" = "write"`)
	// The launch contract survives: agentd socket readable, private state denied.
	assert.Contains(t, content, `"/run/agentd.sock" = "read"`)
	assert.Contains(t, content, `"/home/u/.tclaude/data" = "none"`)
	assert.Contains(t, content, `"/home/u/project" = "write"`)
}

// Omitting the baseline must reproduce today's profile byte-for-byte.
func TestCodexDefaultBaselineRendersUnchanged(t *testing.T) {
	rules := CodexSandboxRules{WriteDirs: []string{"/home/u/project"}}
	got, err := codexAgentProfileContentForRules("tclaude-agent-test", "/run/agentd.sock", "/home/u/.tclaude/data",
		rules, sandboxpolicy.NetworkAccessInherit, "linux")
	require.NoError(t, err)
	want, err := codexAgentProfileContentForNameAndRulesAndNetworkForOS("tclaude-agent-test", "/run/agentd.sock",
		"/home/u/.tclaude/data", nil, []string{"/home/u/project"}, nil, sandboxpolicy.NetworkAccessInherit, "linux")
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Contains(t, got, `extends = ":workspace"`)
}

// On Codex a deny dominates any narrower grant regardless of declaration order,
// so break-glass only works if the baseline private-state deny is SUPPRESSED
// for exactly the acknowledged path. Leaving it would silently discard the
// operator's decision.
func TestCodexBreakGlassSuppressesCoveredPrivateStateDeny(t *testing.T) {
	content, err := codexAgentProfileContentForRules("tclaude-agent-test", "/run/agentd.sock", "/home/u/.tclaude/data",
		CodexSandboxRules{BreakGlassReadDirs: []string{"/home/u/.tclaude/data"}},
		sandboxpolicy.NetworkAccessInherit, "linux")
	require.NoError(t, err)
	assert.Contains(t, content, `"/home/u/.tclaude/data" = "read"`)
	assert.NotContains(t, content, `"/home/u/.tclaude/data" = "none"`,
		"a surviving deny would dominate and silently void the acknowledged grant")

	// Read must not become write.
	assert.NotContains(t, content, `"/home/u/.tclaude/data" = "write"`)
}

func TestCodexBreakGlassWriteRendersWriteAccess(t *testing.T) {
	content, err := codexAgentProfileContentForRules("tclaude-agent-test", "/run/agentd.sock", "/home/u/.tclaude/data",
		CodexSandboxRules{BreakGlassWriteDirs: []string{"/home/u/.tclaude/data"}},
		sandboxpolicy.NetworkAccessInherit, "linux")
	require.NoError(t, err)
	assert.Contains(t, content, `"/home/u/.tclaude/data" = "write"`)
	assert.NotContains(t, content, `"/home/u/.tclaude/data" = "none"`)
}

// A break-glass rule on an UNRELATED protected root must not suppress the
// private-state deny.
func TestCodexBreakGlassOnOtherRootKeepsPrivateStateDenied(t *testing.T) {
	content, err := codexAgentProfileContentForRules("tclaude-agent-test", "/run/agentd.sock", "/home/u/.tclaude/data",
		CodexSandboxRules{BreakGlassReadDirs: []string{"/home/u/.codex"}},
		sandboxpolicy.NetworkAccessInherit, "linux")
	require.NoError(t, err)
	assert.Contains(t, content, `"/home/u/.codex" = "read"`)
	assert.Contains(t, content, `"/home/u/.tclaude/data" = "none"`)
}

// Host control (the tmux server socket directory) is a strictly more severe
// class than protected state and is NOT reachable through break-glass.
func TestCodexBreakGlassNeverReopensTmuxSocketDirectory(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "/tmp")
	tmuxDir, err := codexTmuxSocketDir()
	require.NoError(t, err)

	content, err := codexAgentProfileContentForRules("tclaude-agent-test", "/run/agentd.sock", "/home/u/.tclaude/data",
		CodexSandboxRules{
			BreakGlassWriteDirs: []string{"/home/u/.tclaude/data"},
			// Even if a rule somehow named the socket dir, the unconditional
			// host-control deny is applied last and wins.
			ReadDirs: []string{tmuxDir},
		}, sandboxpolicy.NetworkAccessInherit, "linux")
	require.NoError(t, err)
	assert.Contains(t, content, `"`+tmuxDir+`" = "none"`)
	assert.NotContains(t, content, `"`+tmuxDir+`" = "read"`)
}

// Claude's documented re-open mechanism is a narrower allowRead inside a
// denyRead region. tclaude additionally suppresses the covered deny so the
// outcome does not depend on Claude's shallowest-first deny ordering.
func TestClaudeBreakGlassEmitsAllowAndDropsCoveredDeny(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	settings := claudeSettingsJSON(SpawnSpec{
		SandboxMode:               ClaudeSandboxOn,
		SandboxBreakGlassReadDirs: []string{home + "/.tclaude/data"},
	})
	require.NotEmpty(t, settings)

	var decoded struct {
		Sandbox struct {
			Filesystem struct {
				AllowRead  []string `json:"allowRead"`
				AllowWrite []string `json:"allowWrite"`
				DenyRead   []string `json:"denyRead"`
				DenyWrite  []string `json:"denyWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	require.NoError(t, json.Unmarshal([]byte(settings), &decoded))

	assert.Contains(t, decoded.Sandbox.Filesystem.AllowRead, home+"/.tclaude/data")
	assert.NotContains(t, decoded.Sandbox.Filesystem.AllowWrite, home+"/.tclaude/data",
		"read must never imply write")
	for _, denied := range decoded.Sandbox.Filesystem.DenyRead {
		assert.NotContains(t, denied, ".tclaude/data",
			"the covered deny must be suppressed or it may re-mask the grant")
	}
	// The protected path the operator did NOT acknowledge stays denied.
	assert.Contains(t, strings.Join(decoded.Sandbox.Filesystem.DenyRead, " "), ".claude/sessions")
	assert.Contains(t, strings.Join(decoded.Sandbox.Filesystem.DenyWrite, " "), ".claude/sessions")
}

func TestClaudeBreakGlassWriteAlsoGrantsRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	settings := claudeSettingsJSON(SpawnSpec{
		SandboxMode:                ClaudeSandboxOn,
		SandboxBreakGlassWriteDirs: []string{home + "/.claude/sessions"},
	})
	var decoded struct {
		Sandbox struct {
			Filesystem struct {
				AllowRead  []string `json:"allowRead"`
				AllowWrite []string `json:"allowWrite"`
				DenyWrite  []string `json:"denyWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	require.NoError(t, json.Unmarshal([]byte(settings), &decoded))
	assert.Contains(t, decoded.Sandbox.Filesystem.AllowWrite, home+"/.claude/sessions")
	// Write implies read here as a consequence of granting WRITE (a tool that
	// cannot read a file cannot usefully rewrite it) — never the reverse.
	assert.Contains(t, decoded.Sandbox.Filesystem.AllowRead, home+"/.claude/sessions")
	for _, denied := range decoded.Sandbox.Filesystem.DenyWrite {
		assert.NotContains(t, denied, ".claude/sessions")
	}
}

// Without break-glass the Claude block must be exactly what it was before.
func TestClaudeSandboxBlockUnchangedWithoutBreakGlass(t *testing.T) {
	want := ClaudeSandboxOnBlock()
	got := claudeSandboxBlockWithBreakGlass(ClaudeSandboxOn, nil)
	assert.Equal(t, want, got)
	assert.Equal(t, want, claudeSandboxBlock(ClaudeSandboxOn))
}

// Containment edge (raised in review): acknowledging a path must never expose
// unrequested siblings. tclaude suppresses a protected deny ONLY when the
// acknowledged grant sits at or above it — i.e. the operator asked for that
// whole root — never to reach one child.
func TestBreakGlassChildGrantDoesNotSuppressParentDeny(t *testing.T) {
	// At-or-above the deny: suppression is correct, because the acknowledged
	// grant already covers everything the deny protected.
	assert.True(t, breakGlassCoversPath([]string{"/home/u/.tclaude/data"}, "/home/u/.tclaude/data"))
	assert.True(t, breakGlassCoversPath([]string{"/home/u/.tclaude"}, "/home/u/.tclaude/data"))
	// Strictly inside the deny: suppression would expose every sibling, so it
	// must NOT happen.
	assert.False(t, breakGlassCoversPath([]string{"/home/u/.tclaude/data/processes"}, "/home/u/.tclaude/data"))
	// An unrelated protected root never suppresses another one's deny.
	assert.False(t, breakGlassCoversPath([]string{"/home/u/.codex"}, "/home/u/.tclaude/data"))
	// A sibling with a shared string prefix is not an ancestor.
	assert.False(t, breakGlassCoversPath([]string{"/home/u/.tclaude/database"}, "/home/u/.tclaude/data"))
}

// Claude: a child grant is reachable natively (deny dirs are applied
// shallowest-first, so the deeper allow re-binds afterwards) while the parent
// deny — and therefore every unrequested sibling — stays in place.
func TestClaudeBreakGlassChildPathReachableWhileSiblingsStayDenied(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	child := home + "/.tclaude/data/processes"

	for _, tc := range []struct {
		name  string
		spec  SpawnSpec
		allow string
	}{
		{"read", SpawnSpec{SandboxMode: ClaudeSandboxOn, SandboxBreakGlassReadDirs: []string{child}}, "allowRead"},
		{"write", SpawnSpec{SandboxMode: ClaudeSandboxOn, SandboxBreakGlassWriteDirs: []string{child}}, "allowWrite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var decoded struct {
				Sandbox struct {
					Filesystem map[string][]string `json:"filesystem"`
				} `json:"sandbox"`
			}
			require.NoError(t, json.Unmarshal([]byte(claudeSettingsJSON(tc.spec)), &decoded))
			fs := decoded.Sandbox.Filesystem

			// The requested child is reachable...
			assert.Contains(t, fs[tc.allow], child)
			// ...and the parent deny is still enforced, so unrequested siblings
			// (db.sqlite, operator_token, …) remain masked.
			assert.Contains(t, fs["denyRead"], tclaudePrivateStateDirTilde,
				"the protected parent must stay denied so siblings are not exposed")
			assert.Contains(t, fs["denyWrite"], tclaudePrivateStateDirTilde)
			// Nothing re-opened the parent itself.
			assert.NotContains(t, fs["allowRead"], home+"/.tclaude/data")
			assert.NotContains(t, fs["allowWrite"], home+"/.tclaude/data")
			// The other protected root was never touched.
			assert.Contains(t, fs["denyRead"], tclaudeClaudeSessionsDirTilde)
		})
	}
}

// Codex has the opposite precedence: a deny masks any narrower grant, so a
// child-path reopen is unrepresentable. Rather than silently discarding the
// acknowledged access (or suppressing the parent deny and exposing every
// sibling), the launch is refused with a typed, actionable error.
func TestCodexBreakGlassChildPathIsTypedCapabilityErrorNotOvergrant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(home+"/.tclaude/data/processes", 0o755))
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)

	for _, access := range []sandboxpolicy.Access{sandboxpolicy.AccessRead, sandboxpolicy.AccessWrite} {
		err := ValidateSandboxBreakGlass(CodexName, SandboxManagedProfile, []sandboxpolicy.BreakGlassGrant{
			{Path: canonicalHome + "/.tclaude/data/processes", Access: access},
		})
		require.Errorf(t, err, "access %s", access)
		var capErr *SandboxCapabilityError
		require.True(t, errors.As(err, &capErr))
		assert.Equal(t, SandboxCapabilityBreakGlass, capErr.Kind)
		assert.Contains(t, capErr.Message, "silently discarded")
		assert.Contains(t, capErr.Message, "expose every sibling",
			"the refusal must say why suppressing the parent deny is not an option")
	}

	// The root itself stays representable — that is the shape Codex CAN do.
	require.NoError(t, ValidateSandboxBreakGlass(CodexName, SandboxManagedProfile,
		[]sandboxpolicy.BreakGlassGrant{{Path: canonicalHome + "/.tclaude/data", Access: sandboxpolicy.AccessWrite}}))
	// So do the protected roots the Codex adapter does not itself deny.
	require.NoError(t, ValidateSandboxBreakGlass(CodexName, SandboxManagedProfile,
		[]sandboxpolicy.BreakGlassGrant{{Path: canonicalHome + "/.codex", Access: sandboxpolicy.AccessRead}}))
}

// Suppressing the private-state deny for an at-or-above grant must not disturb
// any OTHER restriction the profile carries.
func TestCodexBreakGlassSuppressionLeavesOtherDeniesIntact(t *testing.T) {
	content, err := codexAgentProfileContentForRules("tclaude-agent-test", "/run/agentd.sock", "/home/u/.tclaude/data",
		CodexSandboxRules{
			DenyDirs:           []string{"/home/u/secrets"},
			BreakGlassReadDirs: []string{"/home/u/.tclaude/data"},
		}, sandboxpolicy.NetworkAccessInherit, "linux")
	require.NoError(t, err)
	assert.Contains(t, content, `"/home/u/.tclaude/data" = "read"`)
	assert.Contains(t, content, `"/home/u/secrets" = "none"`,
		"suppressing one acknowledged deny must not drop unrelated restrictions")
}
