package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: a fix that is to be the same agent as Implement, whose new agent
// is retired once Implement is done, is warned about; kept for later steps
// (the default), it is not.
func TestProcessValidate_WarnsWhenSameAsNamesARetiredAgent(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	const source = `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: same-as-retired
start: implement
nodes:
  implement:
    type: task
    name: Implement
    performer: { kind: agent, prompt: "Do it", fallback: { action: spawn, lifetime: LIFETIME } }
    next: fix
  fix:
    type: task
    performer: { kind: agent, prompt: "Fix it", anyone: true, sameAs: implement }
    next: done
  done: { type: end, result: success }
`
	warnings := func(lifetime string) []string {
		t.Helper()
		rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/validate",
			map[string]any{"source": strings.Replace(source, "LIFETIME", lifetime, 1)})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Diagnostics []struct{ TargetID, Code, Severity, Message string } `json:"diagnostics"`
		}
		testharness.DecodeJSON(t, rec, &body)
		var out []string
		for _, d := range body.Diagnostics {
			if d.Code == "same_as_retired" {
				assert.Equal(t, "warning", d.Severity)
				assert.Equal(t, "fix", d.TargetID)
				out = append(out, d.Message)
			}
		}
		return out
	}
	got := warnings("step")
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "Implement's new agent is retired once Implement is done")
	assert.Empty(t, warnings("run"))
	assert.Empty(t, warnings("keep"))
}
