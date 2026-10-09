package harness

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const historySource = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
const historyTarget = "019fe740-43a4-7023-b8ae-1ee64459f2a2"

func TestHistoryTransferRemapsMetadataOnly(t *testing.T) {
	for _, name := range []string{DefaultName, CodexName} {
		t.Run(name, func(t *testing.T) {
			h, _ := Get(name)
			require.True(t, h.SupportsHistoryTransfer())
			native := h.History.(jsonlHistory)
			prose := historySource + " /original api_key=keepverbatim123456"
			var raw string
			if name == CodexName {
				raw = `{"type":"session_meta","payload":{"id":"` + historySource + `","cwd":"/original"}}` + "\n" + `{"type":"turn_context","payload":{"cwd":"/original"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","content":[{"text":"` + prose + `"}]}}` + "\n"
			} else {
				raw = `{"sessionId":"` + historySource + `","cwd":"/original","type":"user","message":{"content":"` + prose + `"}}` + "\n"
			}
			require.NoError(t, native.Validate([]byte(raw), historySource))
			got, err := native.rewriteHistory([]byte(raw), historySource, historyTarget, "/receiving")
			require.NoError(t, err)
			assert.Contains(t, string(got), prose)
			assert.Contains(t, string(got), historyTarget)
			assert.Contains(t, string(got), `"cwd":"/receiving"`)
			var obj map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(strings.Split(string(got), "\n")[0]), &obj))
			require.Error(t, native.Validate([]byte(strings.ReplaceAll(raw, historySource, historyTarget)), historySource))
			require.Error(t, native.Validate([]byte("not-json\n"), historySource))
		})
	}
	for _, name := range []string{OpenCodeName, CopilotName, GeminiName} {
		h, _ := Get(name)
		assert.False(t, h.SupportsHistoryTransfer())
	}
}
