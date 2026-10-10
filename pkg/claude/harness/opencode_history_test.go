package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// This helper runs only as the CLI subprocess, not as a test in the parent.
func TestOpenCodeHistoryCLI(t *testing.T) {
	if os.Getenv("TCLAUDE_OPENCODE_HISTORY_HELPER") != "1" {
		t.Skip("native CLI subprocess")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	root := filepath.Join(os.Getenv("HOME"), "native-fixture")
	_ = os.MkdirAll(root, 0700)
	path := func(id string) string { return filepath.Join(root, id+".json") }
	fail := func(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
	switch args[0] {
	case "export":
		raw, e := os.ReadFile(path(args[1]))
		if e != nil {
			fail(e)
		}
		_, _ = os.Stdout.Write(raw)
	case "import":
		raw, e := os.ReadFile(args[1])
		if e != nil {
			fail(e)
		}
		var d struct{ Info struct{ ID string } }
		if e = json.Unmarshal(raw, &d); e != nil {
			fail(e)
		}
		if e = os.WriteFile(path(d.Info.ID), raw, 0600); e != nil {
			fail(e)
		}
	case "session":
		if args[1] == "delete" {
			if e := os.Remove(path(args[2])); e != nil {
				fail(e)
			}
		} else {
			rows := []map[string]any{}
			entries, _ := os.ReadDir(root)
			for _, entry := range entries {
				raw, _ := os.ReadFile(filepath.Join(root, entry.Name()))
				var d struct {
					Info struct{ ID, Directory string }
				}
				_ = json.Unmarshal(raw, &d)
				rows = append(rows, map[string]any{"id": d.Info.ID, "directory": d.Info.Directory, "title": "memory", "created": 1, "updated": 1})
			}
			_ = json.NewEncoder(os.Stdout).Encode(rows)
		}
	default:
		fail(fmt.Errorf("unsupported CLI command"))
	}
	os.Exit(0)
}
func TestOpenCodeHistoryTransferNativeProjection(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("TCLAUDE_OPENCODE_HISTORY_HELPER", "1")
	bin := filepath.Join(home, "bin")
	require.NoError(t, os.Mkdir(bin, 0700))
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(os.Args[0], "'", "'\\''") + "' -test.run=^TestOpenCodeHistoryCLI$ -- \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "opencode"), []byte(script), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cwd := testutil.CanonicalTempDir(t)
	root := filepath.Join(home, "native-fixture")
	require.NoError(t, os.Mkdir(root, 0700))
	const source = "ses_source"
	raw := []byte(`{"info":{"id":"ses_source","directory":"/source","permission":[{"action":"allow"}],"share":{"url":"old"}},"messages":[{"info":{"id":"msg_user","sessionID":"ses_source","role":"user"},"parts":[{"id":"prt_user","sessionID":"ses_source","messageID":"msg_user","type":"text","text":"keep /source and ses_source literally"}]},{"info":{"id":"msg_assistant","sessionID":"ses_source","parentID":"msg_user","path":{"cwd":"/source","root":"/source"},"role":"assistant"},"parts":[{"id":"prt_assistant","sessionID":"ses_source","messageID":"msg_assistant","type":"text","text":"remote result"}]}]}`)
	require.NoError(t, os.WriteFile(filepath.Join(root, source+".json"), raw, 0600))
	h := openCodeHistory{}
	wire, err := h.Export(source, cwd)
	require.NoError(t, err)
	require.NoError(t, h.Validate(wire, source))
	require.Error(t, h.Validate(wire, "ses_other"))
	id, cleanup, err := h.ImportReader(bytes.NewReader(wire), source, cwd)
	require.NoError(t, err)
	require.NotEqual(t, source, id)
	imported, err := os.ReadFile(filepath.Join(root, id+".json"))
	require.NoError(t, err)
	var data struct {
		Info     map[string]json.RawMessage
		Messages []struct {
			Info  map[string]json.RawMessage
			Parts []map[string]json.RawMessage
		}
	}
	require.NoError(t, json.Unmarshal(imported, &data))
	require.Equal(t, cwd, rawString(data.Info, "directory"))
	require.Nil(t, data.Info["permission"])
	require.Nil(t, data.Info["share"])
	require.Equal(t, rawString(data.Messages[0].Info, "id"), rawString(data.Messages[1].Info, "parentID"))
	require.NotEqual(t, "msg_user", rawString(data.Messages[0].Info, "id"))
	require.NotEqual(t, "prt_user", rawString(data.Messages[0].Parts[0], "id"))
	require.Equal(t, "keep /source and ses_source literally", rawString(data.Messages[0].Parts[0], "text"))
	// Native resume re-exports the reminted session without losing either turn.
	reader, err := h.Open(id, cwd)
	require.NoError(t, err)
	resume, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Contains(t, string(resume), "remote result")
	cleanup()
	_, err = os.Stat(filepath.Join(root, id+".json"))
	require.True(t, os.IsNotExist(err))
	require.FileExists(t, filepath.Join(root, source+".json"))
}
