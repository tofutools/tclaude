package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestProcessCLIRequiresExplicitDocumentAndPreservesRequest(t *testing.T) {
	root := &cobra.Command{Use: "test", SilenceUsage: true, SilenceErrors: true}
	calls := 0
	registerOrchestration(root, func(_ *cobra.Command, method, path string, body any) error {
		calls++
		require.Equal(t, "POST", method)
		require.Equal(t, "/v2/processes", path)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(body.(json.RawMessage), &decoded))
		require.Equal(t, "stable-request", decoded["request_id"])
		return nil
	})
	root.SetArgs([]string{"process", "start"})
	require.Error(t, root.Execute())
	require.Zero(t, calls)
	file := filepath.Join(t.TempDir(), "start.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"request_id":"stable-request","id":"run","start":{"Deadline":"2026-09-07T00:00:00Z"}}`), 0600))
	root.SetArgs([]string{"process", "start", "--file", file})
	require.NoError(t, root.Execute())
	require.Equal(t, 1, calls)
}
