package resumeprovenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureEncodeDecodeAndCompare(t *testing.T) {
	cwd := t.TempDir()
	captured, err := Capture(cwd)
	require.NoError(t, err)
	assert.Equal(t, RepositoryNone, captured.RepositoryState)
	assert.NotZero(t, captured.Cwd.Device)
	assert.NotZero(t, captured.Cwd.Inode)

	raw, err := Encode(captured)
	require.NoError(t, err)
	decoded, err := Decode(raw)
	require.NoError(t, err)
	assert.Equal(t, captured, decoded)
	require.NoError(t, Compare(captured, decoded))
}

func TestDecodeRejectsUntrustedShapes(t *testing.T) {
	valid := `{"version":1,"cwd":{"path":"/tmp/x","device":1,"inode":2},"repository_state":"none"}`
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "missing", raw: "", want: "missing"},
		{name: "unknown field", raw: strings.TrimSuffix(valid, "}") + `,"extra":true}`, want: "unknown field"},
		{name: "trailing value", raw: valid + `{}`, want: "trailing"},
		{name: "future version", raw: strings.Replace(valid, `"version":1`, `"version":2`, 1), want: "unsupported"},
		{name: "zero device", raw: strings.Replace(valid, `"device":1`, `"device":0`, 1), want: "filesystem identity"},
		{name: "git without identity", raw: strings.Replace(valid, `"repository_state":"none"`, `"repository_state":"git"`, 1), want: "missing repository"},
		{name: "none with identity", raw: strings.TrimSuffix(valid, "}") + `,"repository":{"dir":{"path":"/tmp/d","device":1,"inode":2},"common_dir":{"path":"/tmp/c","device":1,"inode":3}}}`, want: "has repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.raw)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
	_, err := Decode(strings.Repeat("x", MaxEncodedLen+1))
	assert.ErrorContains(t, err, "exceeds")
}

func TestCompareDetectsCwdReplacementAndSymlinkRetarget(t *testing.T) {
	parent := t.TempDir()
	original := filepath.Join(parent, "original")
	other := filepath.Join(parent, "other")
	require.NoError(t, os.Mkdir(original, 0o755))
	require.NoError(t, os.Mkdir(other, 0o755))
	link := filepath.Join(parent, "launch")
	require.NoError(t, os.Symlink(original, link))
	expected, err := Capture(link)
	require.NoError(t, err)

	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(other, link))
	retargeted, err := Capture(link)
	require.NoError(t, err)
	assert.ErrorContains(t, Compare(expected, retargeted), "cwd path changed")

	// Replacing the original at the same canonical pathname changes its inode.
	require.NoError(t, os.Rename(original, original+"-old"))
	require.NoError(t, os.Mkdir(original, 0o755))
	replaced, err := Capture(original)
	require.NoError(t, err)
	assert.ErrorContains(t, Compare(expected, replaced), "filesystem identity changed")
}

func TestCompareDetectsGitMetadataReplacement(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	expected, err := Capture(repo)
	require.NoError(t, err)
	require.NotNil(t, expected.Repository)

	oldGit := filepath.Join(filepath.Dir(repo), "old-git")
	require.NoError(t, os.Rename(filepath.Join(repo, ".git"), oldGit))
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	actual, err := Capture(repo)
	require.NoError(t, err)
	assert.Error(t, Compare(expected, actual), "same pathname with replacement Git metadata must fail")
}

func TestCompareDetectsGitFileIndirectionRetarget(t *testing.T) {
	root := t.TempDir()
	mainRepo := filepath.Join(root, "main")
	worktree := filepath.Join(root, "worktree")
	require.NoError(t, os.Mkdir(mainRepo, 0o755))
	require.NoError(t, exec.Command("git", "init", "-q", mainRepo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, "seed"), []byte("seed"), 0o600))
	for _, args := range [][]string{
		{"-C", mainRepo, "config", "user.email", "test@example.com"},
		{"-C", mainRepo, "config", "user.name", "Test"},
		{"-C", mainRepo, "add", "seed"},
		{"-C", mainRepo, "commit", "-q", "-m", "seed"},
		{"-C", mainRepo, "worktree", "add", "-q", worktree},
	} {
		require.NoError(t, exec.Command("git", args...).Run())
	}
	expected, err := Capture(worktree)
	require.NoError(t, err)
	require.NotNil(t, expected.Repository)

	other := filepath.Join(root, "other")
	require.NoError(t, os.Mkdir(other, 0o755))
	require.NoError(t, exec.Command("git", "init", "-q", other).Run())
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o600))
	actual, err := Capture(worktree)
	require.NoError(t, err)
	assert.Error(t, Compare(expected, actual), "retargeting a linked-worktree .git file must fail")
}
