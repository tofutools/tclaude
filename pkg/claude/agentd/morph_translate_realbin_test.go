package agentd_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Opt-in smoke against real harness binaries (never in CI):
//
//	TCLAUDE_MORPH_REALBIN=1 scripts/test.sh ./pkg/claude/agentd/ -run TestMorphTranslateRealBinary -v
//
// It exercises only credential-free native install paths: OpenCode's own
// `import`/`export` validate the translated session against the real schema.
// A resume that prompts each real CLI spends model calls on the operator's
// accounts and is deliberately not part of this test.
func TestMorphTranslateRealBinary(t *testing.T) {
	if os.Getenv("TCLAUDE_MORPH_REALBIN") != "1" {
		t.Skip("set TCLAUDE_MORPH_REALBIN=1 to run against installed harness binaries")
	}
	t.Run("opencode-import-export", func(t *testing.T) {
		if _, err := exec.LookPath("opencode"); err != nil {
			t.Skip("opencode is not installed")
		}
		f := newFlow(t)
		// OpenCode keeps its store under XDG dirs; keep it in the test HOME.
		for _, k := range []string{"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME"} {
			t.Setenv(k, "")
		}
		const source = "0f0e0d0c-0b0a-4908-8706-050403020102"
		cwd := testutil.CanonicalTempDir(t)
		f.HaveAliveSession(source, "realbin-src", "realbin-pane", cwd)
		cc := f.World.CCs.GetByLabel("realbin-src")
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "user", "uuid": "u1", "cwd": cwd, "timestamp": "2026-10-01T10:00:00.000Z", "message": map[string]any{"role": "user", "content": morphSecret}}))
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "assistant", "uuid": "a1", "parentUuid": "u1", "cwd": cwd, "timestamp": "2026-10-01T10:00:01.000Z", "message": map[string]any{"id": "m1", "role": "assistant", "model": "claude-opus-5-5", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}}}}}))
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "user", "uuid": "u2", "parentUuid": "a1", "cwd": cwd, "timestamp": "2026-10-01T10:00:02.000Z", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "README.md\nmain.go"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}}}}))
		require.NoError(t, cc.AppendTurn(map[string]any{"type": "assistant", "uuid": "a2", "parentUuid": "u2", "cwd": cwd, "timestamp": "2026-10-01T10:00:03.000Z", "message": map[string]any{"id": "m2", "role": "assistant", "model": "claude-opus-5-5", "content": []any{map[string]any{"type": "text", "text": "There are two files."}}}}))
		convops.ScanAndUpsertFile(cc.JsonlPath)

		claude, opencode := mustGetHarness(t, harness.DefaultName), mustGetHarness(t, harness.OpenCodeName)
		res, err := harness.TranslateHistory(claude, opencode, source, cwd, harness.TranslateOptions{Policy: transcript.Options{TargetModel: "anthropic/claude-sonnet-5-5"}})
		require.NoError(t, err, "the real `opencode import` accepts the translated session")
		t.Cleanup(res.Cleanup)
		text := historyText(t, opencode, res.ConvID, cwd)
		for _, want := range []string{morphSecret, "⟦tool call · Bash⟧", "README.md\nmain.go", "There are two files.", "carried over from claude"} {
			assert.Contains(t, text, want, "the real `opencode export` returns the carried turn")
		}
		assert.Contains(t, text, "image/png), 70 bytes]", "the image survives the real OpenCode store")

		// And back out of OpenCode: the session the real binary now owns
		// translates to another harness.
		codex := mustGetHarness(t, harness.CodexName)
		t.Setenv("CODEX_HOME", "")
		back, err := harness.TranslateHistory(opencode, codex, res.ConvID, cwd, harness.TranslateOptions{})
		require.NoError(t, err)
		t.Cleanup(back.Cleanup)
		assert.True(t, strings.Contains(historyText(t, codex, back.ConvID, cwd), morphSecret))
	})
}

// Opt-in resume smoke that SPENDS MODEL CALLS on the operator's accounts, one
// cheap prompt per installed destination CLI (local only, never in CI):
//
//	TCLAUDE_MORPH_RESUME_SMOKE=1 scripts/test.sh ./pkg/claude/agentd/ -run TestMorphResumeRealCLI -v -count=1
//
// Each case translates a seeded source history into the destination's native
// store and resumes it with the real CLI, asking for the secret word the
// source turn carried. The stores live in the test HOME; the operator's
// credential file is copied in only while its access token has well over the
// run's length left, so the CLI never refreshes (and rotates) the copy. A
// credential the CLI would have to refresh is used in place instead (OpenCode)
// and the session the smoke imported is deleted afterwards.
func TestMorphResumeRealCLI(t *testing.T) {
	if os.Getenv("TCLAUDE_MORPH_RESUME_SMOKE") != "1" {
		t.Skip("set TCLAUDE_MORPH_RESUME_SMOKE=1 to resume translated histories with the real CLIs (spends model calls)")
	}
	realHome, err := os.UserHomeDir()
	require.NoError(t, err)
	realOpenCodeData := os.Getenv("XDG_DATA_HOME")
	if realOpenCodeData == "" {
		realOpenCodeData = filepath.Join(realHome, ".local", "share")
	}
	const ask = "What is the secret word I asked you to remember earlier in this conversation? Reply with only that word."

	type smokeCase struct {
		src, dst, bin string
		model         string
		creds         func(t *testing.T, home string) // seeds the copied credential, or skips
		args          func(convID string) []string
	}
	cases := []smokeCase{
		{src: harness.CodexName, dst: harness.DefaultName, bin: "claude", model: "claude-haiku-5-5",
			creds: func(t *testing.T, home string) {
				copyFreshCredential(t, filepath.Join(realHome, ".claude", ".credentials.json"), filepath.Join(home, ".claude", ".credentials.json"), claudeTokenExpiry)
			},
			args: func(id string) []string { return []string{"-p", "--model", "haiku", "--resume", id, ask} }},
		{src: harness.DefaultName, dst: harness.CodexName, bin: "codex", model: "gpt-5.6-luna",
			creds: func(t *testing.T, home string) {
				copyFreshCredential(t, filepath.Join(realHome, ".codex", "auth.json"), filepath.Join(home, ".codex", "auth.json"), codexTokenExpiry)
			},
			args: func(id string) []string {
				return []string{"exec", "--skip-git-repo-check", "-c", `model="gpt-5.6-luna"`, "-c", `model_reasoning_effort="low"`, "resume", id, ask}
			}},
		{src: harness.DefaultName, dst: harness.CopilotName, bin: "copilot", model: "claude-haiku-5.5",
			// Copilot's GitHub token lives in the OS keyring, not under COPILOT_HOME.
			creds: func(*testing.T, string) {},
			args:  func(id string) []string { return []string{"--resume", id, "--model", "claude-haiku-5.5", "-s", "-p", ask} }},
		{src: harness.DefaultName, dst: harness.OpenCodeName, bin: "opencode", model: "openai/gpt-5.6-luna",
			creds: func(*testing.T, string) {},
			args:  func(id string) []string { return []string{"run", "--session", id, "-m", "openai/gpt-5.6-luna", ask} }},
	}
	for _, c := range cases {
		t.Run(c.src+"->"+c.dst, func(t *testing.T) {
			bin, err := exec.LookPath(c.bin)
			if err != nil {
				t.Skipf("%s is not installed", c.bin)
			}
			t.Setenv(session.ResourceDelegationDirEnv, "")
			t.Setenv("CODEX_HOME", "")
			t.Setenv("COPILOT_HOME", "")
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)
			if c.dst == harness.OpenCodeName {
				// OpenCode's OAuth credential refreshes on use; a copy would
				// rotate the operator's refresh token out from under them.
				t.Setenv("XDG_DATA_HOME", realOpenCodeData)
				t.Setenv("XDG_CONFIG_HOME", "")
				t.Setenv("XDG_STATE_HOME", "")
				t.Setenv("XDG_CACHE_HOME", "")
			}
			c.creds(t, f.World.HomeDir)
			f.HaveGroup("crew")
			spawn := spawnMorphSource(t, f, c.src, "smoke")
			seedMorphHistory(t, f, c.src, spawn.ConvID)
			profile, err := db.ConversationResumeProfileForConv(spawn.ConvID)
			require.NoError(t, err)
			require.NotNil(t, profile)
			// The CLIs key their stores on the resolved cwd.
			cwd, err := filepath.EvalSymlinks(profile.Cwd)
			require.NoError(t, err)

			res, err := harness.TranslateHistory(mustGetHarness(t, c.src), mustGetHarness(t, c.dst), spawn.ConvID, cwd,
				harness.TranslateOptions{Policy: transcript.Options{TargetModel: c.model}})
			require.NoError(t, err)
			t.Cleanup(res.Cleanup)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, c.args(res.ConvID)...)
			cmd.Dir = cwd
			cmd.Env = smokeEnv()
			cmd.Stdin = strings.NewReader("")
			out, err := cmd.CombinedOutput()
			t.Logf("%s resume output:\n%s", c.bin, out)
			require.NoError(t, err)
			assert.Contains(t, strings.ToUpper(string(out)), "PELICAN", "the resumed %s session remembers the carried turn", c.dst)
		})
	}
}

// smokeEnv is the test process environment minus the markers of the agent
// session running the test, so a nested CLI starts as a plain one. Store
// relocations are dropped too: a tclaude pane exports CLAUDE_CONFIG_DIR at the
// operator's real config, which would send the CLI past the test HOME. Empty
// values are dropped rather than passed, since a CLI may read an empty
// COPILOT_HOME as a relative path instead of unset.
func smokeEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		switch {
		case value == "",
			name == "CLAUDECODE", strings.HasPrefix(name, "CLAUDE_CODE_"), name == "CLAUDE_CONFIG_DIR",
			strings.HasPrefix(name, "CODEX_"), name == "COPILOT_HOME", name == "GEMINI_CLI_HOME",
			strings.HasPrefix(name, "OPENCODE_"), strings.HasPrefix(name, "TCLAUDE_"),
			name == "TMUX", name == "TMUX_PANE":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// copyFreshCredential copies an operator credential file into the test HOME
// when its access token outlives the run by a wide margin, so the CLI uses it
// without refreshing; otherwise it skips rather than risk a rotated copy.
func copyFreshCredential(t *testing.T, src, dst string, expiry func([]byte) (time.Time, bool)) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("no credential at %s", src)
	}
	exp, ok := expiry(data)
	if !ok || time.Until(exp) < 30*time.Minute {
		t.Skipf("the credential at %s expires too soon to copy without a refresh; rerun after the CLI refreshes it", src)
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o700))
	require.NoError(t, os.WriteFile(dst, data, 0o600))
}

func claudeTokenExpiry(data []byte) (time.Time, bool) {
	var v struct {
		OAuth struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(data, &v) != nil || v.OAuth.ExpiresAt == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(v.OAuth.ExpiresAt), true
}

func codexTokenExpiry(data []byte) (time.Time, bool) {
	var v struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
		LastRefresh time.Time `json:"last_refresh"`
	}
	if json.Unmarshal(data, &v) != nil {
		return time.Time{}, false
	}
	// Codex also refreshes on age (about eight days since last_refresh), not
	// only near the token's own expiry; stay well inside that window.
	if v.LastRefresh.IsZero() || time.Since(v.LastRefresh) > 6*24*time.Hour {
		return time.Time{}, false
	}
	parts := strings.Split(v.Tokens.AccessToken, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
