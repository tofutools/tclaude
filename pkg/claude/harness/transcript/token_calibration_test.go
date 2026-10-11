package transcript

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reference counts were measured locally using OpenAI tiktoken's cl100k_base
// and o200k_base encodings. No tokenizer dependency or model call runs in CI.
// https://github.com/openai/tiktoken
// This is a safety estimate, not a claim about every provider's tokenizer.
func TestTokenEstimateCalibration(t *testing.T) {
	raw, err := os.ReadFile("testdata/token_calibration.json")
	require.NoError(t, err)
	var samples []struct {
		Name string `json:"name"`
		Text string `json:"text"`
		CL   int64  `json:"cl100k_base"`
		O    int64  `json:"o200k_base"`
	}
	require.NoError(t, json.Unmarshal(raw, &samples))
	for _, sample := range samples {
		t.Run(sample.Name, func(t *testing.T) {
			n := estimateTokens(Entry{Blocks: []Block{{Kind: BlockText, Text: sample.Text}}})
			assert.GreaterOrEqual(t, n, max(sample.CL, sample.O), "the history safety budget covers the measured encodings")
		})
	}
}
