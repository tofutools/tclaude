package session

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TestPreCompactFloor pins the window→floor matching: exact windows,
// near-miss windows that should still resolve by ratio, and windows too
// far from any configured class (which must fail open via ok=false).
func TestPreCompactFloor(t *testing.T) {
	def := config.DefaultPreCompactThresholds() // {200k→150k, 1M→800k}
	cases := []struct {
		name   string
		window int64
		want   int64
		ok     bool
	}{
		{"exact 200k", 200_000, 150_000, true},
		{"exact 1M", 1_000_000, 800_000, true},
		{"near 1M (1048576) still matches 1M", 1_048_576, 800_000, true},
		{"near 200k (204800) still matches 200k", 204_800, 150_000, true},
		{"500k is exactly 2x from 1M → matches 1M", 500_000, 800_000, true},
		{"tiny 8k window has no class within 2x → fail open", 8_000, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := preCompactFloor(def, c.window)
			assert.Equal(t, c.ok, ok)
			if c.ok {
				assert.Equal(t, c.want, got)
			}
		})
	}
}

// TestDecidePreCompact drives the guard against a real DB snapshot and a
// real on-disk config, asserting it blocks only when it should and fails
// open everywhere else. The guard must never block manual compaction by
// default, and must allow whenever the data to judge is missing.
func TestDecidePreCompact(t *testing.T) {
	enabledDefault := &config.PreCompactGuardConfig{Enabled: true}
	cases := []struct {
		name         string
		guard        *config.PreCompactGuardConfig
		trigger      string
		pct          float64
		window       int64
		seedSnapshot bool
		envSession   string // "" → simulate a session not launched by tclaude
		wantBlock    bool
	}{
		{"guard absent → allow", nil, "auto", 20, 1_000_000, true, "sess", false},
		{"guard disabled → allow", &config.PreCompactGuardConfig{Enabled: false}, "auto", 20, 1_000_000, true, "sess", false},
		// The headline case: a 1M session CC tries to auto-compact at the
		// 200K boundary (~20% of the 1M bar) is refused.
		{"auto at 200K boundary on 1M window → block", enabledDefault, "auto", 20, 1_000_000, true, "sess", true},
		{"auto above 1M floor (85%) → allow", enabledDefault, "auto", 85, 1_000_000, true, "sess", false},
		{"auto below 200k floor (50%→100k) → block", enabledDefault, "auto", 50, 200_000, true, "sess", true},
		{"auto above 200k floor (80%→160k) → allow", enabledDefault, "auto", 80, 200_000, true, "sess", false},
		{"manual default → allow", enabledDefault, "manual", 5, 1_000_000, true, "sess", false},
		{"manual with block_manual → block", &config.PreCompactGuardConfig{Enabled: true, BlockManual: true}, "manual", 5, 1_000_000, true, "sess", true},
		{"empty/unknown trigger → allow", enabledDefault, "", 5, 1_000_000, true, "sess", false},
		{"no context snapshot → allow", enabledDefault, "auto", 0, 0, false, "sess", false},
		{"non-tclaude session (no env id) → allow", enabledDefault, "auto", 20, 1_000_000, false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			db.ResetForTest()

			cfg := config.DefaultConfig()
			cfg.PreCompactGuard = c.guard
			require.NoError(t, config.Save(cfg))

			if c.envSession != "" {
				require.NoError(t, SaveSessionState(&SessionState{
					ID:     c.envSession,
					ConvID: "conv-" + c.envSession,
					Status: StatusIdle,
				}))
				if c.seedSnapshot {
					require.NoError(t, db.UpdateContextSnapshot(c.envSession, c.pct, 1, 0, c.window))
				}
			}

			var buf bytes.Buffer
			input := HookCallbackInput{
				HookEventName: "PreCompact",
				ConvID:        "conv-" + c.envSession,
				Trigger:       c.trigger,
			}
			require.NoError(t, decidePreCompact(input, c.envSession, &buf))

			if c.wantBlock {
				var dec preCompactDecision
				require.NoError(t, json.Unmarshal(buf.Bytes(), &dec),
					"expected a JSON block decision, got %q", buf.String())
				assert.Equal(t, "block", dec.Decision)
				assert.NotEmpty(t, dec.Reason, "a block must carry a human-readable reason")
			} else {
				assert.Empty(t, strings.TrimSpace(buf.String()),
					"expected no decision (allow), got %q", buf.String())
			}
		})
	}
}

// TestPreCompactGuardConfigValidate locks the threshold validation: a
// sane ladder passes; a floor ≥ its window (which could never be
// reached, blocking forever) and non-positive sizes are rejected.
func TestPreCompactGuardConfigValidate(t *testing.T) {
	ok := config.DefaultConfig()
	ok.PreCompactGuard = &config.PreCompactGuardConfig{
		Enabled:    true,
		Thresholds: []config.PreCompactThreshold{{WindowSize: 1_000_000, MinTokens: 800_000}},
	}
	assert.Empty(t, config.Validate(ok), "a sane threshold ladder must validate")

	bad := config.DefaultConfig()
	bad.PreCompactGuard = &config.PreCompactGuardConfig{
		Enabled: true,
		Thresholds: []config.PreCompactThreshold{
			{WindowSize: 0, MinTokens: 100},             // window must be positive
			{WindowSize: 200_000, MinTokens: 0},         // floor must be positive
			{WindowSize: 200_000, MinTokens: 9_999_999}, // floor ≥ window
		},
	}
	assert.Len(t, config.Validate(bad), 3, "each malformed threshold must report one error")
}

// TestRunHookCallback_PreCompactEmitsBlockOnStdout exercises the full
// stdin→decision→stdout plumbing: a PreCompact payload routed through
// runHookCallback must parse the trigger and write a {"decision":"block"}
// to the hook's stdout when the conversation is below the floor.
func TestRunHookCallback_PreCompactEmitsBlockOnStdout(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	cfg := config.DefaultConfig()
	cfg.PreCompactGuard = &config.PreCompactGuardConfig{Enabled: true}
	require.NoError(t, config.Save(cfg))

	require.NoError(t, SaveSessionState(&SessionState{
		ID:      "pc-sess",
		ConvID:  "conv-pc",
		Status:  StatusIdle,
		Harness: harness.CodexName,
	}))
	// 20% of a 1M window = ~200K used, below the 800K floor → block.
	require.NoError(t, db.UpdateContextSnapshot("pc-sess", 20, 1, 0, 1_000_000))

	out := captureHookStdout(t, "pc-sess", map[string]any{
		"session_id":      "conv-pc",
		"hook_event_name": "PreCompact",
		"trigger":         "auto",
		"cwd":             dir,
		"model":           "gpt-5.5",
	})

	var dec preCompactDecision
	require.NoError(t, json.Unmarshal([]byte(out), &dec),
		"stdout should carry a JSON block decision, got %q", out)
	assert.Equal(t, "block", dec.Decision)
	assert.NotEmpty(t, dec.Reason)

	snap, err := db.GetContextSnapshot("pc-sess")
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.5", snap.Model,
		"a blocked PreCompact still captures Codex's active model")
	assert.Equal(t, "gpt-5.5", snap.ModelID,
		"the resume-safe model id converges before the guard returns")
}

// captureHookStdout runs runHookCallback with payload on stdin and
// TCLAUDE_SESSION_ID set, capturing whatever the callback writes to
// stdout (the PreCompact decision). os.Stdin/os.Stdout are restored
// after.
func captureHookStdout(t *testing.T, sessionID string, payload map[string]any) string {
	t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(t, err)

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	_, _ = inW.Write(data) // small payload fits the pipe buffer
	require.NoError(t, inW.Close())
	origStdin := os.Stdin
	os.Stdin = inR
	t.Cleanup(func() { os.Stdin = origStdin })

	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	origStdout := os.Stdout
	os.Stdout = outW
	t.Cleanup(func() { os.Stdout = origStdout })

	t.Setenv("TCLAUDE_SESSION_ID", sessionID)
	require.NoError(t, runHookCallback())

	require.NoError(t, outW.Close())
	os.Stdout = origStdout
	buf, err := io.ReadAll(outR)
	require.NoError(t, err)
	return string(buf)
}
