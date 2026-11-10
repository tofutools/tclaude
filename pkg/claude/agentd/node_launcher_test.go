package agentd

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTclaudeExecutable(t *testing.T) {
	look := func(paths map[string]string) func(string) (string, error) {
		return func(name string) (string, error) {
			if p, ok := paths[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		}
	}
	exists := func(set ...string) func(string) bool {
		return func(p string) bool {
			for _, s := range set {
				if s == p {
					return true
				}
			}
			return false
		}
	}
	// An absolute binary is kept as given (a symlinked bin path stays).
	got, err := tclaudeExecutable("/opt/homebrew/bin/tclaude", look(nil), exists())
	require.NoError(t, err)
	require.Equal(t, "/opt/homebrew/bin/tclaude", got)
	// A bare name resolves through PATH.
	got, err = tclaudeExecutable("tclaude", look(map[string]string{"tclaude": "/home/u/.local/bin/tclaude"}), exists())
	require.NoError(t, err)
	require.Equal(t, "/home/u/.local/bin/tclaude", got)
	// The standalone daemon uses the tclaude next to it, else on PATH.
	got, err = tclaudeExecutable("/usr/local/bin/tclaude-agentd", look(nil), exists("/usr/local/bin/tclaude"))
	require.NoError(t, err)
	require.Equal(t, "/usr/local/bin/tclaude", got)
	got, err = tclaudeExecutable("/srv/tclaude-agentd", look(map[string]string{"tclaude": "/usr/bin/tclaude"}), exists())
	require.NoError(t, err)
	require.Equal(t, "/usr/bin/tclaude", got)
	_, err = tclaudeExecutable("/srv/tclaude-agentd", look(nil), exists())
	require.Error(t, err)
}

func TestThrowawayBinaryIsNotBakedIn(t *testing.T) {
	require.True(t, throwawayBinary("/home/u/.cache/go-build/ab/exe/tclaude"))
	require.False(t, throwawayBinary("/home/u/.local/bin/tclaude"))
}
