package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResourcesLockSendsTheToggleAndRefusesAnUnknownKind(t *testing.T) {
	var calls []string
	var bodies []map[string]any
	prev := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = prev })
	DaemonRequestImpl = func(method, path string, in, _ any, opts DaemonOpts) error {
		calls = append(calls, method+" "+path)
		raw, err := json.Marshal(in)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		bodies = append(bodies, body)
		assert.Equal(t, "30s", opts.AskHuman.String())
		return nil
	}
	var out bytes.Buffer
	rc := runResourcesLock(&resourcesLockParams{Kind: "board", ID: "plan", AskHuman: "30s"}, false, &out, &bytes.Buffer{})
	require.Equal(t, rcOK, rc)
	assert.Equal(t, []string{http.MethodPost + " /v1/resource-locks"}, calls)
	assert.Equal(t, map[string]any{"kind": "board", "id": "plan", "locked": false}, bodies[0])
	assert.Contains(t, out.String(), "Unlocked board plan")

	rc = runResourcesLock(&resourcesLockParams{Kind: "group", ID: "x"}, true, &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, rcInvalidArg, rc)
	assert.Len(t, calls, 1, "an unknown kind sends nothing")
}
