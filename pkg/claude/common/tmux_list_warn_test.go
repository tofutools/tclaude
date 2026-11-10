package common

import "testing"

func TestTmuxNoServer(t *testing.T) {
	for msg, want := range map[string]bool{
		"no server running on /private/tmp/tmux-501/tclaude":                            true,
		"error connecting to /private/tmp/tmux-501/tclaude (No such file or directory)": true,
		"error connecting to /private/tmp/tmux-501/tclaude (Permission denied)":         false,
		"directory /private/tmp/tmux-501 has unsafe permissions":                        false,
		"protocol version mismatch (client 8, server 7)":                                false,
		`exec: "tmux": executable file not found in $PATH`:                              false,
	} {
		if got := tmuxNoServer(msg); got != want {
			t.Errorf("%q: %v, want %v", msg, got, want)
		}
	}
}
