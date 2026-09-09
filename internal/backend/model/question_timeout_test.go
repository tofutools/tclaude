package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQuestionTimeoutExplicitNativeInheritOverridesProfile(t *testing.T) {
	var options ConfigurationOptions
	require.NoError(t, json.Unmarshal([]byte(`{"AskUserQuestionTimeout":" inherit "}`), &options))
	base := DesiredConfiguration{Harness: "claude", AskUserQuestionTimeout: "5m"}
	require.Equal(t, AskUserQuestionTimeout("inherit"), options.Apply(base).AskUserQuestionTimeout)
	require.Equal(t, AskUserQuestionTimeout("5m"), (ConfigurationOptions{}).Apply(base).AskUserQuestionTimeout)
	harness := "codex"
	require.Empty(t, (&NativeConfigurationOptions{Harness: &harness}).Apply(base).AskUserQuestionTimeout)
	for _, value := range []AskUserQuestionTimeout{"", "inherit", "never", "60s", "5m", "10m"} {
		require.NoError(t, value.Validate("claude"))
		if value != "" {
			require.Error(t, value.Validate("codex"))
		}
	}
	for _, raw := range []string{`60`, `true`, `"1m"`, `"60S"`, `"2m"`} {
		var value AskUserQuestionTimeout
		require.Error(t, json.Unmarshal([]byte(raw), &value))
	}
}
