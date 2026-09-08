//go:build linux || darwin

package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRelayImportVerifiesRetainedHistoryBeforeServerStart(t *testing.T) {
	for _, scenario := range []string{"exact", "changed-input", "changed-export", "wrong-directory", "missing-point", "native-error"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			input, output := filepath.Join(root, "input.json"), filepath.Join(root, "output.json")
			writeOpenCodeExport(t, input, "ses_fixture", root, "second answer")
			raw, err := os.ReadFile(input)
			require.NoError(t, err)
			digest := sha256.Sum256(raw)
			selected := ServerRelayImport{Path: input, Digest: hex.EncodeToString(digest[:]), NativeID: "ses_fixture", WorkingDir: root}
			require.NoError(t, os.WriteFile(output, raw, 0600))
			marker := filepath.Join(root, "imported")
			executable := filepath.Join(root, "native-fixture")
			script := `#!/bin/sh
case "$1" in
import)
  printf imported > "$IMPORT_MARKER"
  if [ "$IMPORT_FAIL" = yes ]; then printf 'private conversation text' >&2; exit 1; fi
  ;;
export) cat "$IMPORT_OUTPUT" ;;
*) exit 2 ;;
esac
`
			require.NoError(t, os.WriteFile(executable, []byte(script), 0700))
			t.Setenv("IMPORT_MARKER", marker)
			t.Setenv("IMPORT_OUTPUT", output)
			t.Setenv("IMPORT_FAIL", "no")
			switch scenario {
			case "changed-input":
				require.NoError(t, os.WriteFile(input, append(raw, '\n'), 0600))
			case "changed-export":
				writeOpenCodeExport(t, output, "ses_fixture", root, "changed answer")
			case "wrong-directory":
				writeOpenCodeExport(t, output, "ses_fixture", filepath.Join(root, "other"), "second answer")
			case "missing-point":
				selected.BeforeMessage = "msg_missing"
			case "native-error":
				t.Setenv("IMPORT_FAIL", "yes")
			}
			err = importRelayHistory(context.Background(), executable, selected)
			if scenario == "exact" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "private conversation text")
			}
			if scenario == "changed-input" || scenario == "missing-point" {
				require.NoFileExists(t, marker, "invalid retained intent must fail before native import")
			} else {
				require.FileExists(t, marker)
			}
		})
	}
}

func TestRelayImportRetainsExactBeforeMessageSelection(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "history.json")
	writeOpenCodeExport(t, path, "ses_fixture", root, "answer")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	digest := sha256.Sum256(raw)
	executable := filepath.Join(root, "native-fixture")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\n[ \"$1\" = import ] && exit 0\ncat \"$IMPORT_OUTPUT\"\n"), 0700))
	t.Setenv("IMPORT_OUTPUT", path)
	require.NoError(t, importRelayHistory(context.Background(), executable, ServerRelayImport{
		Path: path, Digest: hex.EncodeToString(digest[:]), NativeID: "ses_fixture", WorkingDir: root, BeforeMessage: "msg_two",
	}))
}
