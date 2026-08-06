package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func preLaunchSnapshot(blocks ...sandboxpolicy.PreLaunchBlock) *sandboxpolicy.Snapshot {
	snapshot := sandboxpolicy.NewSnapshot(sandboxpolicy.EffectiveProfile{PreLaunch: blocks}, nil)
	return &snapshot
}

// A profile with no blocks must render nothing at all, so every existing launch
// produces the byte-identical command it produced before the feature existed.
func TestRenderPreLaunchScriptEmptyWithoutBlocks(t *testing.T) {
	for _, snapshot := range []*sandboxpolicy.Snapshot{nil, preLaunchSnapshot()} {
		got, err := RenderPreLaunchScript(snapshot)
		require.NoError(t, err)
		assert.Empty(t, got)
	}
}

// Operator bash under dash does not fail, it quietly does something else, so a
// host without bash must refuse the launch rather than run the block wrong.
func TestRenderPreLaunchScriptRefusesWithoutBash(t *testing.T) {
	blocks := []sandboxpolicy.PreLaunchBlock{{Name: "b", Script: "true\n"}}
	_, err := renderPreLaunchScript(blocks, false, "/bin/sh")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no bash was found")
	assert.Contains(t, err.Error(), "/bin/sh")

	// …and a profile with no blocks still launches fine on such a host: the
	// refusal is about the operator's shell, not about the host.
	got, err := renderPreLaunchScript(nil, false, "/bin/sh")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// The rendered fragment is shell, so assert what it DOES by running it, not
// what it looks like. Everything below runs the real thing under real bash.
func runRenderedPreLaunch(t *testing.T, rendered, after string) (stdout string, exitCode int) {
	t.Helper()
	if !clcommon.BootstrapShellIsBash() {
		t.Skip("no bash on this host")
	}
	cmd := exec.Command(clcommon.BootstrapShellPath(), "-p", "-c", rendered+after)
	cmd.Env = append(os.Environ(), "TCLAUDE_PRELAUNCH_TEST=1")
	out, err := cmd.CombinedOutput()
	code := 0
	var exitErr *exec.ExitError
	if err != nil {
		require.ErrorAs(t, err, &exitErr)
		code = exitErr.ExitCode()
	}
	return string(out), code
}

// The load-bearing property: a block's environment must survive into the
// harness command. If blocks ran in a subshell this would silently return the
// ambient value, and the whole feature would do nothing.
func TestPreLaunchEnvironmentReachesTheHarnessCommand(t *testing.T) {
	rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{
		{Name: "first", Script: "export FROM_BLOCK=one\n"},
		{Name: "second", Script: "export SECOND=\"$FROM_BLOCK-two\"", Exports: []string{"SECOND"}},
	}, true, "/bin/bash")
	require.NoError(t, err)

	out, code := runRenderedPreLaunch(t, rendered, `printf '%s|%s\n' "$FROM_BLOCK" "$SECOND"`)
	assert.Equal(t, 0, code)
	assert.Equal(t, "one|one-two\n", out,
		"blocks run in the launching shell, in order, and each sees the previous one's environment")
}

// A block that fails must stop the launch with its NAME in the message. An
// agent whose wrapper silently failed to install looks like a broken tool, and
// the operator debugs the wrong thing.
func TestPreLaunchFailureAbortsTheLaunchAndNamesTheBlock(t *testing.T) {
	rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{
		{Name: "ok-block", Script: "true\n"},
		{Name: "broken-block", Script: "false\n"},
	}, true, "/bin/bash")
	require.NoError(t, err)

	out, code := runRenderedPreLaunch(t, rendered, `echo HARNESS_STARTED`)
	assert.Equal(t, preLaunchFailExitCode, code)
	assert.Contains(t, out, "broken-block")
	assert.NotContains(t, out, "ok-block", "only the block that actually failed is named")
	assert.NotContains(t, out, "HARNESS_STARTED",
		"a failed block must abort before the harness runs, not start a half-configured agent")
}

// `set -e` and the ERR trap are tclaude's, not the harness's. Leaving either
// armed would change the harness command's own shell semantics — a harness
// command whose first pipeline returns non-zero would vanish into the trap.
func TestPreLaunchUnwindsItsShellOptionsBeforeTheHarness(t *testing.T) {
	rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{
		{Name: "b", Script: "true\n"},
	}, true, "/bin/bash")
	require.NoError(t, err)

	out, code := runRenderedPreLaunch(t, rendered,
		`false; echo "survived:$?"; case "$-" in *e*) echo ERREXIT_LEAKED;; esac; echo "trap:[$(trap -p ERR)]"`)
	assert.Equal(t, 0, code, "a non-zero command in the harness command must not abort the pane")
	assert.Contains(t, out, "survived:1")
	assert.NotContains(t, out, "ERREXIT_LEAKED")
	assert.Contains(t, out, "trap:[]", "the ERR trap must be cleared before the harness command")
	assert.NotContains(t, out, "tclaude_pre_launch_block",
		"the bookkeeping variable must not be left in the harness's environment")
}

// A block need not end in a newline. Without one, its last line would run into
// the next block's bookkeeping assignment and both would be mangled.
func TestPreLaunchTerminatesBlocksWithoutTrailingNewlines(t *testing.T) {
	rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{
		{Name: "no-newline", Script: "export A=1"},
		{Name: "next", Script: "export B=2"},
	}, true, "/bin/bash")
	require.NoError(t, err)
	out, code := runRenderedPreLaunch(t, rendered, `printf '%s%s\n' "$A" "$B"`)
	assert.Equal(t, 0, code)
	assert.Equal(t, "12\n", out)
}

// The motivating case, end to end as shell: a wrapper earlier on PATH that
// scopes XDG to one tool, instead of redirecting XDG globally and breaking
// every other XDG-aware tool in the agent (gh, git, codex, claude).
func TestPreLaunchSupportsTheWrapperOnPathPattern(t *testing.T) {
	dir := t.TempDir()
	rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{{
		Name: "playwright",
		Script: "pw_home=" + dir + "/pw\n" +
			"mkdir -p \"$pw_home\"/{config,cache,data} \"$pw_home/bin\"\n" +
			"cat > \"$pw_home/bin/faketool\" <<'EOF'\n" +
			"#!/bin/bash\necho \"config=$XDG_CONFIG_HOME\"\nEOF\n" +
			"chmod +x \"$pw_home/bin/faketool\"\n" +
			"printf '%s\\n' '#!/bin/bash' 'XDG_CONFIG_HOME=\"$0.d\" exec \"$0.real\" \"$@\"' > \"$pw_home/bin/wrapped\"\n" +
			"cp \"$pw_home/bin/faketool\" \"$pw_home/bin/wrapped.real\"\n" +
			"mkdir -p \"$pw_home/bin/wrapped.d\"\n" +
			"chmod +x \"$pw_home/bin/wrapped\"\n" +
			"export PATH=\"$pw_home/bin:$PATH\"\n",
		Exports: []string{"PATH"},
	}}, true, "/bin/bash")
	require.NoError(t, err)

	out, code := runRenderedPreLaunch(t, rendered,
		`printf 'ambient=[%s]\n' "$XDG_CONFIG_HOME"; wrapped`)
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "ambient=[]",
		"the agent's own XDG_CONFIG_HOME must be untouched; only the wrapped tool is scoped")
	assert.Contains(t, out, "config="+filepath.Join(dir, "pw", "bin", "wrapped.d"))

	// Brace expansion is the concrete reason the bootstrap shell had to be
	// pinned: under dash this directory would be named literally "{config,...}".
	for _, sub := range []string{"config", "cache", "data"} {
		assert.DirExists(t, filepath.Join(dir, "pw", sub))
	}
}

// Every adapter must emit the fragment AFTER EnvExports (or the daemon's
// forwarded environment would overwrite what the block just set — the entire
// point for names like XDG_CONFIG_HOME and PATH) and BEFORE the binary (or the
// harness could not inherit it).
func TestEveryHarnessEmitsPreLaunchBetweenExportsAndBinary(t *testing.T) {
	const marker = "PRE_LAUNCH_MARKER=1; "
	for _, name := range []string{
		harness.DefaultName, harness.CodexName, harness.CopilotName, harness.OpenCodeName,
	} {
		t.Run(name, func(t *testing.T) {
			h, err := harness.Resolve(name)
			require.NoError(t, err)
			spec := harness.SpawnSpec{
				EnvExports:      "export ENV_EXPORTS_MARKER=1; ",
				PreLaunchScript: marker,
				Cwd:             t.TempDir(),
				ServerURL:       "http://127.0.0.1:1", // OpenCode needs an endpoint
				SessionID:       "ses_test",
			}
			cmd := h.Spawn.BuildCommand(spec)

			exportsAt := strings.Index(cmd, "ENV_EXPORTS_MARKER")
			markerAt := strings.Index(cmd, marker)
			binaryAt := strings.Index(cmd, h.Spawn.Binary())
			require.GreaterOrEqual(t, markerAt, 0, "%s dropped the pre-launch script entirely: %s", name, cmd)
			require.GreaterOrEqual(t, binaryAt, 0, "%s: cannot locate the binary in %s", name, cmd)
			assert.Less(t, exportsAt, markerAt, "%s must emit pre-launch AFTER EnvExports", name)
			assert.Less(t, markerAt, binaryAt, "%s must emit pre-launch BEFORE the binary", name)
		})
	}
}

// An empty PreLaunchScript must leave every adapter's command byte-identical,
// so a profile without blocks cannot be affected by this feature at all.
func TestEmptyPreLaunchLeavesEveryHarnessCommandUnchanged(t *testing.T) {
	for _, name := range []string{
		harness.DefaultName, harness.CodexName, harness.CopilotName, harness.OpenCodeName,
	} {
		t.Run(name, func(t *testing.T) {
			h, err := harness.Resolve(name)
			require.NoError(t, err)
			base := harness.SpawnSpec{
				EnvExports: "export X=1; ",
				Cwd:        t.TempDir(),
				ServerURL:  "http://127.0.0.1:1",
				SessionID:  "ses_test",
			}
			withEmpty := base
			withEmpty.PreLaunchScript = ""
			assert.Equal(t, h.Spawn.BuildCommand(base), h.Spawn.BuildCommand(withEmpty))
		})
	}
}

// A helper function is an entirely ordinary shape in 64 KB of allowed script,
// and without `set -E` the ERR trap is not inherited into one: the pane would
// die with a bare exit 1 and NO message, which is precisely the "operator
// debugs the wrong thing" outcome naming the block exists to prevent.
func TestPreLaunchAttributesFailuresInsideFunctions(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"function body", "install_it() { false; }\ninstall_it\n"},
		{"command substitution", "value=$(false)\n"},
		{"nested function", "outer() { inner() { false; }; inner; }\nouter\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{
				{Name: "blk", Script: tc.script},
			}, true, "/bin/bash")
			require.NoError(t, err)
			out, code := runRenderedPreLaunch(t, rendered, `echo HARNESS_STARTED`)
			assert.Equal(t, preLaunchFailExitCode, code)
			assert.Contains(t, out, "pre-launch block 'blk' failed",
				"a failure inside a function must still be attributed to its block")
			assert.NotContains(t, out, "HARNESS_STARTED")
		})
	}
}

// `curl … | tar -x` is how a block that installs something is actually
// written. Without pipefail the pipeline's status is the LAST command's, so a
// failed download does not abort at all and the harness starts with the setup
// silently skipped — worse than aborting, because everything looks fine.
func TestPreLaunchAbortsOnFailedPipeline(t *testing.T) {
	rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{
		{Name: "installer", Script: "false | cat\n"},
	}, true, "/bin/bash")
	require.NoError(t, err)
	out, code := runRenderedPreLaunch(t, rendered, `echo HARNESS_STARTED`)
	assert.Equal(t, preLaunchFailExitCode, code)
	assert.Contains(t, out, "installer")
	assert.NotContains(t, out, "HARNESS_STARTED")
}

// …and every option tclaude turned on must be off again for the harness, or a
// harness command containing a tolerated non-zero pipeline would abort the pane.
func TestPreLaunchUnwindsPipefailAndErrtrace(t *testing.T) {
	rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{
		{Name: "b", Script: "true\n"},
	}, true, "/bin/bash")
	require.NoError(t, err)
	out, code := runRenderedPreLaunch(t, rendered,
		`false | cat; echo "pipeline:$?"; case "$-" in *E*) echo ERRTRACE_LEAKED;; esac; `+
			`case "$(set -o | grep pipefail)" in *on*) echo PIPEFAIL_LEAKED;; esac`)
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "pipeline:0")
	assert.NotContains(t, out, "ERRTRACE_LEAKED")
	assert.NotContains(t, out, "PIPEFAIL_LEAKED")
}

// A block's `exports` list is a promise that the block defines those names.
// The failure it guards against is not the block erroring — that the ERR trap
// already catches — but the block succeeding and still leaving the variable
// undefined, because a path moved or a conditional branch was not taken. Left
// unchecked that starts an agent whose tool is subtly misconfigured, and the
// blame lands on the tool rather than on the block.
func TestPreLaunchDeclaredExportMustActuallyBeSet(t *testing.T) {
	rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{
		{
			Name:    "forgetful",
			Script:  "if false; then export PLAYWRIGHT_CLI_SESSION=never; fi\n",
			Exports: []string{"PLAYWRIGHT_CLI_SESSION"},
		},
	}, true, "/bin/bash")
	require.NoError(t, err)

	out, code := runRenderedPreLaunch(t, rendered, `echo HARNESS_STARTED`)
	assert.Equal(t, preLaunchFailExitCode, code)
	assert.Contains(t, out, "forgetful", "the message must name the block that broke its promise")
	assert.Contains(t, out, "PLAYWRIGHT_CLI_SESSION", "…and the name it failed to set")
	assert.NotContains(t, out, "HARNESS_STARTED")
}

// The check must not be a presence-of-non-empty-value check: exporting a
// deliberately empty value is a legitimate way to say "defined, but blank",
// and failing that would make the declaration unusable for it.
func TestPreLaunchDeclaredExportAcceptsAnEmptyValue(t *testing.T) {
	rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{
		{Name: "blank", Script: "export TCL_BLANK=\n", Exports: []string{"TCL_BLANK"}},
	}, true, "/bin/bash")
	require.NoError(t, err)

	out, code := runRenderedPreLaunch(t, rendered, `echo "started:[$TCL_BLANK]"`)
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "started:[]")
}

// Each block is checked against its own promises, right after it runs, so a
// later block cannot accidentally satisfy an earlier one's declaration.
func TestPreLaunchExportsAreCheckedPerBlockInOrder(t *testing.T) {
	rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{
		{Name: "first", Script: "true\n", Exports: []string{"TCL_LATE"}},
		{Name: "second", Script: "export TCL_LATE=1\n"},
	}, true, "/bin/bash")
	require.NoError(t, err)

	out, code := runRenderedPreLaunch(t, rendered, `echo HARNESS_STARTED`)
	assert.Equal(t, preLaunchFailExitCode, code)
	assert.Contains(t, out, "first",
		"a name set only by a later block must not satisfy an earlier block's declaration")
	assert.NotContains(t, out, "HARNESS_STARTED")
}

// A satisfied declaration must be invisible: no bookkeeping left behind and the
// harness command untouched.
func TestPreLaunchSatisfiedExportsLeaveNothingBehind(t *testing.T) {
	rendered, err := renderPreLaunchScript([]sandboxpolicy.PreLaunchBlock{
		{Name: "good", Script: "export TCL_OK=yes\n", Exports: []string{"TCL_OK"}},
	}, true, "/bin/bash")
	require.NoError(t, err)

	out, code := runRenderedPreLaunch(t, rendered, `echo "n=[$n]"; echo "ok=$TCL_OK"`)
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "ok=yes")
	assert.Contains(t, out, "n=[]",
		"the loop variable must stay local to the helper, not leak into the harness environment")
}
