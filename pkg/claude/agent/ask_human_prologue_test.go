package agent

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The commands that take --ask-human share one prologue: a malformed value
// is refused before the daemon is asked anything, and a set one says the
// command will wait for the human before it makes the request.
var askHumanSample = []struct {
	name string
	run  func(ask string, stdout, stderr io.Writer) int
}{
	{"retire", func(ask string, o, e io.Writer) int {
		return runRetire(&retireParams{Selector: "worker", AskHuman: ask}, o, e)
	}},
	{"stop", func(ask string, o, e io.Writer) int {
		return runStop(&stopParams{Selector: "worker", AskHuman: ask}, o, e)
	}},
	{"groups stop", func(ask string, o, e io.Writer) int {
		return runGroupsStop(&groupsStopParams{Name: "crew", AskHuman: ask}, o, e)
	}},
	{"groups rebrief", func(ask string, o, e io.Writer) int {
		return runGroupsRebrief(&groupsRebriefParams{Name: "crew", AskHuman: ask}, o, e)
	}},
	{"groups archive", func(ask string, o, e io.Writer) int { return runGroupsArchiveOrUnarchive("crew", "archive", ask, o, e) }},
	{"groups grant-owner", func(ask string, o, e io.Writer) int {
		return runGroupsGrantOwner(&groupsGrantOwnerParams{Group: "crew", Conv: "worker", AskHuman: ask}, o, e)
	}},
	{"task set", func(ask string, o, e io.Writer) int {
		return taskWrite("", ask, map[string]any{"url": "https://example.com/1"}, o, e)
	}},
	{"tags", func(ask string, o, e io.Writer) int { return tagsWrite("", ask, []string{"x"}, o, e) }},
	{"present-pr", func(ask string, o, e io.Writer) int {
		return runPresentPR(&presentPRParams{URL: "https://github.com/o/r/pull/1", AskHuman: ask}, o, e)
	}},
}

// askHumanStub records the first daemon request: its ask-human wait and
// what the command had printed by then. The request fails, so the command
// stops there.
func askHumanStub(t *testing.T) (calls *int, opts *DaemonOpts, printedBefore *string, stdout *bytes.Buffer) {
	t.Helper()
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	stdout = &bytes.Buffer{}
	calls, opts, printedBefore = new(int), &DaemonOpts{}, new(string)
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(_, _ string, _, _ any, o DaemonOpts) error {
		if *calls == 0 {
			*opts, *printedBefore = o, stdout.String()
		}
		*calls++
		return errors.New("stub: stop here")
	}
	return calls, opts, printedBefore, stdout
}

func TestAskHumanPrologue_RefusesAMalformedValueBeforeTheDaemon(t *testing.T) {
	for _, c := range askHumanSample {
		t.Run(c.name, func(t *testing.T) {
			calls, _, _, stdout := askHumanStub(t)
			var stderr bytes.Buffer
			rc := c.run("soon", stdout, &stderr)
			assert.Equal(t, rcInvalidArg, rc)
			assert.Equal(t, "Error: invalid --ask-human value \"soon\" (use seconds like 30 or duration like 30s)\n", stderr.String())
			assert.Empty(t, stdout.String())
			assert.Zero(t, *calls, "the daemon is not asked")
		})
	}
}

func TestAskHumanPrologue_SaysItWillWaitBeforeTheRequest(t *testing.T) {
	for _, c := range askHumanSample {
		t.Run(c.name, func(t *testing.T) {
			calls, opts, printedBefore, stdout := askHumanStub(t)
			var stderr bytes.Buffer
			c.run("5", stdout, &stderr)
			assert.NotZero(t, *calls)
			assert.Equal(t, 5*time.Second, opts.AskHuman)
			assert.Equal(t, "Waiting up to 5s for human approval...\n", *printedBefore)
		})
	}
}

func TestAskHumanPrologue_SaysNothingWithoutTheFlag(t *testing.T) {
	for _, c := range askHumanSample {
		t.Run(c.name, func(t *testing.T) {
			calls, opts, printedBefore, stdout := askHumanStub(t)
			var stderr bytes.Buffer
			c.run("", stdout, &stderr)
			assert.NotZero(t, *calls, "the command reached the daemon")
			assert.Zero(t, opts.AskHuman)
			assert.False(t, strings.Contains(*printedBefore, "Waiting"), *printedBefore)
		})
	}
}
