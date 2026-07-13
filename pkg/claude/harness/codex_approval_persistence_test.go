package harness

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCodexAppID = "asdk_app_69a089a326dc8191b32a3f2553f5be2c"

func sealedTestProfile(t *testing.T, home, launchID string) (string, string) {
	t.Helper()
	t.Setenv("CODEX_HOME", home)
	name, path, err := EnsureCodexAgentLaunchProfile([]string{"/tmp/work"}, launchID)
	require.NoError(t, err)
	return name, path
}

func appendTestApproval(t *testing.T, path, tool string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("\n[tui.model_availability_nux]\n\"gpt-test\" = 1\n\n" +
		"[apps." + testCodexAppID + ".tools.\"" + tool + "\"]\n" +
		"approval_mode = \"approve\"\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestExtractCodexLaunchProfileApprovals_VerifiedExactAddition(t *testing.T) {
	home := t.TempDir()
	_, path := sealedTestProfile(t, home, "1111111111111111")
	appendTestApproval(t, path, "linear.save_issue")
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	got, err := ExtractCodexLaunchProfileApprovals(data)
	require.NoError(t, err)
	require.Equal(t, []CodexToolApproval{{AppID: testCodexAppID, Tool: "linear.save_issue"}}, got)
}

func TestExtractCodexLaunchProfileApprovals_ClassifiesUnsealedProfile(t *testing.T) {
	_, err := ExtractCodexLaunchProfileApprovals([]byte("model = \"still-being-written\"\n"))
	require.Error(t, err)
	assert.True(t, IsCodexLaunchProfileNotSealed(err))
}

func TestExtractCodexLaunchProfileApprovals_BaselineMutationFailsClosed(t *testing.T) {
	home := t.TempDir()
	_, path := sealedTestProfile(t, home, "2222222222222222")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data = []byte(strings.Replace(string(data), `extends = ":workspace"`, `extends = ":danger"`, 1))
	data = append(data, []byte("\n[apps."+testCodexAppID+".tools.\"linear.save_issue\"]\napproval_mode = \"approve\"\n")...)

	_, err = ExtractCodexLaunchProfileApprovals(data)
	require.ErrorContains(t, err, "baseline changed")
}

func TestExtractCodexLaunchProfileApprovals_IgnoresNonExactToolConfig(t *testing.T) {
	home := t.TempDir()
	_, path := sealedTestProfile(t, home, "3333333333333333")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("\n[apps." + testCodexAppID + ".tools.\"linear.save_issue\"]\n" +
		"approval_mode = \"approve\"\nenabled = true\n\n" +
		"[apps." + testCodexAppID + ".tools.\"linear.save_comment\"]\n" +
		"approval_mode = \"prompt\"\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	got, err := ExtractCodexLaunchProfileApprovals(data)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestPromoteCodexLaunchProfileApprovals_MergeAndIdempotence(t *testing.T) {
	home := t.TempDir()
	_, path := sealedTestProfile(t, home, "4444444444444444")
	appendTestApproval(t, path, "linear.save_issue")
	configPath := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("# keep me\nmodel = \"gpt-test\"\n"), 0o640))

	report, err := PromoteCodexLaunchProfileApprovals(path)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Added)
	config, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Contains(t, string(config), "# keep me")
	assert.Contains(t, string(config), `[apps.`+testCodexAppID+`.tools."linear.save_issue"]`)
	assert.Contains(t, string(config), `approval_mode = "approve"`)
	info, err := os.Stat(configPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())

	report, err = PromoteCodexLaunchProfileApprovals(path)
	require.NoError(t, err)
	assert.Equal(t, 0, report.Added)
	assert.Equal(t, 1, report.Existing)
}

func TestPromoteCodexLaunchProfileApprovals_ExistingDecisionWins(t *testing.T) {
	home := t.TempDir()
	_, path := sealedTestProfile(t, home, "5555555555555555")
	appendTestApproval(t, path, "linear.save_issue")
	configPath := filepath.Join(home, "config.toml")
	original := "[apps." + testCodexAppID + ".tools.\"linear.save_issue\"]\napproval_mode = \"prompt\"\n"
	require.NoError(t, os.WriteFile(configPath, []byte(original), 0o600))

	report, err := PromoteCodexLaunchProfileApprovals(path)
	require.NoError(t, err)
	assert.Zero(t, report.Added)
	require.Len(t, report.Conflicts, 1)
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, original, string(after))
}

func TestPromoteCodexLaunchProfileApprovals_InlineTablesFailWithoutChangingConfig(t *testing.T) {
	for name, original := range map[string]string{
		"inline apps":  "apps = {}\n",
		"inline tools": "[apps." + testCodexAppID + "]\ntools = {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			_, path := sealedTestProfile(t, home, "7777777777777777")
			appendTestApproval(t, path, "linear.save_issue")
			configPath := filepath.Join(home, "config.toml")
			require.NoError(t, os.WriteFile(configPath, []byte(original), 0o600))

			_, err := PromoteCodexLaunchProfileApprovals(path)
			require.ErrorContains(t, err, "conflict with existing Codex config shape")
			after, readErr := os.ReadFile(configPath)
			require.NoError(t, readErr)
			assert.Equal(t, original, string(after))
		})
	}
}

func TestCodexConfigWriters_DoNotLoseConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	approval := CodexToolApproval{AppID: testCodexAppID, Tool: "linear.save_issue"}

	start := make(chan struct{})
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errCh <- ensureDirTrustedInFile(configPath, "/proj/concurrent")
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := mergeCodexToolApprovals(configPath, []CodexToolApproval{approval})
		errCh <- err
	}()
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `[projects."/proj/concurrent"]`)
	assert.Contains(t, string(data), `[apps.`+testCodexAppID+`.tools."linear.save_issue"]`)
}

func TestPromoteCodexLaunchProfileApprovals_PreservesConfigSymlink(t *testing.T) {
	home := t.TempDir()
	_, path := sealedTestProfile(t, home, "aaaaaaaaaaaaaaaa")
	appendTestApproval(t, path, "linear.save_issue")
	target := filepath.Join(home, "real-config.toml")
	require.NoError(t, os.WriteFile(target, []byte("# target\n"), 0o600))
	configPath := filepath.Join(home, "config.toml")
	require.NoError(t, os.Symlink(target, configPath))

	report, err := PromoteCodexLaunchProfileApprovals(path)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Added)
	info, err := os.Lstat(configPath)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Contains(t, string(data), "linear.save_issue")
}

func TestCodexProfileSeal_OnlyLaunchSpecificProfiles(t *testing.T) {
	launch, err := codexAgentProfileContentForNameAndRules(
		CodexAgentProfile+"-6666666666666666", "/tmp/agentd.sock", "/tmp/private", nil, nil, nil)
	require.NoError(t, err)
	assert.Contains(t, launch, codexAgentProfileBaselineMarker)
	base, err := codexAgentProfileContentForNameAndRules(
		CodexAgentProfile, "/tmp/agentd.sock", "/tmp/private", nil, nil, nil)
	require.NoError(t, err)
	assert.NotContains(t, base, codexAgentProfileBaselineMarker)
}
