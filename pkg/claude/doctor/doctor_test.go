package doctor

import (
	"errors"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func fakeProbes(onPath map[string]bool, sandboxDefault string, decided bool) Probes {
	return Probes{
		LookPath: func(name string) (string, error) {
			if onPath[name] {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		SandboxDefault:           func() (string, bool, error) { return sandboxDefault, decided, nil },
		TclaudeLayerPresence:     func() error { return nil },
		TclaudeLayerAvailability: func() error { return nil },
		CopilotPosture:           func(*harness.Harness) error { return nil },
		StatusBarInstalled:       func() bool { return true },
		OSRelease:                func() map[string]string { return map[string]string{"ID": "ubuntu"} },
	}
}

func byID(checks []Check) map[string]Check {
	out := map[string]Check{}
	for _, c := range checks {
		out[c.ID] = c
	}
	return out
}

func TestUndecidedSandboxDefaultIsADecision(t *testing.T) {
	got := byID(Run(fakeProbes(map[string]bool{"tmux": true, "git": true, "bwrap": true}, "", false)))
	check := got["sandbox-default"]
	assert.Equal(t, SeverityDecision, check.Severity)
	assert.Equal(t, CategoryDecisions, check.Category)
	assert.Equal(t, ActionSandboxDefault, check.Action)
}

func TestDecidedSandboxDefaultIsOKAndCarriesTheValue(t *testing.T) {
	got := byID(Run(fakeProbes(map[string]bool{"tmux": true, "bwrap": true}, "harness", true)))
	check := got["sandbox-default"]
	assert.Equal(t, SeverityOK, check.Severity)
	assert.Equal(t, "harness", check.Value)
}

func TestMissingBwrapBlocksOnlyWhenTheLayerIsTheDefault(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is the Linux backend")
	}
	blocking := byID(Run(fakeProbes(map[string]bool{"tmux": true}, "tclaude-layer", true)))["bwrap"]
	assert.Equal(t, SeverityBlocking, blocking.Severity)
	assert.Equal(t, "sudo apt install bubblewrap", blocking.Fix)

	optional := byID(Run(fakeProbes(map[string]bool{"tmux": true}, "harness", true)))["bwrap"]
	assert.Equal(t, SeverityInfo, optional.Severity, "a backend nobody chose is not a fault")
}

func TestNoHarnessInstalledIsBlocking(t *testing.T) {
	got := byID(Run(fakeProbes(map[string]bool{"tmux": true}, "harness", true)))
	require.Contains(t, got, "harness-none")
	assert.Equal(t, SeverityBlocking, got["harness-none"].Severity)
	assert.Equal(t, SeverityBlocking, byID(Run(fakeProbes(nil, "harness", true)))["tmux"].Severity)
}

func TestPackageInstallCommandIsEmptyForAnUnknownDistro(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("distro detection is Linux-only")
	}
	assert.Equal(t, "", packageInstallCommand(map[string]string{"ID": "plan9"}, "tmux"))
	assert.Equal(t, "sudo dnf install tmux", packageInstallCommand(map[string]string{"ID": "rocky", "ID_LIKE": "rhel centos fedora"}, "tmux"))
}
