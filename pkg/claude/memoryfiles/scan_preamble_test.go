package memoryfiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

// Every memory-files command starts by resolving the target's project dirs
// the same way. These pin what each prints and returns there, so the four
// share one preamble without any of them changing.
type scanCommand struct {
	name string
	run  func(t *testing.T, dir string, prefix bool, stdout, stderr *os.File) int
}

func scanCommands() []scanCommand {
	return []scanCommand{
		{"cat", func(t *testing.T, dir string, prefix bool, stdout, stderr *os.File) int {
			return RunCat(&CatParams{Dir: dir, Prefix: prefix}, stdout, stderr)
		}},
		{"ls", func(t *testing.T, dir string, prefix bool, stdout, stderr *os.File) int {
			return RunLs(&LsParams{Dir: dir, Prefix: prefix}, stdout, stderr)
		}},
		{"clean", func(t *testing.T, dir string, prefix bool, stdout, stderr *os.File) int {
			return RunClean(&CleanParams{Dir: dir, Prefix: prefix, Yes: true}, stdout, stderr, tmpStream(t, ""))
		}},
		{"prune-index", func(t *testing.T, dir string, prefix bool, stdout, stderr *os.File) int {
			return RunPruneIndex(&PruneIndexParams{Dir: dir, Prefix: prefix, Yes: true}, stdout, stderr, tmpStream(t, ""))
		}},
	}
}

func TestScanPreambleNoProjectDirs(t *testing.T) {
	const target = "/work/proj"
	for _, c := range scanCommands() {
		t.Run(c.name, func(t *testing.T) {
			memEnv(t, target, nil)
			stdout, stderr := tmpStream(t, ""), tmpStream(t, "")
			require.Equal(t, 0, c.run(t, target, false, stdout, stderr))
			assert.Equal(t, "No Claude project directories found for /work/proj (encoded: "+convops.PathToProjectDir(target)+").\n",
				readStream(t, stdout))
			// Not a git worktree: the scan falls back to the exact dir, and says so.
			assert.Equal(t, "Note: /work/proj is not a git worktree (or git is unavailable); scanning the exact project dir only — use --prefix to scan sibling dirs by encoded-name prefix.\n",
				readStream(t, stderr))
		})
	}
}

func TestScanPreambleRefusesADegenerateTarget(t *testing.T) {
	for _, c := range scanCommands() {
		t.Run(c.name, func(t *testing.T) {
			memEnv(t, "/", nil)
			stdout, stderr := tmpStream(t, ""), tmpStream(t, "")
			require.Equal(t, 1, c.run(t, "/", true, stdout, stderr))
			assert.Empty(t, readStream(t, stdout))
			assert.Regexp(t, `^Error: .*degenerate.*\n$`, readStream(t, stderr))
		})
	}
}

func TestScanPreambleNoMemoryFiles(t *testing.T) {
	const target = "/work/proj"
	for _, c := range scanCommands() {
		if c.name == "prune-index" {
			continue // it reads each dir's index, not the file list
		}
		t.Run(c.name, func(t *testing.T) {
			projects := memEnv(t, target, nil)
			require.NoError(t, os.MkdirAll(filepath.Join(projects, convops.PathToProjectDir(target)), 0o755))
			stdout, stderr := tmpStream(t, ""), tmpStream(t, "")
			require.Equal(t, 0, c.run(t, target, false, stdout, stderr))
			assert.Equal(t, "No memory files found under 1 matched project dir(s).\n", readStream(t, stdout))
		})
	}
}
