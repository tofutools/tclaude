package conv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// These pin what `conv cp`, `conv mv` and `conv delete` print and change,
// through the real Run functions: how the source conversation is found (in
// the current directory's project, or in every project with -g), what an
// unknown id answers, and what each leaves on disk and in sessions-index.json.

const pinConvID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

type convPin struct {
	src, dst         string // real project directories
	srcProj, dstProj string // their Claude project directories
}

// haveConversation sets up a fresh home with one conversation, plus a
// sidecar directory, in a src project, and an empty dst project directory
// with a legacy sessions-index.json for the commands to update.
func haveConversation(t *testing.T) convPin {
	t.Helper()
	setupTestDB(t)
	root := testutil.CanonicalTempDir(t)
	p := convPin{src: filepath.Join(root, "src"), dst: filepath.Join(root, "dst")}
	require.NoError(t, os.MkdirAll(p.src, 0o755))
	require.NoError(t, os.MkdirAll(p.dst, 0o755))
	p.srcProj, p.dstProj = GetClaudeProjectPath(p.src), GetClaudeProjectPath(p.dst)
	require.NoError(t, os.MkdirAll(filepath.Join(p.srcProj, pinConvID, "subagents"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(p.srcProj, pinConvID, "subagents", "a.jsonl"), []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(p.srcProj, pinConvID+".jsonl"), []byte(
		`{"type":"user","sessionId":"`+pinConvID+`","cwd":"`+p.src+`","gitBranch":"main","message":{"role":"user","content":"pin prompt"},"timestamp":"2024-01-01T00:00:00Z"}
{"type":"custom-title","customTitle":"Pinned","sessionId":"`+pinConvID+`"}
`), 0o644))
	require.NoError(t, os.MkdirAll(p.dstProj, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(p.dstProj, "sessions-index.json"), []byte(`{"version":1,"entries":[]}`), 0o644))
	return p
}

// runConv runs one of the conv Run functions with file-backed stdio.
func runConv(t *testing.T, stdin string, run func(stdout, stderr, stdin *os.File) int) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	open := func(name, content string) *os.File {
		f, err := os.Create(filepath.Join(dir, name))
		require.NoError(t, err)
		_, err = f.WriteString(content)
		require.NoError(t, err)
		_, err = f.Seek(0, 0)
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	out, errf, in := open("out", ""), open("err", ""), open("in", stdin)
	rc := run(out, errf, in)
	read := func(f *os.File) string {
		b, err := os.ReadFile(f.Name())
		require.NoError(t, err)
		return string(b)
	}
	return rc, read(out), read(errf)
}

// legacyIndexEntries reads a project's sessions-index.json entries.
func legacyIndexEntries(t *testing.T, proj string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(proj, "sessions-index.json"))
	require.NoError(t, err)
	var idx struct {
		Entries []map[string]any `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(raw, &idx))
	return idx.Entries
}

var copiedLine = regexp.MustCompile(`^Copied conversation aaaaaaaa -> ([0-9a-f]{8}) in (.+)\n$`)

// copiedConvID is the id of the one conversation file in the dst project.
func copiedConvID(t *testing.T, p convPin) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(p.dstProj, "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	return strings.TrimSuffix(filepath.Base(matches[0]), ".jsonl")
}

func TestRunCp_CopiesTheConversationUnderANewID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global bool
	}{{"from the current directory", false}, {"from any project with -g", true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := haveConversation(t)
			if !tc.global {
				t.Chdir(p.src)
			}

			rc, out, errOut := runConv(t, "", func(o, e, i *os.File) int {
				return RunCp(&CpParams{ConvID: "aaaaaaaa", DestPath: p.dst, Global: tc.global}, o, e, i)
			})

			require.Equal(t, 0, rc, errOut)
			assert.Empty(t, errOut)
			m := copiedLine.FindStringSubmatch(out)
			require.NotNil(t, m, "stdout: %q", out)
			assert.Equal(t, p.dst, m[2])
			newID := copiedConvID(t, p)
			assert.Equal(t, newID[:8], m[1])
			assert.NotEqual(t, pinConvID, newID)

			body, err := os.ReadFile(filepath.Join(p.dstProj, newID+".jsonl"))
			require.NoError(t, err)
			assert.NotContains(t, string(body), pinConvID, "the copy's session id is rewritten")
			assert.Contains(t, string(body), `"sessionId":"`+newID+`"`)
			assert.FileExists(t, filepath.Join(p.dstProj, newID, "subagents", "a.jsonl"), "the sidecar directory is copied under the new id")
			assert.FileExists(t, filepath.Join(p.srcProj, pinConvID+".jsonl"), "the source stays")

			entries := legacyIndexEntries(t, p.dstProj)
			require.Len(t, entries, 1)
			assert.Equal(t, newID, entries[0]["sessionId"])
			assert.Equal(t, p.dst, entries[0]["projectPath"])
			assert.Equal(t, "Pinned", entries[0]["customTitle"])
			assert.Equal(t, filepath.Join(p.dstProj, newID+".jsonl"), entries[0]["fullPath"])
		})
	}
}

// An unknown id is not found; outside -g the answer hints at it. The same
// answer from cp, mv and delete, which share the lookup.
func TestConvRunners_AnUnknownConversationIsNotFound(t *testing.T) {
	runners := map[string]func(global bool, dst string) func(o, e, i *os.File) int{
		"cp": func(global bool, dst string) func(o, e, i *os.File) int {
			return func(o, e, i *os.File) int {
				return RunCp(&CpParams{ConvID: "ffffffff", DestPath: dst, Global: global}, o, e, i)
			}
		},
		"mv": func(global bool, dst string) func(o, e, i *os.File) int {
			return func(o, e, i *os.File) int {
				return RunMv(&MvParams{ConvID: "ffffffff", DestPath: dst, Global: global}, o, e, i)
			}
		},
		"delete": func(global bool, _ string) func(o, e, i *os.File) int {
			return func(o, e, i *os.File) int {
				return RunDelete(&DeleteParams{ConvID: "ffffffff", Yes: true, Global: global}, o, e, i)
			}
		},
	}
	for name, runner := range runners {
		for _, global := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "", true: " -g"}[global], func(t *testing.T) {
				p := haveConversation(t)
				t.Chdir(p.src)

				rc, out, errOut := runConv(t, "", runner(global, p.dst))

				assert.Equal(t, 1, rc)
				assert.Empty(t, out)
				want := "Conversation ffffffff not found\n"
				if !global {
					want += "Hint: use -g to search all projects\n"
				}
				assert.Equal(t, want, errOut)
				assert.FileExists(t, filepath.Join(p.srcProj, pinConvID+".jsonl"), "nothing is touched")
			})
		}
	}
}

func TestRunMv_MovesTheConversationFoundInAnotherProject(t *testing.T) {
	p := haveConversation(t)

	rc, out, errOut := runConv(t, "", func(o, e, i *os.File) int {
		return RunMv(&MvParams{ConvID: "aaaaaaaa", DestPath: p.dst, Global: true}, o, e, i)
	})

	require.Equal(t, 0, rc, errOut)
	assert.Equal(t, "Moved conversation aaaaaaaa to "+p.dst+"\n", out)
	assert.Empty(t, errOut)
	assert.NoFileExists(t, filepath.Join(p.srcProj, pinConvID+".jsonl"))
	assert.FileExists(t, filepath.Join(p.dstProj, pinConvID+".jsonl"))
	assert.FileExists(t, filepath.Join(p.dstProj, pinConvID, "subagents", "a.jsonl"))
	entries := legacyIndexEntries(t, p.dstProj)
	require.Len(t, entries, 1)
	assert.Equal(t, pinConvID, entries[0]["sessionId"])
	assert.Equal(t, p.dst, entries[0]["projectPath"])
}

func TestRunDelete_DeletesTheConversationFoundInTheCurrentDirectory(t *testing.T) {
	p := haveConversation(t)
	t.Chdir(p.src)

	rc, out, errOut := runConv(t, "", func(o, e, i *os.File) int {
		return RunDelete(&DeleteParams{ConvID: "aaaaaaaa", Yes: true}, o, e, i)
	})

	require.Equal(t, 0, rc, errOut)
	assert.Equal(t, "Conversation: aaaaaaaa\nProject:      "+p.src+"\nTitle/Prompt: Pinned\nMessages:     0\nDeleted conversation aaaaaaaa\n", out)
	assert.NoFileExists(t, filepath.Join(p.srcProj, pinConvID+".jsonl"))
}

func TestRunDelete_AskedNoKeepsTheConversation(t *testing.T) {
	p := haveConversation(t)

	rc, out, _ := runConv(t, "n\n", func(o, e, i *os.File) int {
		return RunDelete(&DeleteParams{ConvID: "aaaaaaaa", Global: true}, o, e, i)
	})

	assert.Equal(t, 0, rc)
	assert.True(t, strings.HasSuffix(out, "\nDelete this conversation? [y/N]: Aborted.\n"), out)
	assert.FileExists(t, filepath.Join(p.srcProj, pinConvID+".jsonl"))
}

// A copy is in conv_index as soon as cp returns, as the library copy that
// clone and `worktree add` use already made it, so a lookup straight after
// `conv cp` finds it without waiting for a rescan.
func TestRunCp_TheCopyIsIndexedAtOnce(t *testing.T) {
	p := haveConversation(t)

	rc, _, errOut := runConv(t, "", func(o, e, i *os.File) int {
		return RunCp(&CpParams{ConvID: "aaaaaaaa", DestPath: p.dst, Global: true}, o, e, i)
	})

	require.Equal(t, 0, rc, errOut)
	row, err := db.GetConvIndex(copiedConvID(t, p))
	require.NoError(t, err)
	require.NotNil(t, row, "the copy is in conv_index")
	assert.Equal(t, "Pinned", row.CustomTitle)
}
