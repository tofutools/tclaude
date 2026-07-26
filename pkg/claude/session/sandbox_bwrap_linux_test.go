//go:build linux

package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestResolveTclaudeLayerRefusesMissingBwrapAndRecordsOffVerdict(t *testing.T) {
	oldLookPath := lookPathBwrap
	oldProbe := probeBwrap
	t.Cleanup(func() {
		lookPathBwrap = oldLookPath
		probeBwrap = oldProbe
	})
	lookPathBwrap = func(string) (string, error) {
		return "", errors.New("executable file not found")
	}

	_, verdict, err := ResolveTclaudeLayer(sandboxpolicy.NetworkHostOpen)
	require.ErrorContains(t, err, "requires bubblewrap (`bwrap`) on PATH")
	assert.Equal(t, "off", verdict.State)
	assert.Equal(t, "tclaude-layer unavailable", verdict.Source)

	row := toRow(&SessionState{
		ID:              "refused",
		OSSandboxState:  verdict.State,
		OSSandboxSource: verdict.Source,
	})
	assert.Equal(t, "off", row.OSSandboxState)
	assert.Equal(t, "tclaude-layer unavailable", row.OSSandboxSource)
}

func TestResolveTclaudeLayerRefusesUnavailableUserNamespace(t *testing.T) {
	oldLookPath := lookPathBwrap
	oldProbe := probeBwrap
	t.Cleanup(func() {
		lookPathBwrap = oldLookPath
		probeBwrap = oldProbe
	})
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(string) error { return errors.New("operation not permitted") }

	_, verdict, err := ResolveTclaudeLayer(sandboxpolicy.NetworkHostOpen)
	require.ErrorContains(t, err, "unprivileged user namespaces may be unavailable")
	assert.Equal(t, "off", verdict.State)
}
