package session

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The tmux client refuses an initial command whose packed argv exceeds
// ~16KB ("command too long", client.c). The launch bootstrap therefore rides
// in a private script — these tests pin the property that matters, AGAINST
// THE PRODUCTION PATH (launchDetachedTmuxSession itself, via a recording
// tmux fake): the tmux argv stays O(1) no matter how large the bootstrap
// command grows (env exports, sandbox rules, worktree paths, launch
// prompts), and the guard trips with an actionable error rather than tmux's
// opaque one.

// launchRecordingTmux is a clcommon.Tmux fake that records every argv it is asked
// to run. new-session invocations succeed (`true`) unless failNewSession is
// set (`false`); everything else succeeds with empty output.
type launchRecordingTmux struct {
	argv            [][]string
	failNewSession  bool
	failServerProbe bool
	resourceEnv     string
	resourceEnvGone bool
	failResourceEnv bool
	serverPID       int
	paneCwd         string
}

func recordedTmuxCommand(args []string) string {
	if len(args) > 1 && args[0] == "-N" {
		return args[1]
	}
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func (r *launchRecordingTmux) Command(args ...string) *exec.Cmd {
	copied := append([]string(nil), args...)
	r.argv = append(r.argv, copied)
	command := recordedTmuxCommand(args)
	if r.failNewSession && command == "new-session" {
		return exec.Command("false")
	}
	if r.failServerProbe && command == "display-message" {
		return exec.Command("false")
	}
	if r.serverPID > 0 && command == "display-message" {
		return exec.Command("printf", "%s\n", strconv.Itoa(r.serverPID))
	}
	if r.resourceEnv != "" && command == "show-environment" {
		return exec.Command("printf", "%s=%s\n", ResourceDelegationDirEnv, r.resourceEnv)
	}
	if r.resourceEnvGone && command == "show-environment" {
		return exec.Command("printf", "-%s\n", ResourceDelegationDirEnv)
	}
	if r.failResourceEnv && command == "show-environment" {
		return exec.Command("false")
	}
	if len(args) > 0 && args[0] == "display-message" &&
		len(args) > 1 && args[len(args)-1] == "#{pane_current_path}" {
		return exec.Command("printf", "%s", r.paneCwd)
	}
	if len(args) > 0 && args[0] == "list-panes" &&
		len(args) > 1 && args[len(args)-1] == "#{pane_pid}" {
		return exec.Command("printf", "123\\n")
	}
	return exec.Command("true")
}

func TestLaunchExternalDelegationRefusesToAutoSpawnTmuxServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMUX", "")
	t.Setenv(ResourceDelegationDirEnv, "/sys/fs/cgroup/system.slice/tclaude-tmux.service")
	rec := &launchRecordingTmux{failServerProbe: true}
	swapTmux(t, rec)

	err := launchDetachedTmuxSession("spwn-external", t.TempDir(), "exec claude")
	require.Error(t, err)
	assert.ErrorContains(t, err, "external tmux runtime tclaude-tmux.service is unavailable")
	assert.Empty(t, rec.newSessions(), "external mode must not let new-session auto-spawn a server")
}

func (r *launchRecordingTmux) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (r *launchRecordingTmux) newSessions() [][]string {
	var out [][]string
	for _, a := range r.argv {
		if recordedTmuxCommand(a) == "new-session" {
			out = append(out, a)
		}
	}
	return out
}

// recordedNewSession is a parsed tmux new-session argv: the tmux options and
// the pane command words that follow them.
type recordedNewSession struct {
	noStart bool              // leading client-level -N
	opts    map[string]string // -d/-s/-c, valueless flags mapped to ""
	pane    []string          // the pane command: `<shell> <script> [markers]`
}

// parseNewSession locates the pane command STRUCTURALLY rather than at a fixed
// argv offset. The offset is environment-dependent: in external-delegation mode
// or when tclaude itself runs inside the tclaude tmux server (which is exactly
// what an agent sandbox does), ExternalTmuxNoStartArgs prepends "-N" and every
// later position shifts by one. Parsing also keeps the option layout itself
// pinned — a dropped or renamed -d/-s/-c fails here instead of silently sliding
// the pane command to a different index.
func parseNewSession(t *testing.T, argv []string) recordedNewSession {
	t.Helper()
	parsed := recordedNewSession{opts: map[string]string{}}
	if len(argv) > 0 && argv[0] == "-N" {
		parsed.noStart = true
		argv = argv[1:]
	}
	require.NotEmpty(t, argv, "empty tmux argv")
	require.Equal(t, "new-session", argv[0], "not a new-session argv")
	rest := argv[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		flag := rest[0]
		switch flag {
		case "-d":
			parsed.opts[flag] = ""
			rest = rest[1:]
		case "-s", "-c":
			require.GreaterOrEqual(t, len(rest), 2, "new-session %s is missing its value", flag)
			parsed.opts[flag] = rest[1]
			rest = rest[2:]
		default:
			t.Fatalf("unrecognized new-session option %q — teach parseNewSession about it", flag)
		}
	}
	for _, flag := range []string{"-d", "-s", "-c"} {
		require.Contains(t, parsed.opts, flag, "new-session must pass %s", flag)
	}
	parsed.pane = rest
	return parsed
}

func swapTmux(t *testing.T, fake clcommon.Tmux) {
	t.Helper()
	prev := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = prev })
}

func TestLaunchArgvIsConstantSizeThroughProductionPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := &launchRecordingTmux{}
	swapTmux(t, rec)
	cwd := t.TempDir()

	smallCmd := "exec claude"
	// ~1.2MB bootstrap — vastly beyond anything a real launch assembles, and
	// two orders of magnitude over tmux's limit if it were inlined.
	hugeCmd := strings.Repeat("export SOME_VAR=some-value; ", 40000) + "exec claude"
	profileMarker := filepath.Join(t.TempDir(), "tclaude-agent-0123456789abcdef.config.toml")

	require.NoError(t, launchDetachedTmuxSession("spwn-small1", cwd, smallCmd))
	require.NoError(t, launchDetachedTmuxSession("spwn-huge01", cwd, hugeCmd, profileMarker))

	launches := rec.newSessions()
	require.Len(t, launches, 2, "each launch runs exactly one new-session")
	smallArgv, hugeArgv := launches[0], launches[1]

	// The command must ride in a script file, never inline: `<shell> <script>`.
	small, huge := parseNewSession(t, smallArgv), parseNewSession(t, hugeArgv)
	assert.Equal(t, cwd, huge.opts["-c"], "launch dir rides as -c, not in the pane command")
	shell := clcommon.BootstrapShellArgv()
	require.Len(t, huge.pane, len(shell)+2, "pane argv must be exactly `<shell> <script> <marker>`")
	require.Len(t, small.pane, len(shell)+1, "a marker-free launch adds no pane words beyond `<shell> <script>`")
	assert.Equal(t, shell, huge.pane[:len(shell)],
		"pane command must run the script under tclaude's pinned bootstrap shell")
	scriptPath := huge.pane[len(shell)]
	assert.Contains(t, scriptPath, "launch-scripts", "script must live in the private launch-scripts dir")
	assert.Equal(t, profileMarker, huge.pane[len(huge.pane)-1],
		"the codex profile marker rides as the trailing argv word")

	// O(1): a 1.2MB bootstrap must not move the tmux argv size (temp-name
	// length jitter aside), and the total stays far under tmux's cap.
	smallBytes, hugeBytes := tmuxArgvBytes(smallArgv), tmuxArgvBytes(hugeArgv)
	if diff := hugeBytes - smallBytes - len(profileMarker) - 1; diff < -16 || diff > 16 {
		t.Fatalf("tmux argv scales with bootstrap size: small=%d huge=%d", smallBytes, hugeBytes)
	}
	assert.Less(t, hugeBytes, 2048, "tmux argv must stay far under the client limit")

	// The script itself carries the full command, deletes itself FIRST, and
	// stays private to the owner. (The fake tmux never runs it, so it is
	// still on disk to inspect.)
	raw, err := os.ReadFile(scriptPath)
	require.NoError(t, err, "read launch script")
	content := string(raw)
	require.Contains(t, content, hugeCmd, "script carries the bootstrap command")
	selfDelete := strings.Index(content, `rm -f -- "$0"`)
	require.GreaterOrEqual(t, selfDelete, 0, "script is missing its self-delete line")
	assert.Less(t, selfDelete, strings.Index(content, "exec claude"), "self-delete must precede the bootstrap")
	info, err := os.Stat(scriptPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "script must be owner-private")
}

func TestOpenCodeCredentialReachesPaneOnlyThroughPrivateBootstrap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TCLAUDE_OPENCODE_SERVER_URL", "http://127.0.0.1:43210")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "private-password")

	rec := &launchRecordingTmux{}
	swapTmux(t, rec)
	cwd := t.TempDir()
	h, err := harness.Resolve(harness.OpenCodeName)
	require.NoError(t, err)
	cmd := h.Spawn.BuildCommand(harness.SpawnSpec{
		ExecutablePath: "/opt/opencode", Cwd: cwd,
		ServerURL: "http://127.0.0.1:43210", SessionID: "ses_test",
		EnvExports: clcommon.BuildEnvExports(nil),
	})
	require.NoError(t, launchDetachedTmuxSession("spwn-opencode", cwd, cmd))
	launches := rec.newSessions()
	require.Len(t, launches, 1)
	argv := strings.Join(launches[0], " ")
	assert.NotContains(t, argv, "private-password")
	assert.NotContains(t, argv, "43210")

	pane := parseNewSession(t, launches[0]).pane
	shell := clcommon.BootstrapShellArgv()
	require.Len(t, pane, len(shell)+1, "pane argv must be exactly `<shell> <script>`")
	require.Equal(t, shell, pane[:len(shell)],
		"pane command must run the script under tclaude's pinned bootstrap shell")
	scriptPath := pane[len(shell)]
	raw, err := os.ReadFile(scriptPath)
	require.NoError(t, err)
	content := string(raw)
	assert.Contains(t, content, "export OPENCODE_SERVER_PASSWORD=private-password")
	assert.Contains(t, content, "opencode attach http://127.0.0.1:43210")
	assert.Contains(t, content, "--session ses_test")
	info, err := os.Stat(scriptPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// A launch from inside the tclaude tmux server (every agent-sandbox run, and
// agentd's own spawns) carries tmux's client-level -N, which shifts every later
// argv position by one. The pane command must still be found, and the launch
// must otherwise be identical — this is the case that made the fixtures above
// fail in an agent sandbox while ordinary CI stayed green (TCL-963).
func TestLaunchArgvCarriesNoStartFlagInsideTclaudeTmuxServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(ResourceDelegationDirEnv, "")
	t.Setenv("TMUX", filepath.Join("/tmp", "tmux-1000", clcommon.TmuxSocketName)+",1,0")
	rec := &launchRecordingTmux{}
	swapTmux(t, rec)
	cwd := t.TempDir()

	require.NoError(t, launchDetachedTmuxSession("spwn-nostrt", cwd, "exec claude"))
	launches := rec.newSessions()
	require.Len(t, launches, 1)

	parsed := parseNewSession(t, launches[0])
	require.True(t, parsed.noStart, "a launch inside the tclaude server must pass -N")
	assert.Equal(t, "spwn-nostrt", parsed.opts["-s"])
	assert.Equal(t, cwd, parsed.opts["-c"])
	shell := clcommon.BootstrapShellArgv()
	require.Len(t, parsed.pane, len(shell)+1, "pane argv must be exactly `<shell> <script>`")
	assert.Equal(t, shell, parsed.pane[:len(shell)],
		"pane command must run the script under tclaude's pinned bootstrap shell")
	assert.Contains(t, parsed.pane[len(shell)], "launch-scripts",
		"script must live in the private launch-scripts dir")
}

func TestLaunchPreflightRejectsOversizedArgvBeforeTmux(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := &launchRecordingTmux{}
	swapTmux(t, rec)

	// A pathological cwd alone can exceed the client limit; the pre-flight
	// must refuse before tmux is ever invoked, naming the sizes.
	hugeCwd := "/" + strings.Repeat("d", tmuxClientArgvLimit)
	err := launchDetachedTmuxSession("spwn-preflt", hugeCwd, "exec claude")
	require.Error(t, err, "expected pre-flight error for oversized argv")
	assert.Contains(t, err.Error(), "command too long", "error must name tmux's failure mode")
	assert.Contains(t, err.Error(), "launch dir", "error must name the offending component")
	assert.Empty(t, rec.newSessions(), "tmux must never be invoked for a refused launch")

	// The refused launch must not leak its script.
	entries, _ := os.ReadDir(filepath.Join(os.Getenv("HOME"), ".tclaude", "data", "launch-scripts"))
	assert.Empty(t, entries, "refused launch leaked a script")
}

func TestLaunchFailureRemovesItsScript(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := &launchRecordingTmux{failNewSession: true}
	swapTmux(t, rec)

	err := launchDetachedTmuxSession("spwn-tmuxfl", t.TempDir(), "exec claude")
	require.Error(t, err, "tmux refusal must surface")
	entries, _ := os.ReadDir(filepath.Join(os.Getenv("HOME"), ".tclaude", "data", "launch-scripts"))
	assert.Empty(t, entries, "failed launch must remove its script (the pane never ran the self-delete)")
}

// Scenario: the launch dies after runNew wrote its session row (here: tmux
// refuses the session). The row this launch created must be rolled back —
// the second bug of the "command too long" incident left such rows as
// zombies carrying a conv-id that never existed. Exercises the REAL runNew
// path, not a helper.
func TestRunNewRollsBackItsSessionRowWhenLaunchFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := &launchRecordingTmux{failNewSession: true}
	swapTmux(t, rec)
	prevCheck := ClaudeAncestorCheck
	ClaudeAncestorCheck = func() bool { return false }
	t.Cleanup(func() { ClaudeAncestorCheck = prevCheck })

	params := &NewParams{Label: "spwn-rollbk", Dir: t.TempDir(), Detached: true}
	err := runNew(params)
	require.Error(t, err, "launch must fail when tmux refuses the session")

	row, lerr := db.LoadSession("spwn-rollbk")
	if lerr != nil {
		require.True(t, errors.Is(lerr, sql.ErrNoRows), "unexpected LoadSession error: %v", lerr)
	}
	assert.Nil(t, row, "failed launch must not leave its session row behind")
}

func TestWriteLaunchScriptSweepsStaleScripts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Seed one stale script (a pane that died before its self-delete ran)
	// and one fresh one; the next launch sweeps only the stale one — and
	// SweepStaleLaunchScripts (the daemon-startup entry) sweeps the same way.
	first, cleanupFirst, err := writeLaunchScript("exec claude")
	require.NoError(t, err)
	defer cleanupFirst()
	stale := time.Now().Add(-launchScriptStaleAfter - time.Minute)
	require.NoError(t, os.Chtimes(first, stale, stale))

	second, cleanupSecond, err := writeLaunchScript("exec claude")
	require.NoError(t, err)
	defer cleanupSecond()

	_, statErr := os.Stat(first)
	assert.True(t, os.IsNotExist(statErr), "stale script not swept: %v", statErr)
	_, statErr = os.Stat(second)
	assert.NoError(t, statErr, "fresh script must survive the sweep")

	// Daemon-startup sweep removes a backdated leftover too.
	require.NoError(t, os.Chtimes(second, stale, stale))
	SweepStaleLaunchScripts()
	_, statErr = os.Stat(second)
	assert.True(t, os.IsNotExist(statErr), "startup sweep must remove stale scripts")
}
