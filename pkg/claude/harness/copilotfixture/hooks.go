package copilotfixture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Hook capture support.
//
// The rest of this package records what Copilot sends to a MODEL PROVIDER.
// This file carries the other half of the integration, and the half tclaude's
// live status depends on: what Copilot sends to a HOOK.
//
// The committed captures under testdata/<version>/hooks are verbatim payloads
// recorded from the pinned CLI running credential-free against a scripted mock
// provider, with only the per-run values normalized (see the placeholders
// below). Nothing in them was typed from a changelog or a report. They are the
// evidence for two claims tclaude now depends on and GitHub documents neither
// of: that Copilot accepts Claude Code's event names, and that it answers them
// with Claude Code's payload.
//
// Two captures are NEGATIVE evidence — payloads for events tclaude deliberately
// does NOT install. PreToolUse is excluded because a non-zero hook exit denies
// the user's tool call; PermissionRequest because, as its capture shows at a
// glance, it ignores the dialect entirely and answers a PascalCase registration
// with a camelCase payload. Committing them makes both exclusions checkable
// instead of something a reader has to take on trust.

// Placeholders substituted into every committed capture. Each keeps the SHAPE
// of the value it replaces — a UUID stays a UUID, an ISO-8601 timestamp stays
// one — so a consumer decoding a fixture exercises the same parsing it would
// in production.
const (
	HookSessionIDPlaceholder = "00000000-0000-4000-8000-000000000000"
	HookCwdPlaceholder       = "<WORKDIR>"
	HookHomePlaceholder      = "<COPILOT_HOME>"
	HookTimestampPlaceholder = "2026-01-01T00:00:00.000Z"
)

// HookScenarioClaudeDialect is the recorded scenario: one fresh `-p` turn in
// which the mock asks for a bash tool call and then answers with text,
// followed by a `--resume` run with a second prompt.
const HookScenarioClaudeDialect = "claude-dialect"

// HookEvent is one recorded hook invocation.
type HookEvent struct {
	// Seq is 1-based firing order. It is part of the evidence, not
	// bookkeeping: Copilot fires UserPromptSubmit BEFORE SessionStart, which
	// is the opposite of every other harness, so a consumer that assumed
	// "SessionStart comes first" would be wrong on the very first turn.
	Seq int `json:"seq"`
	// Event is the name of the hook key that fired, taken from the
	// registration rather than from the payload — which matters because one
	// recorded event (PermissionRequest) carries no hook_event_name at all.
	Event string `json:"event"`
	// File is the capture's filename within the scenario directory.
	File string `json:"file"`

	// Payload is the verbatim sanitized JSON the CLI wrote to the hook's
	// stdin, filled in by LoadHookCapture. Kept raw so a consumer decodes the
	// recorded bytes rather than a re-serialization of somebody's struct.
	Payload json.RawMessage `json:"-"`
}

// HookCapture is one scenario's recorded hook stream, in firing order.
type HookCapture struct {
	Scenario string
	Events   []HookEvent
}

// EventNames lists the recorded events in firing order.
func (c HookCapture) EventNames() []string {
	out := make([]string, 0, len(c.Events))
	for _, e := range c.Events {
		out = append(out, e.Event)
	}
	return out
}

// Find returns the first recorded payload for an event name.
func (c HookCapture) Find(event string) (json.RawMessage, bool) {
	for _, e := range c.Events {
		if e.Event == event {
			return e.Payload, true
		}
	}
	return nil, false
}

// FindAll returns every recorded payload for an event name, in order — the
// fresh and resumed SessionStart, for instance.
func (c HookCapture) FindAll(event string) []json.RawMessage {
	var out []json.RawMessage
	for _, e := range c.Events {
		if e.Event == event {
			out = append(out, e.Payload)
		}
	}
	return out
}

// LoadHookCapture reads a committed scenario: its order manifest plus every
// payload it names.
//
// Exported because the consumers that matter live in other packages — the
// decode table test and the daemon flow test both have to run against the
// recorded bytes, not against a copy someone re-typed into a Go literal.
func LoadHookCapture(t *testing.T, scenario string) HookCapture {
	t.Helper()
	dir := HookCaptureDir()
	manifest, err := os.ReadFile(filepath.Join(dir, scenario+".order.json"))
	if err != nil {
		t.Fatalf("copilotfixture: reading hook order manifest for %q: %v", scenario, err)
	}
	var events []HookEvent
	if err := json.Unmarshal(manifest, &events); err != nil {
		t.Fatalf("copilotfixture: parsing hook order manifest for %q: %v", scenario, err)
	}
	for i := range events {
		payload, err := os.ReadFile(filepath.Join(dir, events[i].File))
		if err != nil {
			t.Fatalf("copilotfixture: reading hook capture %s: %v", events[i].File, err)
		}
		if !json.Valid(payload) {
			t.Fatalf("copilotfixture: hook capture %s is not valid JSON", events[i].File)
		}
		events[i].Payload = json.RawMessage(payload)
	}
	if len(events) == 0 {
		t.Fatalf("copilotfixture: hook scenario %q recorded no events", scenario)
	}
	return HookCapture{Scenario: scenario, Events: events}
}

// HookCaptureDir locates the committed captures relative to this source file,
// so a test in another package can load them without knowing the tree layout
// or depending on its own working directory.
func HookCaptureDir() string {
	return filepath.Join(packageDir(), "testdata", PinnedCLIVersion, "hooks")
}

// packageDir resolves this package's source directory. runtime.Caller is the
// only way to do that which survives being called from another package's test
// binary, whose working directory is that package's own directory.
func packageDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Dir(file)
}

// HookPayloadFor returns a recorded payload with the placeholder session id
// and working directory substituted for real ones, so a test can drive the
// production hook path with a payload that matches its own session row.
//
// The substitution is textual and deliberately so: it keeps the recorded field
// names, nesting and value shapes exactly as the CLI wrote them, changing only
// the two values a consumer must own.
func HookPayloadFor(payload json.RawMessage, sessionID, cwd string) json.RawMessage {
	text := string(payload)
	if sessionID != "" {
		text = strings.ReplaceAll(text, HookSessionIDPlaceholder, sessionID)
	}
	if cwd != "" {
		text = strings.ReplaceAll(text, HookCwdPlaceholder, cwd)
	}
	return json.RawMessage(text)
}
