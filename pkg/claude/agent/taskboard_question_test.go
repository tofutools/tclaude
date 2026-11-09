package agent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A local image path in a question's body is useless to the operator, who
// reads it in the dashboard, maybe on another device (tk_72dff9f8).
func TestLocalImagePath(t *testing.T) {
	cases := map[string]string{
		"See /tmp/x/scratchpad/mock/epics-ab.png for the board.": "/tmp/x/scratchpad/mock/epics-ab.png",
		"PNGs:\n- ~/shots/a.JPG — the strip":                    "~/shots/a.JPG",
		"(/home/me/out.webp)":                                    "/home/me/out.webp",
		"path=/var/tmp/s.svg":                                    "/var/tmp/s.svg",
		"Look at C:\\Users\\me\\shot.png please":                 "C:\\Users\\me\\shot.png",
		"Attached as #13 epics-ab.png":                           "",
		"See https://example.com/a/b.png":                        "",
		"The notes are in /tmp/notes.md":                         "",
		"":                                                       "",
	}
	for body, want := range cases {
		assert.Equal(t, want, localImagePath(body), body)
	}
}

// ask-operator warns about a local image path in the body unless a file
// goes with the question, and refuses --attach-existing on a group (a new
// task has no files), before it reaches a daemon.
func TestAskOperatorLocalPathWarningAndExisting(t *testing.T) {
	run := func(p taskboardAskOperatorParams) (int, string) {
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := runTaskboardAskOperator(&p, new(bytes.Buffer), stdout, stderr)
		return rc, stderr.String()
	}
	rc, errOut := run(taskboardAskOperatorParams{Target: "crew", Question: "Which?", Existing: []string{"#3"}})
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, errOut, "--attach-existing")

	// Invalid --wait stops it before any daemon; the warning comes first.
	_, errOut = run(taskboardAskOperatorParams{Target: "tk_00000001", Question: "Which?", Body: "Mock: /tmp/m/a.png", Recommend: "9"})
	assert.Contains(t, errOut, "Warning: the body names a local image (/tmp/m/a.png)")
	assert.Contains(t, errOut, "--attach /tmp/m/a.png")
	_, errOut = run(taskboardAskOperatorParams{Target: "tk_00000001", Question: "Which?", Body: "Mock: /tmp/m/a.png", Existing: []string{"a.png"}, Recommend: "9"})
	assert.NotContains(t, errOut, "Warning")
}
