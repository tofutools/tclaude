package agent

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The library edit commands' --file and result messages, byte for byte
// (awb tcl-mlvszj).

type libraryEditCmd struct {
	noun string
	run  func(name, file string, stdin io.Reader, stdout, stderr io.Writer) int
	body string
}

func libraryEditCmds() []libraryEditCmd {
	return []libraryEditCmd{
		{"role", func(name, file string, stdin io.Reader, stdout, stderr io.Writer) int {
			return runRolesEdit(&rolesEditParams{Name: name, File: file}, stdin, stdout, stderr)
		}, `{"name":"%s"}`},
		{"template", func(name, file string, stdin io.Reader, stdout, stderr io.Writer) int {
			return runTemplatesEdit(&templatesEditParams{Name: name, File: file}, stdin, stdout, stderr)
		}, `{"name":"%s"}`},
		{"profile", func(name, file string, stdin io.Reader, stdout, stderr io.Writer) int {
			return runProfilesEdit(&profilesEditParams{Name: name, File: file}, stdin, stdout, stderr)
		}, `{"name":"%s"}`},
	}
}

func TestLibraryEditFileMessages(t *testing.T) {
	for _, c := range libraryEditCmds() {
		t.Run(c.noun, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			assert.Equal(t, rcInvalidArg, c.run("x", "", nil, &stdout, &stderr))
			assert.Equal(t, "Error: --file is required (path to a "+c.noun+" JSON file, or - to read stdin)\n", stderr.String())

			stderr.Reset()
			assert.Equal(t, rcInvalidArg, c.run("x", "-", strings.NewReader("{not json"), &stdout, &stderr))
			assert.Equal(t, "Error: --file is not valid "+c.noun+" JSON: invalid character 'n' looking for beginning of object key string\n", stderr.String())
			assert.Empty(t, stdout.String())
		})
	}
}

func TestLibraryEditResultMessages(t *testing.T) {
	cmds := libraryEditCmds()
	cmds = append(cmds, libraryEditCmd{"sandbox profile", func(name, file string, stdin io.Reader, stdout, stderr io.Writer) int {
		return runSandboxProfilesEdit(&sandboxProfilesEditParams{Name: name, File: file}, stdin, stdout, stderr)
	}, `{"name":"%s","network_access":"none"}`})
	for _, c := range cmds {
		for _, to := range []string{"orig", "renamed"} {
			t.Run(c.noun+" to "+to, func(t *testing.T) {
				var calls []capturedReq
				stubDaemon(t, &calls, ok(`{"id":1,"name":"`+to+`"}`))
				var stdout, stderr bytes.Buffer
				// The submitted name differs from what the daemon stored, so the
				// output is pinned to the daemon's answer.
				rc := c.run("orig", "-", strings.NewReader(strings.ReplaceAll(c.body, "%s", "submitted")), &stdout, &stderr)
				require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
				want := "Updated " + c.noun + ` "orig"` + "\n"
				if to != "orig" {
					want = "Updated " + c.noun + ` "orig" → renamed to "renamed"` + "\n"
				}
				assert.Equal(t, want, stdout.String())
			})
		}
	}
}
