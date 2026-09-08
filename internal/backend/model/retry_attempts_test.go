package model_test

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestRetryAttemptsPreserveBoundedJSONAndExactLargeValues(t *testing.T) {
	for _, test := range []struct{ input, output string }{
		{"0", "0"}, {"100", "100"}, {"4294967296", "4294967296"},
		{"9007199254740993", `"9007199254740993"`},
		{`"9223372036854775807"`, `"9223372036854775807"`},
	} {
		var count model.RetryAttempts
		require.NoError(t, json.Unmarshal([]byte(test.input), &count))
		out, err := json.Marshal(count)
		require.NoError(t, err)
		require.Equal(t, test.output, string(out))
	}
	for _, invalid := range []string{"9223372036854775808", "1.5", "null", `"1.2"`} {
		var count model.RetryAttempts
		require.Error(t, json.Unmarshal([]byte(invalid), &count))
	}
	// Existing bounded policy bytes (and consequently definition hashes) stay unchanged.
	out, err := json.Marshal(model.RetryPolicy{MaxAttempts: 2})
	require.NoError(t, err)
	require.JSONEq(t, `{"MaxAttempts":2,"Backoff":0,"AttemptBudget":0,"Retryable":null}`, string(out))
	require.Contains(t, string(out), `"MaxAttempts":2`)
}
