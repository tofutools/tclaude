package agentd

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestHumanTokenOutputPolicy(t *testing.T) {
	old := tokenOutputInteractive
	t.Cleanup(func() { tokenOutputInteractive = old })
	for _, terminal := range []bool{false, true} {
		tokenOutputInteractive = func() bool { return terminal }
		for _, mode := range []string{"", "auto", "always", "never"} {
			p := &serveParams{TUI: true, PrintHumanToken: mode}
			want := mode == "always" || (mode == "" || mode == "auto") && terminal
			require.Equal(t, want, humanTokenSecretsAllowed(p), "mode=%q terminal=%v", mode, terminal)
			require.Equal(t, want, tokenBannerInTUI(p))
			for _, src := range []tokenSource{{kind: tokenSourceEphemeral}, {kind: tokenSourceFile, path: "/private/operator_token"}, {kind: tokenSourceKeychain}} {
				var out bytes.Buffer
				resolved, err := humanTokenOutputMode(p)
				require.NoError(t, err)
				if resolved != "never" {
					writeOperatorTokenBanner(&out, "tclo_fixture_secret", src, humanTokenSecretsAllowed(p))
				}
				require.Equal(t, want, bytes.Contains(out.Bytes(), []byte("tclo_fixture_secret")))
			}
		}
	}
	for _, mode := range []string{"", "auto", "never"} {
		p := &serveParams{TUI: true, PrintHumanToken: mode, NoPrintHumanToken: true}
		got, err := humanTokenOutputMode(p)
		require.NoError(t, err)
		require.Equal(t, "never", got)
		require.False(t, tokenBannerInTUI(p))
	}
	for _, p := range []*serveParams{{PrintHumanToken: "invalid"}, {PrintHumanToken: "always", NoPrintHumanToken: true}} {
		_, err := humanTokenOutputMode(p)
		require.Error(t, err)
		require.False(t, humanTokenSecretsAllowed(p))
		// Validation precedes sockets, locks, migrations and token generation.
		require.Error(t, runServe(p))
	}
}
