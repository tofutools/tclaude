//go:build linux || darwin

package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxBootstrapHelper(t *testing.T) {
	path := os.Getenv("TCLAUDE_BOOTSTRAP_ARTIFACT")
	if path == "" {
		t.Skip("invoked only as an isolated native bootstrap")
	}
	require.NoError(t, ExecuteSandboxChild(context.Background(), SandboxChildArtifact{Path: path, Digest: os.Getenv("TCLAUDE_BOOTSTRAP_DIGEST")}))
}

func TestSandboxChildArtifactPreservesIntentAndRefusesChangedSource(t *testing.T) {
	root := t.TempDir()
	private, public := filepath.Join(root, "private"), filepath.Join(root, "public")
	require.NoError(t, os.Mkdir(private, 0700))
	require.NoError(t, os.Mkdir(public, 0700))
	inspector, err := NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	bound, err := inspector.BindSandboxMounts(context.Background(), []model.SandboxFilesystemRule{{HostPath: public, Access: model.SandboxFilesystemWrite}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bound.Close() })
	child := ProcessSpec{Executable: "/bin/sh", Args: []string{"-c", "printf '%s' \"$LITERAL\""}, Directory: public, ExactEnvironment: true, Env: []string{"LITERAL=$HOME stays literal"}}
	artifact, err := inspector.PrepareSandboxChild(private, "/usr/bin/wrapper", child, bound, true)
	require.NoError(t, err)
	_, err = inspector.PrepareSandboxChild(private, "/usr/bin/wrapper", child, bound, true)
	require.Error(t, err, "preparation must not overwrite an existing native command")
	require.NoError(t, VerifySandboxChild(context.Background(), artifact))
	input, _, err := readSandboxChild(artifact)
	require.NoError(t, err)
	require.Equal(t, child.Args, input.Arguments)
	require.Equal(t, child.Env, input.Environment)
	invocation, err := artifact.Invocation("/usr/bin/bootstrap")
	require.NoError(t, err)
	require.Empty(t, invocation.Env)
	require.True(t, invocation.ExactEnvironment)

	require.NoError(t, os.Rename(public, public+"-retained"))
	require.NoError(t, os.Mkdir(public, 0700))
	require.ErrorContains(t, VerifySandboxChild(context.Background(), artifact), "identity changed")
	require.NoFileExists(t, artifact.Path+".started", "verification cannot claim or launch the native command")
	require.NoError(t, os.WriteFile(artifact.Path, []byte(`{"Version":1}`), 0600))
	require.ErrorContains(t, VerifySandboxChild(context.Background(), artifact), "artifact identity changed")
}
