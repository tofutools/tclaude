package hub

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { RunExecGuardian(); os.Exit(m.Run()) }
