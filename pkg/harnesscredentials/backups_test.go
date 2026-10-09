package harnesscredentials

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStandaloneBackupRestoreAndUndo(t *testing.T) {
	home := homeFixture(t)
	dir := filepath.Join(home, ".codex")
	require.NoError(t, os.Mkdir(dir, 0700))
	path := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"token":"old"}`), 0600))
	private := filepath.Join(home, "private")
	yes := func() bool { return true }
	pushed, err := Receive(home, private, Bundle{Harness: "codex", Files: []File{{"auth.json", []byte(`{"token":"new"}`)}}}, true, yes)
	require.NoError(t, err)
	entries, err := ListBackups(private, "codex")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.False(t, entries[0].CreatedAt.IsZero())
	raw, err := json.Marshal(entries)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "token")
	restored, err := Restore(home, private, "codex", "", yes)
	require.NoError(t, err)
	require.True(t, restored.Restored)
	require.Equal(t, pushed.BackupID, restored.RestoredFrom)
	raw, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"token":"old"}`, string(raw))
	_, err = Restore(home, private, "codex", restored.BackupID, yes)
	require.NoError(t, err)
	raw, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"token":"new"}`, string(raw))
	_, err = Restore(home, private, "gemini", pushed.BackupID, yes)
	require.Error(t, err)
	_, err = Restore(home, private, "codex", "../other", yes)
	require.Error(t, err)
	_, err = Restore(home, private, "codex", pushed.BackupID, func() bool { return false })
	require.Error(t, err)
}
func TestRestoreAbsentFilesAndBackupFailurePreservesCurrent(t *testing.T) {
	home := homeFixture(t)
	private := filepath.Join(home, "private")
	yes := func() bool { return true }
	receipt, err := Receive(home, private, Bundle{Harness: "codex", Files: []File{{"auth.json", []byte(`{"token":"new"}`)}}}, true, yes)
	require.NoError(t, err)
	path := filepath.Join(home, ".codex", "auth.json")
	_, err = Restore(home, private, "codex", receipt.BackupID, yes)
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
	require.NoError(t, os.WriteFile(path, []byte(`{"token":"keep"}`), 0600))
	require.NoError(t, os.Rename(private, private+"-moved"))
	require.NoError(t, os.WriteFile(private, []byte("blocked"), 0600))
	_, err = Receive(home, private, Bundle{Harness: "codex", Files: []File{{"auth.json", []byte(`{"token":"new"}`)}}}, true, yes)
	require.Error(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"token":"keep"}`, string(raw))
}
func TestCredentialBackupsRejectTamperedPathsAndLinks(t *testing.T) {
	home := homeFixture(t)
	private := filepath.Join(home, "private")
	yes := func() bool { return true }
	receipt, err := Backup(home, private, "codex", yes)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(receipt.BackupLocation, []byte(`{"harness":"codex","files":[{"name":"../outside","exists":false}]}`), 0600))
	_, err = Restore(home, private, "codex", receipt.BackupID, yes)
	require.Error(t, err)
	entries, err := ListBackups(private, "codex")
	require.NoError(t, err)
	require.Empty(t, entries)
	require.NoError(t, os.Remove(receipt.BackupLocation))
	require.NoError(t, os.Symlink(filepath.Join(home, "missing"), receipt.BackupLocation))
	_, err = Restore(home, private, "codex", receipt.BackupID, yes)
	require.Error(t, err)
}
func TestLargeOriginalBackupAndRevocationRollback(t *testing.T) {
	home := homeFixture(t)
	dir := filepath.Join(home, ".codex")
	require.NoError(t, os.Mkdir(dir, 0700))
	path := filepath.Join(dir, "auth.json")
	original := []byte(`{"token":"` + strings.Repeat("a", MaxBytes-20) + `"}`)
	require.NoError(t, os.WriteFile(path, original, 0600))
	private := filepath.Join(home, "private")
	yes := func() bool { return true }
	receipt, err := Receive(home, private, Bundle{Harness: "codex", Files: []File{{"auth.json", []byte(`{}`)}}}, true, yes)
	require.NoError(t, err)
	_, err = Restore(home, private, "codex", receipt.BackupID, yes)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, raw)
	t.Setenv("GEMINI_CLI_HOME", home)
	dir = filepath.Join(home, ".gemini")
	require.NoError(t, os.Mkdir(dir, 0700))
	for _, f := range []string{"oauth_creds.json", "google_accounts.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte(`{"old":true}`), 0600))
	}
	gemini, err := Backup(home, private, "gemini", yes)
	require.NoError(t, err)
	for _, f := range []string{"oauth_creds.json", "google_accounts.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte(`{"new":true}`), 0600))
	}
	calls := 0
	restored, err := Restore(home, private, "gemini", gemini.BackupID, func() bool { calls++; return calls < 3 })
	require.Error(t, err)
	require.NotEmpty(t, restored.BackupID)
	for _, f := range []string{"oauth_creds.json", "google_accounts.json"} {
		raw, err = os.ReadFile(filepath.Join(dir, f))
		require.NoError(t, err)
		require.Equal(t, `{"new":true}`, string(raw))
	}
}
