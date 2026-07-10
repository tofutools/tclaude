package testharness

import (
	"os"
	"path/filepath"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

func TestCheckSpawnDirProofMarkerChecksPinnedRepositoryRoots(t *testing.T) {
	root := t.TempDir()
	args := clcommon.SpawnArgs{
		DirWriteProof:        "root-proof",
		GitWorktreeWriteDirs: []string{root},
	}
	if err := checkSpawnDirProofMarker(args); err == nil {
		t.Fatal("missing repository-root marker was accepted")
	}
	marker := filepath.Join(root, clcommon.SpawnDirWriteProofPrefix+args.DirWriteProof)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkSpawnDirProofMarker(args); err != nil {
		t.Fatalf("valid repository-root marker rejected: %v", err)
	}
}
