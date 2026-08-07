package copilotfixture_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// TCL-973's acceptance evidence: tclaude's OWN directory-trust seeding, run
// against the real pinned CLI.
//
// This file deliberately does not re-measure the gate itself. Phase 0 already
// pinned both halves of that, and both are CI-gated:
//
//   - TestCopilotPermissionFolderTrustBlocksFirst — a fresh COPILOT_HOME blocks
//     at launch with zero provider requests. That scenario is what gives the
//     one below its meaning; without it, "the seeded launch reached the mock"
//     would be consistent with there being no gate at all.
//   - TestCopilotPermissionTrustBypassSurface — no launch flag clears it, a
//     hand-written config.json `trustedFolders` does, and the same key in
//     settings.json does not.
//
// What is left, and what only these scenarios can establish, is that the
// PRODUCTION seeder (harness.EnsureCopilotDirTrustedForLaunch, called exactly
// as session.New calls it) produces a file the real CLI honours — on a fresh
// home and, more importantly, on an installed one whose config.json is already
// owned and rewritten by the CLI. A fixture that hand-wrote its own JSON would
// prove the CLI honours that JSON while saying nothing about what tclaude
// writes.

// dirTrustDeadline bounds a seeded run. A cleared launch reaches the mock in
// ~2s; the budget is generous because the arm that matters here is the
// positive one, and SettledWhen ends the run as soon as its evidence lands.
const dirTrustDeadline = 60 * time.Second

// seedTrustLikeProduction calls the production seeder with the launch's own
// environment — the same call session.New makes for a --trust-dir spawn, whose
// profile may have relocated COPILOT_HOME.
//
// projectDir is the cwd as the CALLER spells it, because that is the one thing
// a scenario here gets to vary: production is handed whatever spelling the
// operator's shell produced, and the seeder's two-spelling behaviour only means
// anything when those differ.
func seedTrustLikeProduction(t *testing.T, dirs copilotfixture.Dirs, projectDir string) {
	t.Helper()
	require.NoError(t, harness.EnsureCopilotDirTrustedForLaunch(
		func(name string) string {
			if name == harness.CopilotHomeEnvVar {
				return dirs.Home
			}
			return ""
		},
		dirs.Root,
		projectDir,
	))
}

// dirTrustRun launches the seeded pane and ends as soon as the provider has
// been contacted, which is the whole observation: the modal blocks before that
// happens, so a provider request is proof the gate was cleared.
func dirTrustRun(
	t *testing.T,
	dirs copilotfixture.Dirs,
	mock *copilotfixture.MockProvider,
	workDir string,
) copilotfixture.PTYResult {
	t.Helper()
	return copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions: copilotfixture.RunOptions{
			Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: workDir,
			BaseURL: mock.BaseURL(),
			Prompt:  "Reply with the text the provider gives you.",
		},
		Deadline:    dirTrustDeadline,
		SettledWhen: func() bool { return len(mock.Requests()) >= 1 },
	})
}

// readManagedJSON parses one of Copilot's two settings files, tolerating the
// whole-line `//` comments the CLI writes into its managed stub.
func readManagedJSON(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)
	kept := make([]string, 0)
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	root := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal([]byte(strings.Join(kept, "\n")), &root),
		"parsing %s", path)
	return root
}

func readTrustedFolders(t *testing.T, path string) []string {
	t.Helper()
	raw, found := readManagedJSON(t, path)["trustedFolders"]
	if !found {
		return nil
	}
	var folders []string
	require.NoError(t, json.Unmarshal(raw, &folders))
	return folders
}

// TestCopilotDirTrustProductionSeedingClearsTheModal is the acceptance proof:
// tclaude's own seeder, and nothing else, turns the pane that
// TestCopilotPermissionFolderTrustBlocksFirst leaves parked forever into one
// that completes its first turn.
func TestCopilotDirTrustProductionSeedingClearsTheModal(t *testing.T) {
	requireSmokeParallel(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK STREAMED ANSWER"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	seedTrustLikeProduction(t, dirs, dirs.WorkDir)

	res := dirTrustRun(t, dirs, mock, dirs.WorkDir)
	require.True(t, res.Settled,
		"a seeded launch must reach the provider; transcript:\n%s", res.TranscriptText())
	assert.NotEmpty(t, mock.Requests())
	assert.False(t, res.Contains(copilotfixture.TrustPromptMarker),
		"the seeded launch must not show the modal at all")
}

// TestCopilotDirTrustSeedingPreservesAnInstalledTrustStore is the INSTALLED
// user's case, and it is the one a fresh-home scenario cannot cover.
//
// A settled install's config.json is a CLI-MANAGED file: after the startup
// migration it carries the operator's own `trustedFolders` plus keys the CLI
// minted itself (`firstLaunchAt`), behind two `//` comment lines, while the
// user settings it used to hold have moved into settings.json. That state is
// produced here by the real binary rather than hand-written, because the hazard
// is a property of the real file — tclaude edits a document it does not own, so
// the seed has to EXTEND the operator's list rather than replace it, keep the
// CLI's own keys, and still parse for the CLI afterwards (the comment lines do
// not survive the round-trip).
//
// It also pins the measurement the whole contract rests on: `trustedFolders` is
// CLI-managed and STAYS in config.json across the migration that moves user
// settings out. A future CLI that relocated the key would leave every seeded
// pane blocked, and must fail here rather than silently.
func TestCopilotDirTrustSeedingPreservesAnInstalledTrustStore(t *testing.T) {
	requireLabParallel(t)

	dirs := copilotfixture.NewSandboxDirs(t)
	existing := filepath.Join(dirs.Root, "already-trusted")
	require.NoError(t, os.MkdirAll(existing, 0o755))

	// Stage a pre-existing install — an accepted folder and a user setting, in
	// the file the CLI keeps them in — then let the CLI settle it.
	configPath := filepath.Join(dirs.Home, "config.json")
	settingsPath := filepath.Join(dirs.Home, "settings.json")
	staged, err := json.Marshal(map[string]any{
		"trustedFolders": []string{existing},
		"theme":          "dark",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, staged, 0o600))

	migrateMock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{{Text: "MIGRATED"}})
	migrate := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: migrateMock.BaseURL(),
		Prompt:  "Reply with the text the provider gives you.",
	})
	require.Equal(t, 0, migrate.ExitCode, "stderr: %s", migrate.Stderr)
	require.Contains(t, readTrustedFolders(t, configPath), existing,
		"trustedFolders must remain in config.json after the startup migration")
	settled := readManagedJSON(t, configPath)
	require.Contains(t, settled, "firstLaunchAt", "the CLI mints its own keys in this file")
	require.NotContains(t, readManagedJSON(t, settingsPath), "trustedFolders",
		"settings.json must not be the trust store")

	// Now seed the launch dir the way production does.
	seedTrustLikeProduction(t, dirs, dirs.WorkDir)
	assert.ElementsMatch(t, []string{existing, dirs.WorkDir},
		readTrustedFolders(t, configPath),
		"seeding must EXTEND the installed list, not replace it")
	assert.Equal(t, settled["firstLaunchAt"], readManagedJSON(t, configPath)["firstLaunchAt"],
		"the CLI's own managed keys must survive tclaude's edit")

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK STREAMED ANSWER"},
	})
	res := dirTrustRun(t, dirs, mock, dirs.WorkDir)
	require.True(t, res.Settled,
		"the seeded launch must clear the modal on an installed home too; transcript:\n%s",
		res.TranscriptText())
	assert.False(t, res.Contains(copilotfixture.TrustPromptMarker))

	// And the operator's own folder is still trusted after that launch: the CLI
	// accepted the rewritten file rather than resetting it.
	assert.Contains(t, readTrustedFolders(t, configPath), existing,
		"the operator's previously trusted folder must survive tclaude's seed")
}

// TestCopilotDirTrustSeedingMatchesTheUnresolvedCwdSpelling makes the seeder's
// two-spelling behaviour measured rather than hedged.
//
// EnsureCopilotDirTrusted seeds both the spelling it is handed and that path's
// symlink-resolved form, because the two ends of the comparison are spelled by
// different parties: tclaude receives the cwd as the operator's shell produced
// it, while Copilot resolves its own. Which side the trust check reads was not
// established, and a wrong guess costs a frozen pane — so both go in.
//
// That hedge is only exercised where the two spellings actually differ, which
// is macOS ($TMPDIR is /var/folders/… for a kernel directory at
// /private/var/folders/…). The scenario therefore hands BOTH the seeder and the
// launch the unresolved spelling, the way a shell would, and skips itself where
// the platform has no second spelling to offer — which is why CI requires it on
// the macOS arm specifically.
func TestCopilotDirTrustSeedingMatchesTheUnresolvedCwdSpelling(t *testing.T) {
	requireLabParallel(t)

	dirs := copilotfixture.NewSandboxDirs(t)
	if dirs.UnresolvedWorkDir == dirs.WorkDir {
		t.Skip("the fixture root has one spelling on this platform; " +
			"the unresolved-cwd case is macOS-shaped (/var vs /private/var)")
	}

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK STREAMED ANSWER"},
	})
	seedTrustLikeProduction(t, dirs, dirs.UnresolvedWorkDir)

	// The premise, and it is falsifiable: given only the /var spelling the
	// seeder must have written the /private/var one too. A change that dropped
	// the resolved form fails HERE, with a clear reason, rather than as an
	// unexplained blocked pane below.
	seeded := readTrustedFolders(t, filepath.Join(dirs.Home, "config.json"))
	assert.ElementsMatch(t, []string{dirs.UnresolvedWorkDir, dirs.WorkDir}, seeded,
		"seeding an unresolved spelling must also seed the resolved one")

	res := dirTrustRun(t, dirs, mock, dirs.UnresolvedWorkDir)
	require.True(t, res.Settled,
		"a launch in the unresolved spelling must clear the modal; transcript:\n%s",
		res.TranscriptText())
	assert.False(t, res.Contains(copilotfixture.TrustPromptMarker))
}
