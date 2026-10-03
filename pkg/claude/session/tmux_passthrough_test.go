package session

import (
	"reflect"
	"testing"
)

func TestConfigureTmuxPassthrough(t *testing.T) {
	rec := withRecordingTmux(t)
	ConfigureTmuxPassthrough("sess-harness")
	// The default must target exactly the managed window, never the server
	// or global window defaults (which may affect unrelated operator sessions).
	want := [][]string{{"set-option", "-t", "=sess-harness:", "allow-passthrough", "on"}}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Fatalf("tmux passthrough config = %v, want %v", rec.calls, want)
	}
}
