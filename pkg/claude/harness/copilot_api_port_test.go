package harness

import (
	"strings"
	"testing"
)

// The embedded server is rendered only when a port was allocated, and its
// absence must leave the argv byte-identical to a send-keys launch. That is the
// whole safety property of the opt-in: an agent nobody moved onto the API drive
// runs exactly the command it ran before this existed.
func TestCopilotBuildCommandEmbeddedServer(t *testing.T) {
	s := copilotSpawner{}
	const uuid = "11111111-2222-3333-4444-555555555555"
	cp := func(rest string) string { return copilotEnvScrub + rest }

	tests := []struct {
		name string
		spec SpawnSpec
		want string
	}{
		{
			name: "no port renders no embedded server at all",
			spec: SpawnSpec{SessionID: uuid},
			want: cp("copilot --session-id " + uuid),
		},
		{
			name: "a port renders TUI+server mode pinned to loopback",
			spec: SpawnSpec{SessionID: uuid, CopilotAPIPort: 4599},
			want: cp("copilot --session-id " + uuid +
				" --ui-server --host 127.0.0.1 --port 4599"),
		},
		{
			name: "a resumed launch keeps the drive it was spawned on",
			spec: SpawnSpec{ResumeID: uuid, CopilotAPIPort: 4599},
			want: cp("copilot --resume=" + uuid +
				" --ui-server --host 127.0.0.1 --port 4599"),
		},
		{
			// The server flags are tclaude's, so they sit with the other
			// tclaude-owned options: after model/effort, before the permission
			// flags and any pass-through args, with -i last.
			name: "the embedded server sits before permission and pass-through args",
			spec: SpawnSpec{
				SessionID:      uuid,
				Model:          "claude-sonnet-4.6",
				CopilotAPIPort: 4599,
				ApprovalPolicy: CopilotApprovalAllowTools,
				ExtraArgs:      []string{"--no-color"},
				InitialPrompt:  "go",
			},
			want: cp("copilot --session-id " + uuid + " --model=claude-sonnet-4.6" +
				" --ui-server --host 127.0.0.1 --port 4599" +
				" --allow-all-tools --no-ask-user --no-color -i go"),
		},
		{
			// A negative or zero value is "no port", never a flag with a
			// nonsense operand: `--port -1` would be parsed as a missing value
			// followed by a stray option.
			name: "a non-positive port renders nothing",
			spec: SpawnSpec{SessionID: uuid, CopilotAPIPort: -1},
			want: cp("copilot --session-id " + uuid),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.BuildCommand(tc.spec); got != tc.want {
				t.Fatalf("BuildCommand()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// --acp and --ui-server are NOT mutually exclusive in Copilot CLI, and that is
// worse than if they were: the combination is accepted, ACP silently wins, no
// TUI ever mounts, and the port still listens — so a pane would look launched,
// answer on its port, and show nothing. Nothing tclaude renders may reach that
// state, and the pass-through gate is what keeps --acp out of the argv.
func TestCopilotEmbeddedServerCannotBeCombinedWithACP(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	if err := ValidateLaunchExtraArgs(h, []string{"--acp"}); err == nil {
		t.Fatal("--acp must be refused as a pass-through arg: combined with " +
			"--ui-server it silently wins and no TUI mounts")
	}
	// The operator cannot hand-roll the server flags either — they are tclaude's
	// to render, from a port tclaude allocated and is watching.
	for _, arg := range []string{"--ui-server", "--port", "--host"} {
		if err := ValidateLaunchExtraArgs(h, []string{arg}); err == nil {
			t.Fatalf("%s must be refused as a pass-through arg", arg)
		}
	}
}

// The rendered command is handed to `sh -c`, so the port must land as one
// argument. It is an int rather than free text, which makes injection
// impossible by construction — this pins that it stays that way.
func TestCopilotEmbeddedServerPortIsASingleArgument(t *testing.T) {
	got := copilotSpawner{}.BuildCommand(SpawnSpec{CopilotAPIPort: 65535})
	if !strings.HasSuffix(got, " --ui-server --host 127.0.0.1 --port 65535") {
		t.Fatalf("unexpected embedded-server rendering: %s", got)
	}
}
