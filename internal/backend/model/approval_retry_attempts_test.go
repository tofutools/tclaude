package model_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestApprovalRetryAttemptsKeepExactLegacyRange(t *testing.T) {
	for _, input := range []string{`4294967296`, `"4294967296"`, `9007199254740993`, `"9007199254740993"`, `9223372036854775807`, `"9223372036854775807"`} {
		var count model.ApprovalRetryAttempts
		require.NoError(t, json.Unmarshal([]byte(input), &count))
		encoded, err := json.Marshal(count)
		require.NoError(t, err)
		var text string
		require.NoError(t, json.Unmarshal(encoded, &text))
		require.Equal(t, strings.Trim(input, "\""), text)
		var again model.ApprovalRetryAttempts
		require.NoError(t, json.Unmarshal(encoded, &again))
		require.Equal(t, count, again)
		require.Equal(t, '"', rune(encoded[0]), "output is exact text")
	}
	for _, input := range []string{`9223372036854775808`, `"9223372036854775808"`, `1.5`, `null`, `"no"`} {
		var count model.ApprovalRetryAttempts
		require.Error(t, json.Unmarshal([]byte(input), &count))
	}
}
