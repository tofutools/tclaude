package copilotfixture_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// The real-binary evidence behind tclaude's Copilot hook support.
//
// Everything tclaude now claims about Copilot hooks is undocumented runtime
// behavior: that a tclaude-owned drop-in file under COPILOT_HOME fires at all,
// and that registering Claude Code's event names makes Copilot answer in
// Claude Code's payload dialect. Both are load-bearing — the first decides
// where setup writes, the second is why no translator exists — and neither can
// be checked by reading GitHub's docs.
//
// So this scenario runs tclaude's OWN installer against the pinned CLI. The
// only substitution is the command each entry invokes: a recorder that captures
// stdin instead of the tclaude binary, which is not installed in a test
// sandbox. The file's structure — which events, the entry shape, the timeout —
// is whatever the production installer wrote.

// hookRecorder is the stand-in for the tclaude callback. It writes each
// payload to a numbered file (hooks run sequentially, so filename order IS
// firing order) and, deliberately, NOTHING to stdout: Copilot reads hook stdout
// as a control channel, and a stray line there can make the agent keep working.
const hookRecorder = `#!/bin/sh
set -e
seq_file="$CAP_DIR/.seq"
n=$(cat "$seq_file" 2>/dev/null || echo 0)
n=$((n + 1))
printf '%s' "$n" > "$seq_file"
cat > "$CAP_DIR/$(printf '%02d' "$n")-$1.json"
`

// TestCopilotHooksFireFromTheInstalledFile is the whole contract in one run:
// install with the production installer, run a turn, and check that what came
// back matches the committed captures byte-for-byte after sanitization.
func TestCopilotHooksFireFromTheInstalledFile(t *testing.T) {
	requireSmoke(t)

	dirs := copilotfixture.NewSandboxDirs(t)
	capture := filepath.Join(dirs.Root, "captures")
	require.NoError(t, os.MkdirAll(capture, 0o755))

	installed := installHooksWithRecorder(t, dirs, capture)
	// The installer's own output, asserted before it is rewritten: this is
	// what proves the recorded payloads describe the file tclaude really
	// ships, not a hand-tuned variant of it.
	assert.ElementsMatch(t, harness.CopilotHookEvents, installed,
		"the recorded run must exercise exactly the events tclaude installs")

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{ToolCall: &copilotfixture.ToolCall{
			ID:   "call_copilotfixture_hooks",
			Name: "bash",
			Args: `{"command":"echo hookprobe","description":"probe"}`,
		}},
		{Text: "MOCK HOOK TURN"},
	})
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(),
		Prompt:  "run echo hookprobe",
	})
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
	assertCredentialFree(t, mock)

	fired := readCapturedHooks(t, capture, dirs)
	require.NotEmpty(t, fired, "no hook fired: the drop-in file was not loaded")
	got := map[string]json.RawMessage{}
	var firedOrder []string
	for _, e := range fired {
		firedOrder = append(firedOrder, e.Event)
		if _, seen := got[e.Event]; !seen {
			got[e.Event] = e.Payload
		}
	}

	// Every payload the run produced must match its committed capture. The
	// committed set covers a resume run too, which this scenario does not
	// perform, so the comparison is per event rather than whole-stream.
	want := copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect)
	for _, event := range harness.CopilotHookEvents {
		recorded, ok := got[event]
		require.Truef(t, ok, "installed event %s did not fire", event)
		expected, ok := want.Find(event)
		require.Truef(t, ok, "no committed capture for %s", event)
		assert.JSONEq(t, string(expected), string(recorded),
			"Copilot hook contract drift on %s. Review the diff as compatibility "+
				"evidence before re-recording the committed capture.", event)
	}

	// The ordering that forced a change in the status machine: Copilot
	// announces the session AFTER the first prompt.
	assert.Less(t, order(firedOrder, "UserPromptSubmit"), order(firedOrder, "SessionStart"),
		"UserPromptSubmit must precede SessionStart, which is why "+
			"harness.SessionStartAfterPrompt exists")
	assert.Less(t, order(firedOrder, "PostToolUse"), order(firedOrder, "Stop"))
	assert.Less(t, order(firedOrder, "Stop"), order(firedOrder, "SessionEnd"))
}

// installHooksWithRecorder runs the PRODUCTION installer against the sandbox
// COPILOT_HOME, then rewrites each entry's command to the recorder, keeping
// every other field the installer chose. It returns the events found in the
// installed file.
func installHooksWithRecorder(t *testing.T, dirs copilotfixture.Dirs, captureDir string) []string {
	t.Helper()
	t.Setenv(harness.CopilotHomeEnvVar, dirs.Home)

	h, ok := harness.Get(harness.CopilotName)
	require.True(t, ok)
	require.True(t, h.SupportsHooks())
	require.NoError(t, h.Hooks.Install(), "production installer")

	path := h.Hooks.ConfigTarget()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "the installer must have written %s", path)

	var file struct {
		Version int                          `json:"version"`
		Hooks   map[string][]json.RawMessage `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &file))

	recorder := filepath.Join(dirs.Root, "record-hook")
	require.NoError(t, os.WriteFile(recorder, []byte(hookRecorder), 0o755))

	events := make([]string, 0, len(file.Hooks))
	for event, entries := range file.Hooks {
		events = append(events, event)
		require.Lenf(t, entries, 1, "event %s", event)
		var entry map[string]any
		require.NoError(t, json.Unmarshal(entries[0], &entry))
		// The event label rides in as an ARGV argument rather than being read
		// back out of the payload: one recorded event (PermissionRequest)
		// carries no event name at all, so a payload-derived label would be
		// unreliable exactly where the evidence matters most.
		entry["command"] = fmt.Sprintf("CAP_DIR=%s %s %s", captureDir, recorder, event)
		rewritten, err := json.Marshal(entry)
		require.NoError(t, err)
		file.Hooks[event] = []json.RawMessage{rewritten}
	}
	out, err := json.MarshalIndent(file, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, out, 0o600))
	return events
}

var (
	uuidRe      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	isoStampRe  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z`)
	capturedSeq = regexp.MustCompile(`^(\d+)-(.+)\.json$`)
)

// readCapturedHooks reads what the recorder captured and applies exactly the
// normalization the committed captures use: the per-run values (session uuid,
// timestamps, sandbox paths) become placeholders, and everything else — every
// field name, every nesting level, every value shape — stays verbatim.
func readCapturedHooks(t *testing.T, dir string, dirs copilotfixture.Dirs) []capturedHook {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if capturedSeq.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	out := make([]capturedHook, 0, len(names))
	for i, name := range names {
		event := capturedSeq.FindStringSubmatch(name)[2]
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)

		text := string(raw)
		for _, sub := range []struct{ from, to string }{
			{dirs.WorkDir, copilotfixture.HookCwdPlaceholder},
			{dirs.Home, copilotfixture.HookHomePlaceholder},
		} {
			text = strings.ReplaceAll(text, sub.from, sub.to)
		}
		text = uuidRe.ReplaceAllString(text, copilotfixture.HookSessionIDPlaceholder)
		text = isoStampRe.ReplaceAllString(text, copilotfixture.HookTimestampPlaceholder)

		var fields map[string]json.RawMessage
		require.NoErrorf(t, json.Unmarshal([]byte(text), &fields), "capture %s", name)
		// PermissionRequest's timestamp is an epoch-millisecond NUMBER rather
		// than a string, so no textual substitution can reach it.
		if stamp, ok := fields["timestamp"]; ok && !strings.HasPrefix(string(stamp), `"`) {
			fields["timestamp"] = json.RawMessage("0")
		}
		normalized, err := copilotfixture.Marshal(fields)
		require.NoError(t, err)
		out = append(out, capturedHook{Event: event, Payload: json.RawMessage(normalized)})
		t.Logf("hook %02d: %s", i+1, event)
	}
	return out
}

// capturedHook is one recorded invocation, in firing order.
type capturedHook struct {
	Event   string
	Payload json.RawMessage
}

// order returns an event's position in the recorded firing order, or -1 when
// it never fired.
func order(fired []string, event string) int {
	for i, name := range fired {
		if name == event {
			return i
		}
	}
	return -1
}
