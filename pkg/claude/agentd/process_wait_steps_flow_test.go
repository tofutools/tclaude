package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: the editors validate a draft with a wait step (the full editor's
// Wait / timer). Wait steps are stored but cannot run yet, so validation names
// each one as a problem: the editors' problems line says the process can't
// start a run before a Start is refused. A process without one is quiet.
func TestProcessValidate_SaysAWaitStepCantRunYet(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	const withWait = `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: with-wait
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it", anyone: true }
    next: waiter
  waiter:
    type: wait
    wait: { duration: 1h }
    next: done
  done: { type: end, result: success }
`
	problems := func(source string) map[string]string {
		t.Helper()
		rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/validate", map[string]any{"source": source})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Diagnostics []struct{ Scope, TargetID, Code, Severity, Message string } `json:"diagnostics"`
		}
		testharness.DecodeJSON(t, rec, &body)
		out := map[string]string{}
		for _, d := range body.Diagnostics {
			if d.Code == "wait_not_runnable" {
				assert.Equal(t, "error", d.Severity)
				assert.Equal(t, "node", d.Scope)
				out[d.TargetID] = d.Message
			}
		}
		return out
	}
	got := problems(withWait)
	require.Contains(t, got, "waiter")
	assert.Contains(t, got["waiter"], "can't start a run")
	without := strings.Replace(strings.Replace(withWait, "next: waiter", "next: done", 1),
		"  waiter:\n    type: wait\n    wait: { duration: 1h }\n    next: done\n", "", 1)
	assert.Empty(t, problems(without))
	// A wait step just placed, with no duration yet, is named too, alongside its other problems.
	assert.Contains(t, problems(strings.Replace(withWait, "    wait: { duration: 1h }\n", "", 1)), "waiter")
}
