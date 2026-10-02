package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// withGeminiSeatbelt pretends this host has sandbox-exec, and returns a
// descriptor copy that advertises the builtin OS sandbox the way the macOS
// registration does.
func withGeminiSeatbelt(t *testing.T) *Harness {
	t.Helper()
	prior := geminiSeatbeltAvailable
	geminiSeatbeltAvailable = true
	t.Cleanup(func() { geminiSeatbeltAvailable = prior })
	h := *MustGet(GeminiName)
	h.BuiltinOSSandbox = true
	return &h
}

func TestGeminiSeatbeltModeIsMacOSOnly(t *testing.T) {
	prior := geminiSeatbeltAvailable
	geminiSeatbeltAvailable = false
	t.Cleanup(func() { geminiSeatbeltAvailable = prior })
	catalog := geminiSandbox{}
	assert.NotContains(t, catalog.Modes(), GeminiSandboxSeatbelt)
	_, err := catalog.ValidateMode(GeminiSandboxSeatbelt)
	assert.ErrorContains(t, err, "macOS")

	geminiSeatbeltAvailable = true
	assert.Contains(t, catalog.Modes(), GeminiSandboxSeatbelt)
	mode, err := catalog.ValidateMode(" seatbelt ")
	require.NoError(t, err)
	assert.Equal(t, GeminiSandboxSeatbelt, mode)
	assert.NotEmpty(t, catalog.ModeHelp(GeminiSandboxSeatbelt))
}

// The seatbelt mode is the env lever Gemini itself reads, placed last so no
// earlier export can contradict it, with SANDBOX present-but-empty so neither
// the parent nor a .env can make Gemini believe it is already contained.
func TestGeminiSpawnerSeatbeltSelectsSandboxExec(t *testing.T) {
	cmd := geminiSpawner{}.BuildCommand(SpawnSpec{
		EnvExports:         "export GEMINI_SANDBOX=docker; export SEATBELT_PROFILE=strict-open; ",
		HarnessBuiltinMode: GeminiSandboxSeatbelt,
	})
	assert.Equal(t, "export GEMINI_SANDBOX=docker; export SEATBELT_PROFILE=strict-open; "+
		"export GEMINI_SANDBOX=sandbox-exec; export SEATBELT_PROFILE=permissive-open; export SANDBOX=; gemini", cmd)
}

func TestGeminiSeatbeltAccessVerdictAndHookBroker(t *testing.T) {
	h := withGeminiSeatbelt(t)
	for mode, want := range map[string]string{
		GeminiSandboxSeatbelt: "on",
		GeminiSandboxOff:      "off",
		GeminiSandboxInherit:  "unconfigured",
	} {
		verdict, err := BuiltinLaunchOSSandboxForValidatedMode(h, mode)
		require.NoErrorf(t, err, "mode %s", mode)
		assert.Equalf(t, want, verdict.State, "mode %s", mode)
	}
	assert.True(t, HooksRunInsideBuiltinSandbox(h, GeminiSandboxSeatbelt),
		"Gemini runs hooks in its Seatbelt child, where the database is read-only")
	assert.False(t, HooksRunInsideBuiltinSandbox(h, GeminiSandboxOff))
	assert.False(t, HooksRunInsideBuiltinSandbox(MustGet(DefaultName), ClaudeSandboxOn),
		"Claude Code runs hooks outside its command sandbox")
}

// A Seatbelt-mode launch keeps its chats under ~/.cache/.gemini; the store and
// the usage locator must find them there too.
func TestGeminiConvStoreReadsTheSeatbeltRuntimeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(GeminiHomeEnvVar, "")
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	project := filepath.Join(home, "proj")
	slugDir := geminiProject(t, filepath.Join(home, ".cache", ".gemini"), "proj", project)
	path := geminiWriteSession(t, slugDir, "session-2026-10-02T08-00-"+geminiIDA[:8]+".jsonl",
		`{"sessionId":"`+geminiIDA+`","projectHash":"h","startTime":"2026-10-02T08:00:00Z","lastUpdated":"2026-10-02T08:00:00Z","kind":"main"}`,
		`{"id":"u1","type":"user","content":"walled hello"}`,
		`{"id":"g1","type":"gemini","content":"hi","model":"gemini-3-flash","tokens":{"input":9,"output":1,"cached":0,"total":10}}`)
	require.NoError(t, os.MkdirAll(project, 0o755))

	all, err := geminiConvStore{}.ListConvs("")
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, geminiIDA, all[0].SessionID)
	assert.Equal(t, "walled hello", all[0].FirstPrompt)

	located, found, err := LocateGeminiSessionFile(geminiIDA)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, path, located)
}

func TestValidateGeminiSeatbeltLaunchNeedsEnvironmentAuth(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".gemini"), 0o755))
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	env := map[string]string{"HOME": home, geminiSystemSettingsEnvVar: filepath.Join(root, "none.json")}
	getenv := func(name string) string { return env[name] }
	settings := filepath.Join(home, ".gemini", "settings.json")

	assert.ErrorContains(t, ValidateGeminiSeatbeltLaunch(getenv, cwd), "selected auth type")
	require.NoError(t, os.WriteFile(settings, []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o644))
	assert.ErrorContains(t, ValidateGeminiSeatbeltLaunch(getenv, cwd), "OAuth credentials")
	for _, authType := range []string{GeminiAuthAPIKey, "vertex-ai"} {
		require.NoError(t, os.WriteFile(settings, []byte(`{"security":{"auth":{"selectedType":"`+authType+`"}}}`), 0o644))
		assert.NoErrorf(t, ValidateGeminiSeatbeltLaunch(getenv, cwd), "auth type %s", authType)
	}
}

func TestGeminiSeatbeltLaunchEnvCarriesOnlyTheHostsTrust(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	proj := filepath.Join(root, "work", "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".gemini"), 0o755))
	env := map[string]string{"HOME": home}
	getenv := func(name string) string { return env[name] }
	store := filepath.Join(home, ".gemini", "trustedFolders.json")
	trust := func(trustDir bool) bool {
		t.Helper()
		got, err := GeminiSeatbeltLaunchEnv(getenv, proj, trustDir)
		require.NoError(t, err)
		return got[geminiTrustWorkspaceEnvVar] == "true"
	}

	assert.False(t, trust(false), "no rule and no --trust-dir: Gemini decides")
	assert.True(t, trust(true), "--trust-dir is about to trust it")

	require.NoError(t, os.WriteFile(store, []byte(`{"`+filepath.Join(root, "work")+`":"TRUST_FOLDER"}`), 0o644))
	assert.True(t, trust(false), "a trusted ancestor covers the folder")

	require.NoError(t, os.WriteFile(store, []byte(`{"`+filepath.Join(root, "work")+`":"TRUST_FOLDER","`+proj+`":"DO_NOT_TRUST"}`), 0o644))
	assert.False(t, trust(true), "the longest rule wins, and distrust is never overridden")

	require.NoError(t, os.WriteFile(store, []byte(`{"`+filepath.Join(proj, "child")+`":"TRUST_PARENT"}`), 0o644))
	assert.True(t, trust(false), "TRUST_PARENT covers the rule's parent")

	require.NoError(t, os.WriteFile(store, []byte(`{"`+filepath.Join(root, "work", "projX")+`":"TRUST_FOLDER"}`), 0o644))
	assert.False(t, trust(false), "a sibling with a shared prefix is not an ancestor")
}
