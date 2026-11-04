package conv

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func withStandaloneRelaunchHooks(t *testing.T, prepare func(string) ([]string, []string, error), settle func(string, bool) []string) {
	t.Helper()
	oldPrepare, oldSettle := StandaloneRelaunchPrepare, StandaloneRelaunchSettle
	StandaloneRelaunchPrepare, StandaloneRelaunchSettle = prepare, settle
	t.Cleanup(func() { StandaloneRelaunchPrepare, StandaloneRelaunchSettle = oldPrepare, oldSettle })
}

const standaloneConv = "33333333-3333-3333-3333-333333333333"

// With agentd down, a human's resume still launches, under the stored policy,
// and says so in one line with what to do.
func TestStandaloneRelaunch_DaemonDownLaunchesUnderTheStoredPolicy(t *testing.T) {
	setupHarnessTestHome(t)
	_, _, err := db.EnsureAgentForConv(standaloneConv, "test")
	require.NoError(t, err)
	settled := false
	withStandaloneRelaunchHooks(t,
		func(string) ([]string, []string, error) {
			return nil, nil, &StandaloneRelaunchUnavailable{Warning: AgentdNotRunning}
		},
		func(string, bool) []string { settled = true; return nil })

	var out bytes.Buffer
	r, err := prepareStandaloneRelaunch(standaloneConv, &out)
	require.NoError(t, err, "a dead daemon never blocks the resume")
	assert.Contains(t, out.String(), "Start agentd")
	assert.Contains(t, out.String(), "resume again")
	r.settle(true)
	assert.False(t, settled, "nothing was prepared, so nothing is settled")
}

// A daemon refusal stops the resume, as an ordinary resume would stop; a
// prepared one is settled with the launch's outcome.
func TestStandaloneRelaunch_RefusalStopsAndPreparedIsSettled(t *testing.T) {
	setupHarnessTestHome(t)
	_, _, err := db.EnsureAgentForConv(standaloneConv, "test")
	require.NoError(t, err)

	withStandaloneRelaunchHooks(t,
		func(string) ([]string, []string, error) { return nil, nil, errors.New("sandbox profile gone") }, nil)
	_, err = prepareStandaloneRelaunch(standaloneConv, nil)
	require.Error(t, err)

	var got []bool
	withStandaloneRelaunchHooks(t,
		func(string) ([]string, []string, error) {
			return []string{"sandbox profile group-v2: now applied"}, []string{"heads up"}, nil
		},
		func(_ string, launched bool) []string { got = append(got, launched); return nil })
	var out bytes.Buffer
	r, err := prepareStandaloneRelaunch(standaloneConv, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "sandbox profile group-v2: now applied\n")
	assert.Contains(t, out.String(), "Warning: heads up")
	r.settle(false)
	r.settle(false)
	assert.Equal(t, []bool{false}, got, "settled once, with the outcome")
}

// A plain conversation has no agent sandbox, so agentd is not asked.
func TestStandaloneRelaunch_APlainConversationIsNotPrepared(t *testing.T) {
	setupHarnessTestHome(t)
	withStandaloneRelaunchHooks(t,
		func(string) ([]string, []string, error) { t.Fatal("asked agentd for a plain conversation"); return nil, nil, nil }, nil)
	var out bytes.Buffer
	_, err := prepareStandaloneRelaunch("44444444-4444-4444-4444-444444444444", &out)
	require.NoError(t, err)
	assert.Empty(t, out.String())
}
