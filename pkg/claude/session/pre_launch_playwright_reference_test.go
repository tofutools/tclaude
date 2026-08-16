package session

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The Playwright reference block (TCL-1042), kept here rather than only in the
// docs so the shell an operator is told to copy is executed by a test instead
// of being prose nobody runs. docs/sandboxing.md quotes this verbatim; if you change
// one, change the other.
//
// The shape is the one the docs argue for: a wrapper earlier on PATH that
// scopes XDG to Playwright alone, rather than exporting XDG_CONFIG_HOME into
// the agent and breaking gh, git, codex and claude along with it.
const playwrightReferenceScript = `pw="$TCLAUDE_PW_HOME"
# Resolve the real binary with our own wrapper directory removed from PATH.
# Blocks re-run on every launch including resume, so by the second launch the
# wrapper may already be first on PATH; a plain lookup would find the wrapper
# and wrap it in itself, an exec loop that hangs with no output. Excluding the
# directory makes a re-run idempotent instead of fatal.
pw_search=""
IFS=: read -ra pw_parts <<< "$PATH"
for pw_entry in "${pw_parts[@]}"; do
  [ "$pw_entry" = "$pw/bin" ] && continue
  pw_search="${pw_search:+$pw_search:}$pw_entry"
done
real="$(PATH="$pw_search" command -v playwright-cli || true)"
if [ -z "$real" ]; then
  echo "playwright-cli is not installed on this host" >&2
  false  # abort the launch; tclaude names this block in the failure
fi
export TCLAUDE_PW_REAL="$real"
mkdir -p "$pw"/{config,cache,data,bin}
# A QUOTED heredoc: nothing is interpolated at write time, so a directory
# containing $, backtick, backslash or quote cannot corrupt the wrapper. The
# wrapper reads both values from its environment at run time instead.
cat > "$pw/bin/playwright-cli" <<'WRAPPER'
#!/bin/bash
XDG_CONFIG_HOME="$TCLAUDE_PW_HOME/config" \
XDG_CACHE_HOME="$TCLAUDE_PW_HOME/cache" \
XDG_DATA_HOME="$TCLAUDE_PW_HOME/data" \
exec "$TCLAUDE_PW_REAL" "$@"
WRAPPER
chmod +x "$pw/bin/playwright-cli"
export PATH="$pw/bin:$PATH"
export PLAYWRIGHT_CLI_WRAPPER_DIR="$pw/bin"
# Playwright's downloaded browser bundles live under $XDG_CACHE_HOME, which we
# just made per-agent — so without this every agent would see an empty browser
# registry and re-download hundreds of MB. Keep the registry shared.
export PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-$HOME/.cache/ms-playwright}"
export PLAYWRIGHT_CLI_SESSION="$(basename "$pw")"
export PLAYWRIGHT_MCP_BROWSER=chrome
export PLAYWRIGHT_MCP_SANDBOX=false
`

func playwrightReferenceBlock() sandboxpolicy.PreLaunchBlock {
	return sandboxpolicy.PreLaunchBlock{
		Name:   "playwright-cli",
		Script: playwrightReferenceScript,
		Exports: []string{
			// Not PATH: it is always set, so the set-ness check would pass even
			// if the block never touched it. The wrapper-dir sentinel is set on
			// the same line of reasoning and is genuinely absent on failure.
			"PLAYWRIGHT_CLI_WRAPPER_DIR", "PLAYWRIGHT_CLI_SESSION",
			"PLAYWRIGHT_MCP_BROWSER", "PLAYWRIGHT_MCP_SANDBOX",
		},
	}
}

// renderReference runs the reference block with a stub playwright-cli on PATH,
// so the block's own logic is exercised on any host — the real binary is only
// needed by the opt-in smoke test below.
func renderReference(t *testing.T, agentDir string) string {
	t.Helper()
	stub := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stub, "playwright-cli"),
		[]byte("#!/bin/bash\necho \"stub config=$XDG_CONFIG_HOME cache=$XDG_CACHE_HOME args=$*\"\n"), 0o755))
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TCLAUDE_PW_HOME", agentDir)

	rendered, err := renderPreLaunchScript(
		[]sandboxpolicy.PreLaunchBlock{playwrightReferenceBlock()}, true, "/bin/bash")
	require.NoError(t, err)
	return rendered
}

// The whole point of the wrapper: Playwright gets private XDG directories while
// the agent's own configuration is left alone. Exporting XDG globally would
// take gh, git, codex and claude down with it.
func TestPlaywrightReferenceScopesXDGToPlaywrightAlone(t *testing.T) {
	agentDir := filepath.Join(t.TempDir(), "pw-home")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	rendered := renderReference(t, agentDir)
	t.Setenv("XDG_CONFIG_HOME", "/agent-own/config")

	out, code := runRenderedPreLaunch(t, rendered,
		`printf 'ambient=[%s]\n' "$XDG_CONFIG_HOME"; playwright-cli --version`)
	require.Equal(t, 0, code, out)

	assert.Contains(t, out, "ambient=[/agent-own/config]",
		"the agent's own XDG_CONFIG_HOME must survive the block untouched")
	assert.Contains(t, out, "stub config="+filepath.Join(agentDir, "config"),
		"playwright must be the one tool that sees the private XDG dirs")
	assert.Contains(t, out, "cache="+filepath.Join(agentDir, "cache"))
}

// The operator's requirements, asserted rather than described.
func TestPlaywrightReferenceSetsTheDocumentedEnvironment(t *testing.T) {
	agentDir := filepath.Join(t.TempDir(), "spwn-abc123")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	rendered := renderReference(t, agentDir)

	out, code := runRenderedPreLaunch(t, rendered,
		`printf 'session=%s browser=%s sandbox=%s\n' \
		   "$PLAYWRIGHT_CLI_SESSION" "$PLAYWRIGHT_MCP_BROWSER" "$PLAYWRIGHT_MCP_SANDBOX"`)
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "session=spwn-abc123 browser=chrome sandbox=false")
}

// Two agents must not collide. The session id is derived from the per-agent
// directory rather than a PID, so it is both unique between agents and stable
// across a resume of the same agent — a PID would be neither.
func TestPlaywrightReferenceGivesEachAgentItsOwnSession(t *testing.T) {
	sessions := map[string]string{}
	for _, agent := range []string{"spwn-aaa111", "spwn-bbb222"} {
		dir := filepath.Join(t.TempDir(), agent)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		rendered := renderReference(t, dir)
		out, code := runRenderedPreLaunch(t, rendered, `printf '%s\n' "$PLAYWRIGHT_CLI_SESSION"`)
		require.Equal(t, 0, code, out)
		sessions[agent] = strings.TrimSpace(out)
	}
	assert.NotEqual(t, sessions["spwn-aaa111"], sessions["spwn-bbb222"],
		"concurrent agents must not share a Playwright session")
}

// Blocks re-run on every launch, including a resume, so by the second launch
// the wrapper directory is already on PATH. Resolving with that directory
// excluded makes the re-run a no-op instead of wrapping the wrapper in itself
// — which would exec-loop in one process: a hang with no output, on the second
// launch only, which is about the worst shape a bug can have.
func TestPlaywrightReferenceIsIdempotentAcrossResume(t *testing.T) {
	agentDir := filepath.Join(t.TempDir(), "pw-home")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	rendered := renderReference(t, agentDir)

	// Two launches in one shell is exactly what a resume looks like to the
	// block: the first one's PATH export is still in effect for the second.
	out, code := runRenderedPreLaunch(t, rendered+rendered,
		`grep -c "$TCLAUDE_PW_REAL" "$PLAYWRIGHT_CLI_WRAPPER_DIR/playwright-cli"; playwright-cli --version`)
	require.Equal(t, 0, code, out)
	assert.NotContains(t, out, filepath.Join(agentDir, "bin", "playwright-cli")+"\" \"$@\"",
		"the wrapper must never exec itself")
	assert.Contains(t, out, "stub config="+filepath.Join(agentDir, "config"),
		"a resumed launch must still reach the real binary through the wrapper")
}

// A missing binary must stop the launch with tclaude's own attribution, not
// merely with the block's echo — the echo would print even if the ERR trap were
// broken, so assert on the framework's message.
func TestPlaywrightReferenceFailsLoudlyWithoutPlaywright(t *testing.T) {
	agentDir := t.TempDir()
	// Keep the system directories reachable so the block fails on the missing
	// binary rather than on a missing mkdir, but put a directory with no
	// playwright-cli first and strip the one this host actually has.
	t.Setenv("TCLAUDE_PW_HOME", agentDir)
	t.Setenv("PATH", stripPlaywrightFromPath(t))

	rendered, err := renderPreLaunchScript(
		[]sandboxpolicy.PreLaunchBlock{playwrightReferenceBlock()}, true, "/bin/bash")
	require.NoError(t, err)

	out, code := runRenderedPreLaunch(t, rendered, `echo HARNESS_STARTED`)
	assert.Equal(t, preLaunchFailExitCode, code)
	assert.Contains(t, out, "not installed")
	assert.Contains(t, out, "pre-launch block 'playwright-cli' failed",
		"tclaude's ERR trap must attribute the failure, not just the block's own echo")
	assert.NotContains(t, out, "HARNESS_STARTED")
}

// stripPlaywrightFromPath returns PATH with every directory containing a
// playwright-cli removed, so the missing-binary path is exercised on a host
// that has one installed.
func stripPlaywrightFromPath(t *testing.T) string {
	t.Helper()
	kept := []string{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "playwright-cli")); err == nil {
			continue
		}
		kept = append(kept, dir)
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

// Opt-in end-to-end smoke against the real binary: render a local HTML file and
// check the PNG comes out at the requested size. Off by default — it drives a
// real browser, which is far too slow and network-dependent for CI, the same
// reason the dashboard's visual smoke is gated behind TCLAUDE_DASHSNAP.
//
//	TCLAUDE_PLAYWRIGHT_SMOKE=1 go test ./pkg/claude/session/ -run TestPlaywrightReferenceRendersAPNG -v
func TestPlaywrightReferenceRendersAPNG(t *testing.T) {
	if os.Getenv("TCLAUDE_PLAYWRIGHT_SMOKE") == "" {
		t.Skip("set TCLAUDE_PLAYWRIGHT_SMOKE=1 to drive a real browser")
	}
	if _, err := exec.LookPath("playwright-cli"); err != nil {
		t.Skip("playwright-cli is not installed")
	}
	agentDir := filepath.Join(t.TempDir(), "spwn-smoke")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, "page.html"),
		[]byte("<!doctype html><title>t</title><body style='margin:0'>"+
			"<div style='width:400px;height:300px;background:#3fb950'></div>"), 0o644))
	shot := filepath.Join(work, "shot.png")

	// playwright-cli blocks the file: protocol unless the config sets
	// allowUnrestrictedFileAccess, and that config is only found at
	// ./.playwright/cli.config.json relative to cwd — i.e. inside the agent's
	// repo. Serving the same local file over loopback renders it just as well
	// without writing into the repo or handing the browser unrestricted read
	// access to the filesystem.
	srv := httptest.NewServer(http.FileServer(http.Dir(work)))
	t.Cleanup(srv.Close)
	page := srv.URL + "/page.html"

	t.Setenv("TCLAUDE_PW_HOME", agentDir)
	rendered, err := renderPreLaunchScript(
		[]sandboxpolicy.PreLaunchBlock{playwrightReferenceBlock()}, true, "/bin/bash")
	require.NoError(t, err)

	// The real CLI is session-oriented: open, size, capture, close. The session
	// is the one the block derived, so this is also the concurrency contract —
	// a second agent runs the identical sequence against its own session.
	out, code := runRenderedPreLaunch(t, rendered,
		// cd first: playwright-cli drops session artifacts in the working
		// directory, and under `go test` that is the package directory — it
		// would litter the repo, which is also the opposite of keeping an
		// agent's generated output inside its own workspace.
		`cd `+clcommon.ShellQuoteArg(work)+`
		 playwright-cli open `+clcommon.ShellQuoteArg(page)+` || echo "SMOKE_FAILED:open:$?"
		 playwright-cli resize 400 300 || echo "SMOKE_FAILED:resize:$?"
		 playwright-cli screenshot --filename `+clcommon.ShellQuoteArg(shot)+` || echo "SMOKE_FAILED:shot:$?"
		 playwright-cli close || echo "SMOKE_FAILED:close:$?"`)
	require.Equal(t, 0, code, out)
	require.NotContains(t, out, "SMOKE_FAILED", out)

	info, err := os.Stat(shot)
	require.NoError(t, err, "the screenshot must exist: %s", out)
	assert.Greater(t, info.Size(), int64(0))

	width, height := pngDimensions(t, shot)
	assert.Equal(t, 400, width)
	assert.Equal(t, 300, height)

}

// pngDimensions reads width/height straight from the IHDR chunk, so the smoke
// test needs no image dependency to prove the render actually produced pixels
// at the requested size.
func pngDimensions(t *testing.T, path string) (int, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Greater(t, len(raw), 24, "too short to be a PNG")
	require.Equal(t, "\x89PNG\r\n\x1a\n", string(raw[:8]), "not a PNG")
	be := func(b []byte) int {
		return int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	}
	return be(raw[16:20]), be(raw[20:24])
}

// The concurrency requirement, against real browsers: two agents with distinct
// per-agent directories must each drive their own Playwright session at the
// same time without colliding. This is what the session id derived from the
// agent directory is for — with a shared session the second agent would steer
// the first one's browser.
//
// Same opt-in gate as the single-agent smoke.
func TestPlaywrightReferenceTwoAgentsRenderConcurrently(t *testing.T) {
	if os.Getenv("TCLAUDE_PLAYWRIGHT_SMOKE") == "" {
		t.Skip("set TCLAUDE_PLAYWRIGHT_SMOKE=1 to drive real browsers")
	}
	if _, err := exec.LookPath("playwright-cli"); err != nil {
		t.Skip("playwright-cli is not installed")
	}

	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, "page.html"),
		[]byte("<!doctype html><title>t</title><body style='margin:0'>"+
			"<div style='width:640px;height:480px;background:#58a6ff'></div>"), 0o644))
	srv := httptest.NewServer(http.FileServer(http.Dir(work)))
	t.Cleanup(srv.Close)

	// Rendered outside the goroutines because t.Setenv is not safe to race on,
	// and each agent needs a different TCLAUDE_PW_HOME baked into its fragment.
	type agent struct {
		name     string
		rendered string
		shot     string
	}
	agents := make([]*agent, 0, 2)
	for _, name := range []string{"spwn-conc-a", "spwn-conc-b"} {
		dir := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		rendered, err := renderPreLaunchScript(
			[]sandboxpolicy.PreLaunchBlock{playwrightReferenceBlock()}, true, "/bin/bash")
		require.NoError(t, err)
		// Set the per-agent directory inside the fragment rather than through
		// the process environment: two agents have two different values, and a
		// single test process has only one environment. In production each
		// pane carries its own, which is exactly what this reproduces.
		agents = append(agents, &agent{
			name:     name,
			rendered: "export TCLAUDE_PW_HOME=" + clcommon.ShellQuoteArg(dir) + "\n" + rendered,
			shot:     filepath.Join(work, name+".png"),
		})
	}

	// runRenderedPreLaunch can t.Skip (no bash) or require — both Goexit, which
	// is illegal off the test goroutine and would surface as a confusing
	// missing-output failure. Run one trivially here so any such exit happens
	// on this goroutine, before any are spawned.
	if _, code := runRenderedPreLaunch(t, "", "true"); code != 0 {
		t.Fatalf("baseline fragment failed with %d", code)
	}

	var wg sync.WaitGroup
	outs := make([]string, len(agents))
	codes := make([]int, len(agents))
	for i, a := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], codes[i] = runRenderedPreLaunch(t, a.rendered,
				`cd `+clcommon.ShellQuoteArg(work)+`
				 playwright-cli open `+clcommon.ShellQuoteArg(srv.URL+"/page.html")+` || echo "SMOKE_FAILED:open:$?"
				 playwright-cli resize 640 480 || echo "SMOKE_FAILED:resize:$?"
				 playwright-cli screenshot --filename `+clcommon.ShellQuoteArg(a.shot)+` || echo "SMOKE_FAILED:shot:$?"
				 printf 'SESSION=%s\n' "$PLAYWRIGHT_CLI_SESSION"
				 playwright-cli close || echo "SMOKE_FAILED:close:$?"`)
		}()
	}
	wg.Wait()

	for i, a := range agents {
		require.Equal(t, 0, codes[i], "%s: %s", a.name, outs[i])
		require.NotContains(t, outs[i], "SMOKE_FAILED", "%s: %s", a.name, outs[i])
		assert.Contains(t, outs[i], "SESSION="+a.name,
			"each agent must drive the session its own directory names")

		width, height := pngDimensions(t, a.shot)
		assert.Equal(t, 640, width, "%s produced a wrong-sized render", a.name)
		assert.Equal(t, 480, height, "%s produced a wrong-sized render", a.name)
	}
}

// The docs quote the reference block and the tests execute it. That is only
// worth claiming if something fails when they diverge — otherwise "executed by
// a test, so it cannot rot" is exactly the kind of assurance that quietly stops
// being true. Compare them rather than trusting a comment telling the next
// person to update both.
func TestPlaywrightReferenceDocsMatchTheExecutedScript(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "sandboxing.md"))
	require.NoError(t, err)
	doc := string(raw)

	heading := "### Reference: scoping `playwright-cli` to one agent"
	start := strings.Index(doc, heading)
	require.GreaterOrEqual(t, start, 0, "the reference section must exist in docs/sandboxing.md")

	fence := "```bash\n"
	open := strings.Index(doc[start:], fence)
	require.GreaterOrEqual(t, open, 0, "the reference section must carry a bash block")
	body := doc[start+open+len(fence):]
	end := strings.Index(body, "```")
	require.GreaterOrEqual(t, end, 0, "the bash block must be closed")

	assert.Equal(t, playwrightReferenceScript, body[:end],
		"docs/sandboxing.md and playwrightReferenceScript have drifted; the docs quote a "+
			"block no test executes")
}
