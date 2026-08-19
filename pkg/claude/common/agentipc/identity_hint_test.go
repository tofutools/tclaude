package agentipc

import "testing"

func TestManagedAgentProcess(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
		hint bool
	}{
		{
			name: "plain operator shell",
			env:  map[string]string{},
			want: false,
		},
		{
			name: "sandboxed launch pinning the daemon socket",
			env:  map[string]string{SocketEnv: "/home/op/.tclaude/api/agentd.sock"},
			want: true,
		},
		{
			name: "managed Codex session",
			env:  map[string]string{CodexPermissionProfileEnv: ManagedCodexProfileName},
			want: true,
		},
		{
			// The case that matters for a Claude Code pane: no socket override,
			// no Codex profile, hint only.
			name: "hinted managed agent",
			env:  map[string]string{AgentHintEnvVar: "1"},
			want: true,
			hint: true,
		},
		{
			name: "unmanaged Codex session",
			env:  map[string]string{CodexPermissionProfileEnv: "workspace-write"},
			want: false,
		},
		{
			name: "hint set to anything but 1",
			env:  map[string]string{AgentHintEnvVar: "0"},
			want: false,
		},
		{
			name: "padded hint",
			env:  map[string]string{AgentHintEnvVar: " 1 "},
			want: true,
			hint: true,
		},
		{
			name: "whitespace-only signals are empty",
			env: map[string]string{
				SocketEnv:                 "   ",
				CodexPermissionProfileEnv: "  ",
				AgentHintEnvVar:           " ",
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{SocketEnv, CodexPermissionProfileEnv, AgentHintEnvVar} {
				t.Setenv(name, tc.env[name])
			}
			if got := ManagedAgentProcess(); got != tc.want {
				t.Fatalf("ManagedAgentProcess() = %v, want %v", got, tc.want)
			}
			if got := HasAgentHint(); got != tc.hint {
				t.Fatalf("HasAgentHint() = %v, want %v", got, tc.hint)
			}
		})
	}
}
