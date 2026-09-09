//go:build linux || darwin

package copilot

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderDirectoryTrustIsExplicitAndMalformedStoreIsNonfatal(t *testing.T) {
	for _, kind := range []string{"off", "on", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			root, err := os.MkdirTemp("", "trust-")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			nativeHome := filepath.Join(root, "native")
			require.NoError(t, os.Mkdir(nativeHome, 0700))
			initial := []byte("{\"operator\":\"retained\"}")
			if kind == "malformed" {
				initial = []byte("{\"trustedFolders\":1}")
			}
			path := filepath.Join(nativeHome, "config.json")
			require.NoError(t, os.WriteFile(path, initial, 0600))
			provider, err := New(Config{Executable: os.Args[0], PrivateRoot: root, NativeHome: nativeHome})
			require.NoError(t, err)
			prepared, err := provider.Prepare(ctx, ports.PreparationRequest{Intent: ports.StartFresh, Spec: model.ResolvedExecutionSpec{ExecutionID: "execution", Attempt: 1, Harness: Name, Model: "fixture", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined, WorkingDirectory: root, TrustDirectory: kind != "off"}})
			require.NoError(t, err)
			require.NoError(t, prepared.Abort(ctx))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			if kind == "on" {
				require.NotEqual(t, initial, after)
				require.Contains(t, string(after), root)
			} else {
				require.Equal(t, initial, after)
			}
		})
	}
}
