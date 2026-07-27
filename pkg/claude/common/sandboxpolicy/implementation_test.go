package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeImplementation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		want    Implementation
		wantErr string
	}{
		{name: "empty defaults to legacy behavior", want: ImplementationHarnessBuiltin},
		{name: "harness builtin", value: " harness-builtin ", want: ImplementationHarnessBuiltin},
		{name: "tclaude layer", value: "tclaude-layer", want: ImplementationTclaudeLayer},
		{name: "stacked", value: "stacked", want: ImplementationStacked},
		{name: "invalid", value: "automatic", wantErr: "invalid sandbox implementation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeImplementation(tc.value)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
