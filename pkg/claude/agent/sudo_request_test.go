package agent

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The client must outlast the daemon's approval wait: the CLI declares its
// wait to the daemon (which never lets the popup, "+extend" included, run
// past it) and keeps a margin on top. The old fixed 10s client timeout
// hung up while the popup was still open.
func TestSudoRequest_ClientBudgetFollowsDeclaredWait(t *testing.T) {
	for _, c := range []struct {
		wait     string
		wantWait time.Duration
	}{
		{"", 10 * time.Minute},
		{"2m", 2 * time.Minute},
		{"2h", sudoMaxWait},
	} {
		t.Run(c.wait, func(t *testing.T) {
			_, opts, _, stdout := askHumanStub(t)
			var body map[string]any
			prev := DaemonRequestImpl
			DaemonRequestImpl = func(m, path string, in, out any, o DaemonOpts) error {
				body = in.(map[string]any)
				return prev(m, path, in, out, o)
			}
			var stderr bytes.Buffer
			runSudoRequest(&sudoRequestParams{Slugs: []string{"groups.create"}, Wait: c.wait}, stdout, &stderr)
			assert.Equal(t, c.wantWait.String(), body["wait"], "the daemon is told the wait")
			assert.Equal(t, c.wantWait+approvalClientMargin, opts.Timeout, "client budget = wait + margin")
			assert.Contains(t, stderr.String(), "waiting up to "+c.wantWait.String())
		})
	}
}

func TestSudoRequest_MalformedWaitRefusedBeforeTheDaemon(t *testing.T) {
	calls, _, _, stdout := askHumanStub(t)
	var stderr bytes.Buffer
	rc := runSudoRequest(&sudoRequestParams{Slugs: []string{"groups.create"}, Wait: "soon"}, stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Zero(t, *calls)
}

// A proactive --target grant has no popup, so it sends no wait.
func TestSudoRequest_TargetSendsNoWait(t *testing.T) {
	_, opts, _, stdout := askHumanStub(t)
	var body map[string]any
	prev := DaemonRequestImpl
	DaemonRequestImpl = func(m, path string, in, out any, o DaemonOpts) error {
		body = in.(map[string]any)
		return prev(m, path, in, out, o)
	}
	runSudoRequest(&sudoRequestParams{Slugs: []string{"groups.create"}, Target: "worker"}, stdout, &bytes.Buffer{})
	assert.NotContains(t, body, "wait")
	assert.Zero(t, opts.Timeout)
}
