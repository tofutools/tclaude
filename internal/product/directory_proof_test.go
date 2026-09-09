package product

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/client"
)

func TestDirectoryProofClientPreservesExactIntentAndCleansMarkers(t *testing.T) {
	root := t.TempDir()
	token := "0123456789abcdef0123456789abcdef"
	proof := &client.DirectoryWriteProof{Token: token, Filename: ".tclaude-write-proof-" + token, Directories: []string{root}}
	marker := filepath.Join(root, proof.Filename)
	calls := 0
	call := func(_ context.Context, method, path string, body, result any) error {
		calls++
		require.Equal(t, http.MethodPost, method)
		require.Equal(t, "/v2/launch", path)
		if calls == 1 {
			return &client.Error{Status: 403, Code: "write_proof_required", WriteProof: proof}
		}
		require.FileExists(t, marker)
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"expected_revision":9007199254740993`)
		require.Contains(t, string(raw), `"write_proof_token":"`+token+`"`)
		return errors.New("retry outcome")
	}
	err := callWithDirectoryProof(context.Background(), call, "POST", "/v2/launch", json.RawMessage(`{"request_id":"same","expected_revision":9007199254740993}`), nil)
	require.EqualError(t, err, "retry outcome")
	require.Equal(t, 2, calls)
	require.NoFileExists(t, marker)
}

func TestDirectoryProofClientDoesNotOverwriteExistingMarkerOrRetryOtherFailures(t *testing.T) {
	root := t.TempDir()
	token := "0123456789abcdef0123456789abcdef"
	proof := &client.DirectoryWriteProof{Token: token, Filename: ".tclaude-write-proof-" + token, Directories: []string{root}}
	marker := filepath.Join(root, proof.Filename)
	require.NoError(t, os.WriteFile(marker, []byte("existing"), 0600))
	for _, code := range []string{"write_proof_required", "conflict", "uncertain"} {
		calls := 0
		call := func(context.Context, string, string, any, any) error {
			calls++
			return &client.Error{Status: 403, Code: code, WriteProof: proof}
		}
		require.Error(t, callWithDirectoryProof(context.Background(), call, "POST", "/v2/launch", map[string]string{"request_id": "same"}, nil))
		require.Equal(t, 1, calls)
		content, err := os.ReadFile(marker)
		require.NoError(t, err)
		require.Equal(t, "existing", string(content))
	}
}
