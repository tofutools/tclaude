package agent

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/conv"
)

// Only agentd refusing the relaunch stops a human's resume; agentd being
// unable to prepare it (down, not confirming the operator, too old for the
// route) lets it go ahead under the stored policy, with its own warning.
func TestStandaloneRelaunchPrepare_ClassifiesTheDaemonsAnswer(t *testing.T) {
	old := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = old })
	for _, tc := range []struct {
		name    string
		err     error
		warning string
	}{
		{"unreachable", errors.New("dial unix: connection refused"), conv.AgentdNotRunning},
		{"unconfirmed", &DaemonError{Status: http.StatusForbidden, Code: "unconfirmed"}, conv.AgentdNotOperator},
		{"unidentified", &DaemonError{Status: http.StatusUnauthorized, Code: "auth"}, conv.AgentdNotOperator},
		{"older daemon", &DaemonError{Status: http.StatusNotFound}, conv.AgentdTooOld},
		{"refused", &DaemonError{Status: http.StatusConflict, Code: "sandbox_profile_changed", Msg: "gone"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			DaemonRequestImpl = func(string, string, any, any, DaemonOpts) error { return tc.err }
			_, err := conv.StandaloneRelaunchPrepare("c")
			require.Error(t, err)
			var unavailable *conv.StandaloneRelaunchUnavailable
			if tc.warning == "" {
				assert.False(t, errors.As(err, &unavailable), "a refusal stops the resume")
				return
			}
			require.True(t, errors.As(err, &unavailable))
			assert.Equal(t, tc.warning, unavailable.Warning)
		})
	}
}
