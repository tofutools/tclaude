package statusbar

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func observeTCL925SQLiteSidecarsAtCleanup(t testing.TB, home, family string) {
	t.Helper()
	t.Cleanup(func() {
		matches, err := filepath.Glob(filepath.Join(home, ".tclaude", "data", "db.sqlite-*"))
		if err != nil {
			t.Fatalf("TCL-925 cleanup probe %s could not list SQLite sidecars: %v", family, err)
		}
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, filepath.Base(match))
		}
		sort.Strings(names)
		t.Logf("TCL-925 cleanup probe %s sidecars=%v", family, names)
	})
}

func TestApplyRenderWritesPreservesGitSnapshotFreshness(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	const conv = "statusline-pr-freshness"
	fetchedAt := time.Now().Add(-10 * time.Second).Truncate(time.Microsecond)
	input := StatusLineInput{}
	input.Workspace.CurrentDir = "/repo"

	require.True(t, applyRenderWrites(renderWrites{
		Input:         input,
		WorkspaceConv: conv,
		Git: &GitSnapshot{
			RepoURL:       "https://github.com/o/r",
			Branch:        "feature",
			DefaultBranch: "main",
			PRNumber:      42,
			PRURL:         "https://github.com/o/r/pull/42",
			PRState:       "open",
			FetchedAt:     fetchedAt,
		},
	}))

	ws, err := db.GetAgentWorkspace(conv)
	require.NoError(t, err)
	assert.WithinDuration(t, fetchedAt, ws.UpdatedAt, time.Microsecond,
		"render time must not make a reused git-cache result look newly fetched")
}

func TestStatusbarSoftDisablesInsideTclaudeLayer(t *testing.T) {
	t.Setenv("TCLAUDE_IGNORE_HOOKS", "1")
	require.NoError(t, run())
}

// TestStatusLineInput_ParsesEffortLevel pins the effort.level field path
// against Claude Code's documented statusline schema. The whole feature
// hinges on reading the right key — CC emits the reasoning-effort level as
// a nested {"effort":{"level":"high"}} block — so this guards against a
// silent rename/typo of the json tag. The payload mirrors the documented
// example (https://code.claude.com/docs/en/statusline), trimmed to the
// fields the statusbar reads.
func TestStatusLineInput_ParsesEffortLevel(t *testing.T) {
	const payload = `{
		"session_id": "abc123",
		"model": { "id": "claude-opus-4-8", "display_name": "Opus 4.8" },
		"workspace": { "current_dir": "/tmp/proj" },
		"context_window": { "used_percentage": 8 },
		"effort": { "level": "high" }
	}`

	var input StatusLineInput
	require.NoError(t, json.Unmarshal([]byte(payload), &input), "unmarshal statusline JSON")
	assert.Equal(t, "high", input.Effort.Level, "effort.level field path")
}

func TestTemporarySandboxWarningFollowsStableAgentAcrossRotation(t *testing.T) {
	home := t.TempDir()
	observeTCL925SQLiteSidecarsAtCleanup(t, home, "statusbar")
	t.Setenv("HOME", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	const oldConv = "statusline-unlocked-old"
	const newConv = "statusline-unlocked-new"
	agentID, _, err := db.EnsureAgentForConv(oldConv, "test")
	require.NoError(t, err)
	normal := "on"
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, SandboxMode: &normal,
	}))
	override := "off"
	require.NoError(t, db.SetTemporarySandboxModeForConv(
		oldConv, normal, "harness-builtin", "test", &override,
	))
	assert.True(t, temporarySandboxOff(oldConv), "the override must raise the SB-OFF badge")

	_, err = db.RotateAgentConv(oldConv, newConv, "clear")
	require.NoError(t, err)
	assert.True(t, temporarySandboxOff(newConv), "the badge must follow the stable agent across a /clear rotation")

	require.NoError(t, db.SetTemporarySandboxModeForConv(newConv, "", "", "", nil))
	assert.False(t, temporarySandboxOff(newConv), "clearing the override must drop the badge")
}

// TestStatusLineInput_ParsesModelID pins the model.id field path — the
// full model ID the documented schema carries alongside display_name.
// Model inheritance on reincarnate/clone/resume hinges on persisting
// exactly this key (it's the only statusline model field `claude
// --model` accepts back), so guard the json tag the same way the
// effort test does.
func TestStatusLineInput_ParsesModelID(t *testing.T) {
	const payload = `{
		"session_id": "abc123",
		"model": { "id": "claude-fable-5", "display_name": "Fable 5" }
	}`

	var input StatusLineInput
	require.NoError(t, json.Unmarshal([]byte(payload), &input), "unmarshal statusline JSON")
	assert.Equal(t, "claude-fable-5", input.Model.ID, "model.id field path")
	assert.Equal(t, "Fable 5", input.Model.DisplayName, "model.display_name field path")
}

// TestStatusLineInput_ModelIDAbsent confirms an older Claude Code
// payload without model.id leaves the field empty — the signal
// UpdateSessionModelID treats as a no-op so inheritance degrades to
// claude's default instead of writing garbage.
func TestStatusLineInput_ModelIDAbsent(t *testing.T) {
	const payload = `{
		"model": { "display_name": "Opus 4.8" }
	}`

	var input StatusLineInput
	require.NoError(t, json.Unmarshal([]byte(payload), &input), "unmarshal statusline JSON")
	assert.Equal(t, "", input.Model.ID, "absent model.id leaves field empty")
}

// TestShortModelLabel pins the head-of-context-line model tag, including
// the fallback that keeps a full-ID-selected model (e.g. `--model
// claude-opus-5`, which CC doesn't emit a two-word display name for) from
// collapsing to the bare "ctx" placeholder.
func TestShortModelLabel(t *testing.T) {
	cases := []struct {
		name        string
		displayName string
		id          string
		want        string
	}{
		{"known two-word display name", "Opus 4.6", "claude-opus-4-6", "o4.6"},
		{"three-word display name", "Claude Opus 4.8", "claude-opus-4-8", "cOpus4.8"},
		{"display name with 1m suffix trimmed", "Opus 4.8[1m]", "claude-opus-4-8[1m]", "o4.8"},
		{"empty display name falls back to id", "", "claude-opus-5", "opus-5"},
		{"empty display name id with 1m suffix", "", "claude-opus-5[1m]", "opus-5"},
		{"single-token display name preferred over id", "Fable", "claude-fable-5", "fable"},
		{"single-token raw id display name", "claude-opus-5", "claude-opus-5", "opus-5"},
		{"nothing known yields placeholder", "", "", "ukn-mdl"},
		{"whitespace-only fields yield placeholder", "   ", "  ", "ukn-mdl"},
		{"lone 1m suffix yields placeholder", "[1m]", "[1m]", "ukn-mdl"},
		// The label names the model only; the window marker beside it is the
		// caller's job (contextWindowTag) because only the caller knows the
		// EFFECTIVE window. Keeping Claude Code's own "(1M context)" here
		// produced "o5(1Mcontext)" — a 1M claim on a pane whose bar had been
		// re-based onto a 450k pin.
		{"context parenthetical stripped", "Opus 5 (1M context)", "claude-opus-5[1m]", "o5"},
		{"context parenthetical stripped lowercase", "opus 5 (200k context)", "claude-opus-5", "o5"},
		{"context parenthetical on single-token name", "Fable (1M context)", "claude-fable-5", "fable"},
		// A qualifier that is NOT about capacity must survive: dropping it would
		// collapse two genuinely different models onto one tag.
		{"non-context parenthetical preserved", "Opus 5 (fast)", "claude-opus-5", "o5(fast)"},
		{"parenthetical only yields placeholder", "(1M context)", "", "ukn-mdl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shortModelLabel(tc.displayName, tc.id))
		})
	}
}

// TestStatusLineInput_EffortAbsent confirms a payload without the effort
// block (model lacks reasoning-effort support) leaves Level empty — the
// signal both surfaces use to omit the effort token rather than render a
// blank one.
func TestStatusLineInput_EffortAbsent(t *testing.T) {
	const payload = `{
		"model": { "display_name": "Sonnet 4.6" },
		"context_window": { "used_percentage": 12 }
	}`

	var input StatusLineInput
	require.NoError(t, json.Unmarshal([]byte(payload), &input), "unmarshal statusline JSON")
	assert.Equal(t, "", input.Effort.Level, "absent effort block leaves level empty")
}
