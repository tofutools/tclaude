package copilotfixture

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// MockModel is the model id used throughout the suite. It is deliberately NOT
// a name from Copilot's built-in catalog.
//
// To be precise about what that does and does not affect: the id is still sent
// to the provider VERBATIM as the wire model — BYOK does not rewrite it — so
// model assertions remain exact. What an unrecognized id changes is only the
// agent-side configuration the CLI derives from its catalog (prompt/output
// token limits), where it falls back to documented defaults and logs a catalog
// miss. That fallback is the desirable behavior here: it keeps the fixture
// independent of catalog churn between CLI releases.
const MockModel = "copilotfixture-mock-model"

// RunTimeout bounds a scenario. Generous relative to the ~1s a healthy run
// takes, but far below the ~100s a 429 retry storm would cost — a scenario
// that blows this budget is reporting a real behavior change.
const RunTimeout = 90 * time.Second

// waitDelay bounds how long Wait may block on descendants that inherited the
// output pipes after the context already killed the process group.
const waitDelay = 5 * time.Second

// scrubbedAuthVars are removed from the child environment so a run cannot
// silently acquire GitHub credentials from the developer or CI environment.
//
// COPILOT_PROVIDER_BASE_URL already makes GitHub auth unnecessary, so this is
// not what makes the run work — it is what makes the run PROVE it works
// without credentials. If a future CLI regressed into requiring a token, a
// machine with GITHUB_TOKEN exported would otherwise keep passing.
var scrubbedAuthVars = []string{
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"COPILOT_GITHUB_TOKEN",
	"GITHUB_COPILOT_GITHUB_TOKEN",
	"GITHUB_COPILOT_API_TOKEN",
	"COPILOT_API_TOKEN",
	"COPILOT_TOKEN",
	"GITHUB_COPILOT_TOKEN",
	"GITHUB_PERSONAL_ACCESS_TOKEN",
	"COPILOT_PROVIDER_API_KEY",
	"COPILOT_PROVIDER_BEARER_TOKEN",
	"COPILOT_PROVIDER_GHES_TOKEN",
	"OPENAI_API_KEY",
	"ANTHROPIC_API_KEY",
	"AZURE_OPENAI_API_KEY",
	// A proxy pointed at anything but the mock silently breaks the loopback
	// route: the CLI honours HTTP_PROXY and reports the mock as unreachable.
	"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy",
	"ALL_PROXY", "all_proxy",
}

// WireAPI selects COPILOT_PROVIDER_WIRE_API.
type WireAPI string

const (
	// WireCompletions is the CLI's default: POST {base}/chat/completions with
	// OpenAI chat SSE framing. Model selection is observable here.
	WireCompletions WireAPI = "completions"

	// WireResponses is the OpenAI Responses wire. Reasoning effort is
	// observable ONLY here — on the completions wire the request body carries
	// no effort key at all, so an effort assertion built on completions would
	// silently assert nothing and still pass.
	WireResponses WireAPI = "responses"
)

// RunOptions describes one credential-free invocation.
type RunOptions struct {
	// Root is the disposable temp root. It backs HOME as well as the
	// directories below, which matters more than it looks: COPILOT_CACHE_HOME
	// redirects only Copilot's own package cache, while the bundled
	// Microsoft/DeveloperTools cache still resolves through
	// XDG_CACHE_HOME/HOME. Without redirecting all three a "hermetic" run
	// still writes into the operator's real ~/.cache.
	Root string

	// Home and Cache back COPILOT_HOME / COPILOT_CACHE_HOME. COPILOT_HOME
	// holds config.json, session-store.db, session-state/ and
	// installed-plugins; Cache holds the unpacked platform payload.
	// COPILOT_CACHE_HOME is undocumented but real, and is set alongside
	// XDG_CACHE_HOME rather than instead of it.
	Home  string
	Cache string

	// Wire selects the provider wire API; empty means WireCompletions.
	Wire WireAPI

	// WorkDir is the CLI's working directory, passed via -C.
	WorkDir string

	// BaseURL activates BYOK and points at the mock.
	BaseURL string

	// Prompt runs non-interactively via -p and exits after completion.
	Prompt string

	// SessionID pins a fresh session's UUID (--session-id). ResumeID resumes an
	// existing one (--resume=<id>, the `=` form: the option's value is optional,
	// so a space-separated value would leave it bare and open the picker).
	// They are mutually exclusive.
	SessionID string
	ResumeID  string

	// Model and Effort exercise the spawner's selection flags. Effort is only
	// observable on the responses wire; on the default completions wire the
	// request body carries no effort key at all.
	Model  string
	Effort string
}

// RunResult is one completed invocation.
type RunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	// Events are the parsed --output-format json lines, in order.
	Events []Event
}

// Event is one line of the CLI's JSONL event stream.
type Event struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	ID        string          `json:"id,omitempty"`
	ParentID  string          `json:"parentId,omitempty"`
	Ephemeral bool            `json:"ephemeral,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`

	// Present only on the terminal "result" line, which carries no id/parentId
	// or data and is the natural assertion anchor for a scenario.
	SessionID string          `json:"sessionId,omitempty"`
	ExitCode  *int            `json:"exitCode,omitempty"`
	Usage     json.RawMessage `json:"usage,omitempty"`
}

// Result returns the terminal result event, or false when the CLI died before
// emitting one.
func (r RunResult) Result() (Event, bool) {
	for _, e := range slices.Backward(r.Events) {
		if e.Type == "result" {
			return e, true
		}
	}
	return Event{}, false
}

// EventTypes lists the event types in order, which is what a fixture compares
// rather than the volatile ids and timestamps on each line.
func (r RunResult) EventTypes() []string {
	out := make([]string, 0, len(r.Events))
	for _, e := range r.Events {
		out = append(out, e.Type)
	}
	return out
}

// Dirs is one run's disposable directory set.
type Dirs struct {
	Root    string
	Home    string
	Cache   string
	WorkDir string
}

// NewSandboxDirs creates the disposable directory set under t.TempDir. Each
// scenario gets its own: COPILOT_HOME carries a SQLite session store, so
// sharing one between scenarios would couple them through that database.
func NewSandboxDirs(t *testing.T) Dirs {
	t.Helper()
	root := t.TempDir()
	d := Dirs{
		Root:    root,
		Home:    filepath.Join(root, "copilot-home"),
		Cache:   filepath.Join(root, "cache"),
		WorkDir: filepath.Join(root, "work"),
	}
	for _, dir := range []string{d.Home, d.Cache, d.WorkDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("copilotfixture: mkdir %s: %v", dir, err)
		}
	}
	return d
}

// Run executes the pinned CLI against the mock and parses its event stream.
//
// A non-zero exit is NOT a test failure: the provider-failure scenario asserts
// on exactly that. Only a failure to launch or to produce parsable output is.
func Run(t *testing.T, opts RunOptions) RunResult {
	t.Helper()

	args := []string{
		"-C", opts.WorkDir,
		// Required for non-interactive use: without it the CLI would block on
		// a permission prompt the moment the mock asks for a tool.
		"--allow-all-tools",
		// JSONL is the machine-readable surface; the human text rendering is
		// not a contract.
		"--output-format", "json",
		"--no-color",
		// Keeps the CLI's own diagnostics out of the captured streams.
		"--log-level", "none",
	}
	// The CLI documents --resume as incompatible with --session-id; sending
	// both would silently pick one and make the scenario mean something other
	// than it reads.
	if opts.ResumeID != "" && opts.SessionID != "" {
		t.Fatal("copilotfixture: SessionID and ResumeID are mutually exclusive")
	}
	switch {
	case opts.ResumeID != "":
		args = append(args, "--resume="+opts.ResumeID)
	case opts.SessionID != "":
		args = append(args, "--session-id", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model="+opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort="+opts.Effort)
	}
	// -p last so no earlier option can swallow the prompt value.
	args = append(args, "-p", opts.Prompt)

	ctx, cancel := context.WithTimeout(context.Background(), RunTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "copilot", args...)
	cmd.Dir = opts.WorkDir
	cmd.Env = buildEnv(opts)
	// Copilot spawns helpers (shell tools, indexers). Without WaitDelay a
	// descendant still holding the output pipe keeps Wait blocked long after
	// the context killed the parent, so a scenario could hang past its own
	// timeout. This bounds that window.
	cmd.WaitDelay = waitDelay

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("copilotfixture: run exceeded %s (stderr: %s)", RunTimeout, stderr.String())
	}
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("copilotfixture: launching copilot failed: %v (stderr: %s)", err, stderr.String())
		}
		exitCode = exitErr.ExitCode()
	}

	return RunResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Events:   parseEvents(t, stdout.String()),
	}
}

// RunShell executes a shell command line under the same credential-free,
// hermetic environment Run uses.
//
// This exists so a test can exercise the PRODUCTION spawner
// (copilotSpawner.BuildCommand) end to end instead of trusting Run's
// independently assembled argv. Run's flags and the spawner's flags are two
// separate pieces of code that could drift apart; only executing the real
// spawner output proves the launch tclaude actually performs works.
//
// stdin is /dev/null because the spawner emits the INTERACTIVE `-i` form. With
// no TTY and closed stdin the CLI still runs the prompt to completion and
// exits, which is what makes the production string testable headlessly.
func RunShell(t *testing.T, opts RunOptions, commandLine string) RunResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), RunTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", commandLine)
	cmd.Dir = opts.WorkDir
	cmd.Env = buildEnv(opts)
	cmd.WaitDelay = waitDelay

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("copilotfixture: opening %s: %v", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()
	cmd.Stdin = devNull

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("copilotfixture: spawner run exceeded %s (stderr: %s)", RunTimeout, stderr.String())
	}
	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			t.Fatalf("copilotfixture: launching spawner command failed: %v", runErr)
		}
		exitCode = exitErr.ExitCode()
	}

	// The spawner's `-i` form renders a human summary rather than JSONL, so
	// there are no events to parse here; the provider traffic is the evidence.
	return RunResult{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}
}

func buildEnv(opts RunOptions) []string {
	drop := make(map[string]bool, len(scrubbedAuthVars)+2)
	for _, k := range scrubbedAuthVars {
		drop[k] = true
	}
	// Dropped rather than merely re-appended: a duplicate key in exec.Cmd.Env
	// has platform-dependent precedence, and these two decide where the run
	// writes. Their replacements are appended below.
	drop["HOME"] = true
	drop["XDG_CACHE_HOME"] = true
	// Inherited rather than env -i: the CLI is a Node program that still needs
	// PATH, HOME and friends to start at all. Everything security-relevant is
	// dropped by name, and everything behavior-relevant is set explicitly
	// below, so what remains cannot steer the run.
	env := make([]string, 0, len(os.Environ())+12)
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if ok && drop[key] {
			continue
		}
		if strings.HasPrefix(key, "COPILOT_") {
			// An operator's own Copilot configuration must not leak into a
			// fixture run; every COPILOT_ variable the suite needs is set below.
			continue
		}
		env = append(env, kv)
	}

	wire := opts.Wire
	if wire == "" {
		wire = WireCompletions
	}
	env = append(env,
		// HOME and XDG_CACHE_HOME are redirected together with the two
		// COPILOT_ variables. COPILOT_CACHE_HOME alone is not enough: it
		// captures only copilot/pkg, while the bundled
		// Microsoft/DeveloperTools cache still resolves via
		// XDG_CACHE_HOME then HOME, so a run missing these two writes outside
		// its temp root.
		"HOME="+opts.Root,
		"XDG_CACHE_HOME="+opts.Cache,
		"COPILOT_HOME="+opts.Home,
		"COPILOT_CACHE_HOME="+opts.Cache,
		"COPILOT_PROVIDER_BASE_URL="+opts.BaseURL,
		"COPILOT_PROVIDER_TYPE=openai",
		// Stated explicitly even for the default, so a future change of
		// default shows up as a fixture diff rather than a silent wire switch.
		"COPILOT_PROVIDER_WIRE_API="+string(wire),
		"COPILOT_MODEL="+MockModel,
		// Skips ALL network access: GitHub auth, telemetry, web tools, the
		// GitHub MCP server and auto-update. This is what makes the run
		// hermetic rather than merely unauthenticated.
		"COPILOT_OFFLINE=true",
		"COPILOT_AUTO_UPDATE=false",
		"NO_COLOR=1",
		"CI=1",
		// Loopback must never traverse a proxy even if one is configured
		// system-wide beyond the variables scrubbed above.
		"NO_PROXY=127.0.0.1,localhost,::1",
		"no_proxy=127.0.0.1,localhost,::1",
	)
	return env
}

func parseEvents(t *testing.T, stdout string) []Event {
	t.Helper()
	var events []Event
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	// Event lines embed skill descriptions and tool output and comfortably
	// exceed bufio's default 64 kB line cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("copilotfixture: unparsable event line %q: %v", truncate(line, 200), err)
		}
		events = append(events, e)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("copilotfixture: reading event stream: %v", err)
	}
	return events
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
