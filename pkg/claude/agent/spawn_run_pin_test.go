package agent

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// These pin RunSpawn before it is split into named steps: every refusal made
// before the daemon is asked is the first one the flags trip, with its exact
// message, and asks nothing; a spawn that fails after a worktree was made for
// it tears that worktree down, except on a 504, where the session may still be
// coming up in it; and a harness gate refuses before any request.

// spawnDaemon fakes the daemon for RunSpawn and records every request.
type spawnDaemon struct {
	calls []string
	// reply answers a request; nil answers the spawn with a plain success.
	reply func(path string, body, out any) error
}

func useSpawnDaemon(t *testing.T, d *spawnDaemon) {
	t.Helper()
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool {
		d.calls = append(d.calls, "available")
		return true
	}
	DaemonRequestImpl = func(_, path string, body, out any, _ DaemonOpts) error {
		d.calls = append(d.calls, path)
		if d.reply != nil {
			return d.reply(path, body, out)
		}
		if resp, ok := out.(*SpawnResponse); ok {
			*resp = SpawnResponse{Group: "alpha", ConvID: "conv-pin"}
		}
		return nil
	}
}

func TestRunSpawn_RefusesConflictingFlagsBeforeAskingTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name    string
		params  SpawnParams
		message string
	}{
		{"group context without non-interactive", SpawnParams{GroupContext: true},
			"Error: --group-context requires --non-interactive\n"},
		{"group context and its opt-out", SpawnParams{GroupContext: true, NoGroupContext: true, NonInteractive: true, InitialMessage: "go"},
			"Error: --group-context and --no-group-context are mutually exclusive\n"},
		{"a shell brief without non-interactive", SpawnParams{Harness: harness.ShellName, InitialMessage: "ls"},
			"Error: --harness shell requires --non-interactive with --initial-message or --file\n"},
		{"non-interactive with no brief", SpawnParams{NonInteractive: true},
			"Error: --non-interactive requires --initial-message, --file or --prompt\n"},
		{"non-interactive with an interactive-only flag", SpawnParams{NonInteractive: true, InitialMessage: "go", Owner: true},
			"Error: --owner is not supported with --non-interactive\n"},
		{"the removed break-glass flag", SpawnParams{IUnderstandBreakGlassRisk: true},
			"Error: " + breakGlassFlagRemoved().Error() + "\n"},
		{"sandbox profiles omitted and named", SpawnParams{OmitSandboxProfiles: true, SandboxProfile: "strict"},
			"Error: --omit-sandbox-profiles and --sandbox-profile are mutually exclusive\n"},
		{"owner and no-owner", SpawnParams{Owner: true, NoOwner: true},
			"Error: --owner and --no-owner are mutually exclusive\n"},
		{"a worktree base without a worktree", SpawnParams{WorktreeBase: "main"},
			"Error: --worktree-base requires --worktree\n"},
		{"a worktree repo without a worktree", SpawnParams{WorktreeRepo: "/repo"},
			"Error: --worktree-repo requires --worktree\n"},
		// Two refusals at once: the earlier check answers.
		{"group context first, then owner", SpawnParams{GroupContext: true, Owner: true, NoOwner: true},
			"Error: --group-context requires --non-interactive\n"},
		{"owner before the worktree modifiers", SpawnParams{Owner: true, NoOwner: true, WorktreeBase: "main"},
			"Error: --owner and --no-owner are mutually exclusive\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &spawnDaemon{}
			useSpawnDaemon(t, d)
			params := tc.params
			params.Group = "alpha"
			stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)

			resp, rc := RunSpawn(&params, stdout, stderr, new(bytes.Buffer))

			assert.Nil(t, resp)
			assert.Equal(t, rcInvalidArg, rc)
			assert.Equal(t, tc.message, stderr.String())
			assert.Empty(t, stdout.String())
			assert.Empty(t, d.calls, "the daemon is not asked")
		})
	}
}

// worktreeSpawnDaemon makes a fresh worktree on prepare and answers the spawn
// with spawnErr.
func worktreeSpawnDaemon(spawnErr error) *spawnDaemon {
	return &spawnDaemon{reply: func(path string, _, out any) error {
		switch path {
		case "/v1/worktrees/prepare":
			*out.(*WorktreePrepareResponse) = WorktreePrepareResponse{
				Path: "/repo/.worktrees/feat", Branch: "feat", Created: true, DiscardToken: "discard-pin", LaunchDir: "/repo/.worktrees/feat",
			}
			return nil
		case "/v1/worktrees/discard":
			*out.(*WorktreeDiscardResponse) = WorktreeDiscardResponse{Removed: true}
			return nil
		}
		return spawnErr
	}}
}

func TestRunSpawn_AFailedSpawnTearsDownTheWorktreeItMade(t *testing.T) {
	for _, tc := range []struct {
		name           string
		nonInteractive bool
	}{{"interactive", false}, {"non-interactive", true}} {
		t.Run(tc.name, func(t *testing.T) {
			d := worktreeSpawnDaemon(&DaemonError{Status: http.StatusBadRequest, Code: "invalid_arg", Msg: "pinned refusal"})
			useSpawnDaemon(t, d)
			stderr := new(bytes.Buffer)

			resp, rc := RunSpawn(&SpawnParams{Group: "alpha", Worktree: "feat", Cwd: "/repo",
				NonInteractive: tc.nonInteractive, InitialMessage: "go"}, new(bytes.Buffer), stderr, new(bytes.Buffer))

			assert.Nil(t, resp)
			assert.Equal(t, rcInvalidArg, rc)
			assert.Contains(t, stderr.String(), "pinned refusal")
			assert.Equal(t, []string{"available", "/v1/worktrees/prepare", "/v1/groups/alpha/spawn", "/v1/worktrees/discard"}, d.calls,
				"the worktree made for the spawn is discarded after it fails")
		})
	}
}

func TestRunSpawn_ASpawnThatTimesOutKeepsItsWorktree(t *testing.T) {
	d := worktreeSpawnDaemon(&DaemonError{Status: http.StatusGatewayTimeout, Code: "timeout", Msg: "conv id poll timed out"})
	useSpawnDaemon(t, d)
	stderr := new(bytes.Buffer)

	resp, rc := RunSpawn(&SpawnParams{Group: "alpha", Worktree: "feat", Cwd: "/repo"}, new(bytes.Buffer), stderr, new(bytes.Buffer))

	assert.Nil(t, resp)
	assert.NotEqual(t, rcOK, rc)
	assert.Contains(t, stderr.String(), "Note: kept the worktree /repo/.worktrees/feat — the session may still be coming up.\n")
	assert.Equal(t, []string{"available", "/v1/worktrees/prepare", "/v1/groups/alpha/spawn"}, d.calls, "nothing is discarded")
}

// --remote-control for Claude Code goes on the wire as true, and for a harness
// without it the gate refuses before the daemon is asked. The gate passes an
// accepted value through unchanged, so only the refusal tests the gate itself;
// a gate whose answer differs from its input, --fast-mode inherit, is pinned by
// TestRunSpawn_ExplicitFastModeInheritStaysOnWire.
func TestRunSpawn_AHarnessGateDecidesWhatIsSent(t *testing.T) {
	d := &spawnDaemon{}
	useSpawnDaemon(t, d)
	var sent SpawnRequest
	d.reply = func(_ string, body, out any) error {
		sent = body.(SpawnRequest)
		*out.(*SpawnResponse) = SpawnResponse{Group: "alpha", ConvID: "conv-pin"}
		return nil
	}
	_, rc := RunSpawn(&SpawnParams{Group: "alpha", Harness: harness.DefaultName, RemoteControl: true}, new(bytes.Buffer), new(bytes.Buffer), new(bytes.Buffer))
	require.Equal(t, rcOK, rc)
	require.NotNil(t, sent.RemoteControl)
	assert.True(t, *sent.RemoteControl)

	refused := &spawnDaemon{}
	useSpawnDaemon(t, refused)
	stderr := new(bytes.Buffer)
	_, rc = RunSpawn(&SpawnParams{Group: "alpha", Harness: harness.CodexName, RemoteControl: true}, new(bytes.Buffer), stderr, new(bytes.Buffer))
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "Error:")
	assert.Equal(t, []string{"available"}, refused.calls, "refused before any request")
}
