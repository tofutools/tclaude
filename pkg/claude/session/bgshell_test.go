package session

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// bashPostToolUse builds the PostToolUse payload Claude Code fires after a
// Bash call, in the shape verified empirically (TCL-613): run_in_background
// rides in tool_input, and the resulting handle comes back as
// tool_response.backgroundTaskId.
func bashPostToolUse(command string, background bool, taskID string) HookCallbackInput {
	in := map[string]any{"command": command, "description": "d"}
	if background {
		in["run_in_background"] = true
	}
	toolInput, _ := json.Marshal(in)
	resp := map[string]any{
		"stdout": "", "stderr": "", "interrupted": false,
		"isImage": false, "noOutputExpected": false,
	}
	if taskID != "" {
		resp["backgroundTaskId"] = taskID
	}
	toolResponse, _ := json.Marshal(resp)
	return HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      "Bash",
		ToolInput:     toolInput,
		ToolResponse:  toolResponse,
	}
}

func TestBgShellLaunch_RecognisesOnlyBackgroundedBash(t *testing.T) {
	id, command, ok := bgShellLaunch(bashPostToolUse("npm run dev", true, "task-abc"))
	require.True(t, ok, "a backgrounded Bash is a launch")
	assert.Equal(t, "task-abc", id)
	assert.Equal(t, "npm run dev", command)

	_, _, ok = bgShellLaunch(bashPostToolUse("ls", false, ""))
	assert.False(t, ok, "a FOREGROUND Bash is not a background shell")

	// Every other tool hook flows through this decoder; none may match.
	notBash := bashPostToolUse("npm run dev", true, "task-abc")
	notBash.ToolName = "Read"
	_, _, ok = bgShellLaunch(notBash)
	assert.False(t, ok, "only the Bash tool launches background shells")

	pre := bashPostToolUse("npm run dev", true, "task-abc")
	pre.HookEventName = "PreToolUse"
	_, _, ok = bgShellLaunch(pre)
	assert.False(t, ok, "PreToolUse fires before the task exists and has no id yet")
}

// The tool_response shape is undocumented. If it changes, the launch is
// still real — degrade to an anon entry rather than dropping the evidence.
func TestBgShellLaunch_DegradesWhenToolResponseIsUnusable(t *testing.T) {
	for name, mutate := range map[string]func(*HookCallbackInput){
		"absent":     func(h *HookCallbackInput) { h.ToolResponse = nil },
		"a string":   func(h *HookCallbackInput) { h.ToolResponse = json.RawMessage(`"stdout text"`) },
		"an array":   func(h *HookCallbackInput) { h.ToolResponse = json.RawMessage(`[{"a":1}]`) },
		"malformed":  func(h *HookCallbackInput) { h.ToolResponse = json.RawMessage(`{not json`) },
		"renamed id": func(h *HookCallbackInput) { h.ToolResponse = json.RawMessage(`{"taskId":"x"}`) },
	} {
		t.Run(name, func(t *testing.T) {
			in := bashPostToolUse("npm run dev", true, "task-abc")
			mutate(&in)
			id, command, ok := bgShellLaunch(in)
			assert.True(t, ok, "the launch still happened")
			assert.Equal(t, "", id, "the id is lost, not invented")
			assert.Equal(t, "npm run dev", command, "the command still drives the liveness match")
		})
	}

	// A malformed tool_input is different: without it there is no evidence
	// the call was backgrounded at all, so it must NOT count.
	in := bashPostToolUse("npm run dev", true, "task-abc")
	in.ToolInput = json.RawMessage(`{not json`)
	_, _, ok := bgShellLaunch(in)
	assert.False(t, ok, "no run_in_background evidence means no ledger entry")
}

func TestBgShellStop_ReadsTaskStop(t *testing.T) {
	stop := HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      "TaskStop",
		ToolInput:     json.RawMessage(`{"task_id":"task-abc"}`),
	}
	id, ok := bgShellStop(stop)
	require.True(t, ok)
	assert.Equal(t, "task-abc", id)

	// A TaskStop whose payload we cannot read still killed something.
	stop.ToolInput = json.RawMessage(`{not json`)
	id, ok = bgShellStop(stop)
	assert.True(t, ok, "the kill happened even if the id is unreadable")
	assert.Equal(t, "", id, "which BgShellSet.Remove treats as drop-the-oldest")

	_, ok = bgShellStop(bashPostToolUse("ls", false, ""))
	assert.False(t, ok, "a Bash call is not a TaskStop")
}

func TestHarnessTracksBackgroundShells(t *testing.T) {
	assert.True(t, harnessTracksBackgroundShells("claude"))
	assert.True(t, harnessTracksBackgroundShells(""), "an unset harness is Claude Code")
	assert.False(t, harnessTracksBackgroundShells("codex"),
		"Codex has no background-shell mechanism and must never grow a ledger")
	assert.False(t, harnessTracksBackgroundShells("not-a-harness"),
		"an unresolvable harness folds to false rather than growing an unretirable count")
}

func TestBackgroundActivityDetail(t *testing.T) {
	// The sub-agents-only wording predates background shells and is
	// asserted verbatim by existing tests — it must not drift.
	assert.Equal(t, "2 subagents running", BackgroundActivityDetail(2, 0))
	assert.Equal(t, "0 subagents running", BackgroundActivityDetail(0, 0))
	assert.Equal(t, "1 background shell running", BackgroundActivityDetail(0, 1))
	assert.Equal(t, "3 background shells running", BackgroundActivityDetail(0, 3))
	assert.Equal(t, "2 subagents, 1 background shell running", BackgroundActivityDetail(2, 1))
}

func TestBgShellNeedle(t *testing.T) {
	assert.Equal(t, "npm run dev", bgShellNeedle("npm run dev"))
	assert.Equal(t, "", bgShellNeedle("ls -l"), "too generic to distinguish processes")
	assert.Equal(t, "", bgShellNeedle(""))

	// A single quote is rewritten by the harness's own shell quoting, so
	// the longest quote-free run is the most specific safe fragment —
	// here the quoted body, which outweighs the "python -c" prefix.
	assert.Equal(t, "print(1234567)",
		bgShellNeedle(`python -c 'print(1234567)'`))

	// Newlines are dropped so one matcher works against both a Linux
	// /proc cmdline (which keeps them) and macOS `ps` output (which
	// cannot represent them).
	assert.Equal(t, "second line here",
		bgShellNeedle("short\nsecond line here\nalso"))
}

func TestReconcileBgShells_RetiresDeadKeepsAliveAndAbstains(t *testing.T) {
	now := time.Now()
	ledger := map[string]db.BgShellSeen{
		"alive":     {Command: "npm run dev --port 4321", Seen: now},
		"dead":      {Command: "pytest tests/integration", Seen: now},
		"undecided": {Command: "ls", Seen: now},
	}
	cmdlines := []string{
		"/usr/bin/node /usr/bin/npm",
		"/bin/bash -c ... eval 'npm run dev --port 4321' < /dev/null",
	}

	got := ReconcileBgShells(ledger, cmdlines)
	assert.Equal(t, []string{"alive"}, got.Alive, "matched inside the wrapper shell's argv")
	assert.Equal(t, []string{"dead"}, got.Dead, "no live process carries this command")
	assert.Equal(t, []string{"undecided"}, got.Undecided,
		"a command too generic to match on is neither confirmed nor retired")

	assert.Empty(t, ReconcileBgShells(nil, cmdlines).Dead, "an empty ledger has nothing to retire")
	// Positive evidence that NOTHING is running retires everything
	// matchable — this is the signal that replaces the exit hook Claude
	// Code never fires.
	got = ReconcileBgShells(ledger, nil)
	assert.ElementsMatch(t, []string{"alive", "dead"}, got.Dead)
	assert.Equal(t, []string{"undecided"}, got.Undecided)
}

// Two identical commands, one survivor: exactly one entry must be retired.
// Claiming each live process at most once is what makes this work — a
// naive contains-check would match both entries to the one process (and
// retire nothing).
func TestReconcileBgShells_DuplicateCommandsClaimOneProcessEach(t *testing.T) {
	now := time.Now()
	ledger := map[string]db.BgShellSeen{
		"first":  {Command: "npm run dev", Seen: now},
		"second": {Command: "npm run dev", Seen: now},
		"third":  {Command: "npm run dev", Seen: now},
	}
	cmdlines := []string{
		"/bin/bash -c eval 'npm run dev'",
		"/bin/bash -c eval 'npm run dev'",
	}

	got := ReconcileBgShells(ledger, cmdlines)
	assert.Len(t, got.Alive, 2, "two survivors claim two processes")
	assert.Len(t, got.Dead, 1, "the third is retired")
	assert.Empty(t, got.Undecided)

	// Deterministic across runs despite Go's randomised map iteration.
	for range 20 {
		assert.Equal(t, got, ReconcileBgShells(ledger, cmdlines))
	}
}

// The host-side half: a real child process must be visible below this test
// process, at whatever depth. This is the property the whole reconcile
// rests on, and the one the issue flagged as unverified.
func TestDescendantCommandLines_FindsARealGrandchild(t *testing.T) {
	marker := fmt.Sprintf("tcl613-marker-%d", os.Getpid())
	// A grandchild, not a direct child: a real background shell runs under
	// one or more wrapper processes (`bwrap` under a sandboxed Claude
	// Code), so the walk must recurse rather than check direct children.
	cmd := exec.Command("sh", "-c", "sh -c 'sleep 30 #"+marker+"' ; true")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	var cmdlines []string
	var ok bool
	// The grandchild is forked asynchronously; poll briefly for it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cmdlines, ok = DescendantCommandLines(os.Getpid())
		if ok && strings.Contains(strings.Join(cmdlines, "\n"), marker) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.True(t, ok, "the host's process table must be readable on a supported platform")
	assert.Contains(t, strings.Join(cmdlines, "\n"), marker,
		"a descendant's full argv must be visible, at any depth")

	// And the matcher must join a ledger entry to it the same way the
	// daemon does.
	got := ReconcileBgShells(map[string]db.BgShellSeen{
		"task-1": {Command: "sleep 30 #" + marker, Seen: time.Now()},
	}, cmdlines)
	assert.Equal(t, []string{"task-1"}, got.Alive, "the recorded command matched a live process")
}

// "Cannot tell" must never be reported as "nothing is running", or the
// reconcile would retire every entry on a host it cannot inspect.
func TestDescendantCommandLines_UnknownRootReportsNotOK(t *testing.T) {
	_, ok := DescendantCommandLines(0)
	assert.False(t, ok, "pid 0 is a row that never recorded one")
	_, ok = DescendantCommandLines(-1)
	assert.False(t, ok)

	// A leaf process is the opposite case: readable, and positively
	// nothing below it.
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	cmdlines, ok := DescendantCommandLines(cmd.Process.Pid)
	assert.True(t, ok, "a live leaf process is readable")
	assert.Empty(t, cmdlines, "and has positively nothing below it")
}
