package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// show and add print the task's dashboard link when the daemon sends one,
// and nothing in its place when it does not (no listener, a linked node).
func TestRunTaskboardPrintsTaskLink(t *testing.T) {
	const link = "http://127.0.0.1:7777/groups?task=tk_1%3Acrew"
	task := `{"id":"tk_1","group":"crew","title":"ship it","stage":"next","status":"open","depends_on":[],"created_at":"now","url":"` + link + `"}`

	var calls []capturedReq
	stubDaemon(t, &calls, ok(task))
	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, runTaskboardShow(&taskboardShowParams{Task: "tk_1"}, &stdout, &stderr), stderr.String())
	assert.Contains(t, stdout.String(), "  link:    "+link+"\n")

	stdout.Reset()
	require.Equal(t, rcOK, runTaskboardAdd(&taskboardAddParams{Group: "crew", Title: "ship it"}, strings.NewReader(""), &stdout, &stderr), stderr.String())
	assert.Equal(t, "added tk_1: ship it  [next, open]\n  link: "+link+"\n", stdout.String())

	stubDaemon(t, &calls, ok(strings.Replace(task, `,"url":"`+link+`"`, "", 1)))
	stdout.Reset()
	require.Equal(t, rcOK, runTaskboardShow(&taskboardShowParams{Task: "tk_1"}, &stdout, &stderr), stderr.String())
	assert.NotContains(t, stdout.String(), "link:")
}
