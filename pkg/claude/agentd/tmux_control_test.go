package agentd

import (
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestTmuxControlModeChoice(t *testing.T) {
	off := false
	cfgOff := &config.Config{Tmux: &config.TmuxConfig{ControlMode: &off}}

	t.Setenv(clcommon.ControlModeEnvVar, "")
	if on, _ := tmuxControlModeChoice(nil); !on {
		t.Fatal("default must be on")
	}
	if on, _ := tmuxControlModeChoice(cfgOff); on {
		t.Fatal("config false must turn it off")
	}
	t.Setenv(clcommon.ControlModeEnvVar, "1")
	if on, _ := tmuxControlModeChoice(cfgOff); !on {
		t.Fatal("the env var wins over the config")
	}
	t.Setenv(clcommon.ControlModeEnvVar, "0")
	if on, _ := tmuxControlModeChoice(nil); on {
		t.Fatal("env 0 must turn it off")
	}
}

// A flow test's injected tmux fake must survive daemon startup untouched.
func TestStartTmuxControlModeKeepsInjectedFake(t *testing.T) {
	t.Setenv(clcommon.ControlModeEnvVar, "")
	prev := clcommon.Default
	fake := fakeTmuxForControlTest{}
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = prev })
	stop := startTmuxControlMode(nil)
	defer stop()
	if _, ok := clcommon.Default.(fakeTmuxForControlTest); !ok {
		t.Fatalf("Default replaced: %T", clcommon.Default)
	}
}

type fakeTmuxForControlTest struct{ clcommon.LiveTmux }
