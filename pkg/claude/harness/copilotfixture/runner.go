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

	// XDGCache backs XDG_CACHE_HOME, and defaults to Cache when empty.
	//
	// The two are separable because the CLI reads them at different places:
	// COPILOT_CACHE_HOME selects the package cache, while the bundled runtime
	// resolves its Microsoft/DeveloperTools device-id file through
	// XDG_CACHE_HOME (with no macOS branch). A scenario that needs to observe
	// which variable owns which write sets this to its own directory; the
	// wire scenarios, which care about neither, leave it empty.
	XDGCache string

	// OmitCacheOverrides drops COPILOT_CACHE_HOME and XDG_CACHE_HOME from the
	// child environment so the CLI resolves its caches from HOME the way an
	// ordinary launch does.
	//
	// It exists for exactly one question, and it is a question no other
	// scenario can ask: the sandbox baseline claims a PLATFORM-DEPENDENT
	// default layout — the package cache moves to ~/Library/Caches/copilot on
	// macOS while the device-id cache stays XDG-shaped at ~/.cache on every
	// platform — and every other scenario overrides both variables, which is
	// precisely what makes those scenarios portable and what makes them unable
	// to observe the split.
	//
	// The run stays hermetic regardless: HOME is still the disposable root, so
	// the defaults resolve INSIDE it. Dropping the overrides removes the
	// redirection, not the containment.
	OmitCacheOverrides bool

	// Wire selects the provider wire API; empty means WireCompletions.
	Wire WireAPI

	// WorkDir is the CLI's working directory, passed via -C.
	WorkDir string

	// BaseURL activates BYOK and points at the mock. Empty leaves the CLI on
	// its own first-party routing, which only the proxy-capture scenario wants
	// — every other scenario needs the mock to answer.
	BaseURL string

	// ProxyEndpoint routes the run's outbound traffic through a capturing
	// proxy (host:port) instead of the offline/BYOK setup.
	//
	// Setting it changes the run's whole character, which is why it is one
	// switch rather than several: COPILOT_OFFLINE is dropped (an offline run
	// makes no connections, so it would observe nothing), the BYOK provider
	// variables are dropped (they would replace the first-party route this
	// scenario exists to observe), and NO_PROXY is cleared so nothing is
	// exempted from the capture. The run is still credential-free — every
	// token variable is scrubbed exactly as it is for every other scenario.
	ProxyEndpoint string

	// Prompt runs non-interactively via -p and exits after completion.
	Prompt string

	// Interactive renders the prompt with `-i` instead of `-p`.
	//
	// This is the mode tclaude actually spawns, and it is a genuinely different
	// permission surface rather than a cosmetic switch: `--allow-all-tools` is
	// documented as REQUIRED for non-interactive use, so `-p` cannot observe
	// what a launch without it does — the CLI refuses before any tool call.
	// Only `-i` can reach the prompt that a detached agent would deadlock on.
	//
	// Callers get no event stream here: `-i` renders a human TUI transcript,
	// not JSONL. The evidence is the provider traffic (did the tool result come
	// back?) and the process's own exit, which is what PermissionOutcome reads.
	Interactive bool

	// OmitAllowAllTools drops the runner's default `--allow-all-tools`.
	//
	// Every pre-existing scenario wants that flag — it is what stops a mock
	// tool call from blocking on a prompt and turning a fixture into a 90s
	// hang. The permission matrix wants the opposite: the blocking posture IS
	// the measurement, so it must be reachable without editing the runner's
	// defaults for everyone else.
	OmitAllowAllTools bool

	// Stdin, when non-empty, is written to the child's stdin instead of
	// /dev/null. It exists for the in-pane-command measurements: `/allow-all`
	// is only reachable by typing it at a running interactive session, so a
	// scenario that asks whether a slash command can widen a launch-time deny
	// has no other way to ask.
	Stdin string

	// Timeout overrides RunTimeout for this run. A scenario that EXPECTS to
	// block sets a short one: waiting the full 90s proves nothing the first few
	// seconds did not, and multiplied across the matrix it is the difference
	// between a job that runs and a job nobody runs.
	Timeout time.Duration

	// ExtraEnv are KEY=VALUE pairs appended last to the child environment.
	//
	// It exists for the ambient-promotion measurement and is deliberately
	// narrow. buildEnv strips every inherited COPILOT_ variable so an
	// operator's own configuration cannot steer a fixture; that strip is also
	// what makes "does COPILOT_ALLOW_ALL silently promote a launch" untestable
	// by any other means, since the scenario needs the variable present in a
	// run that is otherwise pristine.
	//
	// Only set keys buildEnv does not already write. A duplicate key's
	// precedence in exec.Cmd.Env is platform-dependent, so a collision here
	// would make the scenario mean different things on Linux and macOS.
	ExtraEnv []string

	// AllowTimeout turns "the run did not finish" from a test failure into a
	// recorded outcome.
	//
	// This is the load-bearing switch for the whole permission matrix. The
	// claim under measurement — that a default-posture launch blocks on its
	// first tool call and never completes — can ONLY be observed as a timeout.
	// Without this the runner would report it as infrastructure failure and the
	// scenario could never distinguish "Copilot deadlocked as predicted" from
	// "the fixture broke".
	AllowTimeout bool

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

	// ExtraArgs are appended after the runner's own flags and before the
	// prompt, so a scenario can vary the launch posture rather than have one
	// baked into the runner.
	//
	// The sandbox characterization uses this for `--experimental`,
	// `--allow-all-paths` and `--disallow-temp-dir`. On `--experimental`
	// specifically: it does NOT gate whether the CLI honours its own sandbox
	// settings — it gates only whether the interactive `/sandbox` command is
	// registered. A settings-enabled sandbox applies with no experimental flag
	// anywhere, which TestCopilotNativeSandboxNeedsNoExperimentalFlag measures
	// on the real binary. Stated here because the opposite reading is the
	// natural one from `copilot help sandbox`, and it is the reading that would
	// let a caller conclude a sandbox is off because tclaude passed no flag.
	ExtraArgs []string
}

// RunResult is one completed invocation.
type RunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	// Events are the parsed --output-format json lines, in order.
	Events []Event

	// TimedOut records that the run was killed at its deadline rather than
	// exiting on its own. Only reachable when RunOptions.AllowTimeout is set;
	// otherwise the runner fails the test instead of returning.
	//
	// ExitCode is meaningless when this is true — it is whatever the kill
	// produced — so a scenario must branch on this field first.
	TimedOut bool
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
	Root     string
	Home     string
	Cache    string
	XDGCache string
	WorkDir  string

	// UnresolvedRoot and UnresolvedWorkDir are the same two directories as the
	// process's own environment spells them, BEFORE symlink resolution. On
	// macOS that is /var/folders/… where the resolved pair is
	// /private/var/folders/…; on a Linux CI box the two spellings are equal.
	//
	// They exist so a scenario can hand the store the spelling an operator's
	// shell would actually supply, which is the whole cwd-matching contract
	// (TCL-987). A scenario that wants the unambiguous lab spelling keeps using
	// Root/WorkDir; these are opt-in, and a test that finds them equal to their
	// resolved twins has no alternate spelling to exercise on that platform.
	UnresolvedRoot    string
	UnresolvedWorkDir string
}

// NewSandboxDirs creates the disposable directory set under t.TempDir. Each
// scenario gets its own: COPILOT_HOME carries a SQLite session store, so
// sharing one between scenarios would couple them through that database.
func NewSandboxDirs(t *testing.T) Dirs {
	t.Helper()
	unresolved := t.TempDir()
	root := unresolved
	// Canonicalized because the CLI records its cwd resolved: on macOS t.TempDir
	// hands back /var/folders/… while Copilot writes /private/var/folders/… into
	// workspace.yaml, and every scenario that compares a path it passed in
	// against a path the CLI wrote back would be measuring that symlink instead
	// of the behavior it names.
	//
	// This makes the fixture lab's own paths unambiguous. It does NOT decide the
	// production question of how tclaude matches an operator-supplied cwd
	// against Copilot's resolved spelling — that comparison lives in
	// copilot_convstore.go. The unresolved spelling is kept alongside rather
	// than discarded so the scenario that DOES test it (TCL-987) can hand the
	// store the spelling a shell would supply.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	d := Dirs{
		Root:     root,
		Home:     filepath.Join(root, "copilot-home"),
		Cache:    filepath.Join(root, "cache"),
		XDGCache: filepath.Join(root, "xdg-cache"),
		WorkDir:  filepath.Join(root, "work"),

		UnresolvedRoot:    unresolved,
		UnresolvedWorkDir: filepath.Join(unresolved, "work"),
	}
	for _, dir := range []string{d.Home, d.Cache, d.XDGCache, d.WorkDir} {
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

	args := []string{"-C", opts.WorkDir}
	if !opts.OmitAllowAllTools {
		// Required for non-interactive use: without it the CLI would block on
		// a permission prompt the moment the mock asks for a tool.
		args = append(args, "--allow-all-tools")
	}
	if !opts.Interactive {
		// JSONL is the machine-readable surface; the human text rendering is
		// not a contract. `-i` has no JSONL surface at all, so asking for one
		// there would be asking for a format the mode does not produce.
		args = append(args, "--output-format", "json")
	}
	args = append(args,
		"--no-color",
		// Keeps the CLI's own diagnostics out of the captured streams.
		"--log-level", "none",
	)
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
	args = append(args, opts.ExtraArgs...)
	// The prompt flag goes last so no earlier option can swallow its value.
	promptFlag := "-p"
	if opts.Interactive {
		promptFlag = "-i"
	}
	args = append(args, promptFlag, opts.Prompt)

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = RunTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "copilot", args...)
	cmd.Dir = opts.WorkDir
	cmd.Env = buildEnv(opts)
	if opts.Stdin != "" {
		cmd.Stdin = strings.NewReader(opts.Stdin)
	} else {
		devNull, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatalf("copilotfixture: opening %s: %v", os.DevNull, err)
		}
		t.Cleanup(func() { _ = devNull.Close() })
		cmd.Stdin = devNull
	}
	// Copilot spawns helpers (shell tools, indexers). Without WaitDelay a
	// descendant still holding the output pipe keeps Wait blocked long after
	// the context killed the parent, so a scenario could hang past its own
	// timeout. This bounds that window.
	cmd.WaitDelay = waitDelay

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	timedOut := ctx.Err() != nil
	if timedOut && !opts.AllowTimeout {
		t.Fatalf("copilotfixture: run exceeded %s (stderr: %s)", timeout, stderr.String())
	}
	exitCode := 0
	if err != nil && !timedOut {
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
		TimedOut: timedOut,
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

// buildEnv assembles the child environment. ExtraEnv is applied by the wrapper
// below rather than inside, so it lands after BOTH of the function's exit
// paths — the proxy-capture branch returns early, and a scenario's explicit
// variable must survive that too.
func buildEnv(opts RunOptions) []string {
	return append(baseEnv(opts), opts.ExtraEnv...)
}

func baseEnv(opts RunOptions) []string {
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
	xdgCache := opts.XDGCache
	if xdgCache == "" {
		xdgCache = opts.Cache
	}
	if !opts.OmitCacheOverrides {
		env = append(env,
			"XDG_CACHE_HOME="+xdgCache,
			"COPILOT_CACHE_HOME="+opts.Cache,
		)
	}
	env = append(env,
		// HOME and XDG_CACHE_HOME are redirected together with the two
		// COPILOT_ variables. COPILOT_CACHE_HOME alone is not enough: it
		// captures only copilot/pkg, while the bundled
		// Microsoft/DeveloperTools cache still resolves via
		// XDG_CACHE_HOME then HOME, so a run missing these two writes outside
		// its temp root.
		//
		// HOME is set unconditionally, which is what keeps an
		// OmitCacheOverrides run hermetic: without the two cache overrides the
		// CLI falls back to its platform defaults, and those defaults hang off
		// HOME, so they land inside the disposable root either way.
		"HOME="+opts.Root,
		"COPILOT_HOME="+opts.Home,
		"COPILOT_AUTO_UPDATE=false",
		"NO_COLOR=1",
		"CI=1",
	)
	if opts.ProxyEndpoint != "" {
		// The capture scenario. Every variable omitted here is omitted for a
		// reason the scenario depends on: COPILOT_OFFLINE would make the run
		// dial nothing, and the BYOK provider variables would replace the
		// first-party route this run exists to observe. NO_PROXY is set EMPTY
		// rather than left out, so no destination — including loopback — is
		// exempted from the capture.
		proxy := "http://" + opts.ProxyEndpoint
		return append(env,
			"HTTP_PROXY="+proxy, "http_proxy="+proxy,
			"HTTPS_PROXY="+proxy, "https_proxy="+proxy,
			// ALL_PROXY matters as much as the pair above: the CLI's native
			// runtime prefers it, so a capture that set only HTTP(S)_PROXY
			// would silently route around itself through an ambient proxy and
			// observe nothing. Both casings, because the runtime accepts both.
			"ALL_PROXY="+proxy, "all_proxy="+proxy,
			// Emptied rather than dropped: an inherited exemption list would
			// carve destinations back out of the capture, and an absent
			// NO_PROXY lets the runtime apply its own default exemptions.
			"NO_PROXY=", "no_proxy=",
			// A syntactically well-formed but INVALID token. Without one the
			// CLI refuses locally ("No authentication information found") and
			// exits before opening a single connection, so the capture would
			// observe nothing and the scenario would silently prove nothing.
			// With it the CLI proceeds to its token exchange, which is exactly
			// the traffic this scenario exists to enumerate. It is not a
			// credential: it authenticates nothing and is rejected by design.
			"GITHUB_TOKEN="+invalidCaptureToken,
		)
	}
	return append(env,
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
		// Loopback must never traverse a proxy even if one is configured
		// system-wide beyond the variables scrubbed above.
		"NO_PROXY=127.0.0.1,localhost,::1",
		"no_proxy=127.0.0.1,localhost,::1",
	)
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
