package transport

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestParameterDefaultProjectionPreservesAbsenceAndExactJSON(t *testing.T) {
	for _, raw := range []string{"", "null", " \nnull\t", "9007199254740993", "0.1234567890123456789"} {
		p := model.ParameterDeclaration{Name: "value", Type: model.ParameterNumber, Default: json.RawMessage(raw)}
		projected := projectParameters([]model.ParameterDeclaration{p})[0]
		if raw == "" || raw == "null" || raw == " \nnull\t" {
			require.Empty(t, projected.DefaultJSON)
		} else {
			require.Equal(t, raw, projected.DefaultJSON)
		}
		require.Equal(t, p.Default, projected.Default, "read projection cannot alter stored defaults or hashes")
	}
}
