package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The Codex SSH workaround's command is regenerated per launch; it is not a
// change anyone made.
func TestRelaunchChangesIgnoresTheRegeneratedSSHCommand(t *testing.T) {
	snapshot := func(command string) *sandboxpolicy.Snapshot {
		effective := sandboxpolicy.EmptySnapshot().Effective
		effective.Environment = []sandboxpolicy.EnvironmentEntry{{Name: codexSSHCommandEnv, Value: command}}
		effective.Provenance.Environment = map[string]sandboxpolicy.ProfileSource{
			codexSSHCommandEnv: codexSSHSource()}
		s := sandboxpolicy.NewSnapshot(effective, nil)
		return &s
	}
	assert.Empty(t, relaunchChanges(snapshot("ssh -F /gen/one"), snapshot("ssh -F /gen/two")))
}

func TestRelaunchChangesNamesWithoutValues(t *testing.T) {
	before, after := sandboxpolicy.EmptySnapshot(), sandboxpolicy.EmptySnapshot()
	before.LaunchEnvironment = []sandboxpolicy.EnvironmentEntry{{Name: "API_KEY", Value: "old-value"}}
	after.LaunchEnvironment = []sandboxpolicy.EnvironmentEntry{{Name: "API_KEY", Value: "new-value"}}
	after.Effective.NetworkAccess = "none"
	got := relaunchChanges(&before, &after)
	assert.Contains(t, got, "group environment: changed API_KEY")
	assert.Contains(t, got, "network access: default → none")
	for _, line := range got {
		assert.NotContains(t, line, "value")
	}
	assert.Nil(t, relaunchChanges(nil, &after))
}

func TestRelaunchChangesNamesEachAxis(t *testing.T) {
	before, after := sandboxpolicy.EmptySnapshot(), sandboxpolicy.EmptySnapshot()
	before.Applied = []sandboxpolicy.AppliedProfile{{Scope: sandboxpolicy.ScopeExplicit, ID: 1, Name: "a"}}
	after.Applied = []sandboxpolicy.AppliedProfile{{Scope: sandboxpolicy.ScopeExplicit, ID: 2, Name: "b"}}
	before.Effective.Filesystem = []sandboxpolicy.FilesystemGrant{{Path: "/keep", Access: "read"}, {Path: "/gone", Access: "read"}}
	after.Effective.Filesystem = []sandboxpolicy.FilesystemGrant{{Path: "/keep", Access: "read"}, {Path: "/new", Access: "write"}, {Path: "/new2", Access: "read"}}
	before.Effective.Network = &sandboxpolicy.NetworkRules{Packs: []string{"github"}}
	after.Effective.Network = &sandboxpolicy.NetworkRules{Packs: []string{"github", "npm"}}
	after.Effective.UnixSockets = &sandboxpolicy.UnixSocketRules{Mode: "allow"}
	after.Effective.Tmpfs = []sandboxpolicy.TmpfsMount{{Path: "/scratch"}}
	assert.Equal(t, []string{
		`sandbox profiles: explicit "a" → explicit "b"`,
		"filesystem rules: 2 added, 1 removed",
		"network rules changed",
		"temporary filesystems changed",
		"unix sockets changed",
	}, relaunchChanges(&before, &after))
}

// A stored snapshot drops empty axes from its JSON; that is not a change.
func TestRelaunchChangesTreatsEmptyAndMissingAxesAlike(t *testing.T) {
	before, after := sandboxpolicy.EmptySnapshot(), sandboxpolicy.EmptySnapshot()
	before.Effective.Tmpfs, after.Effective.Tmpfs = nil, []sandboxpolicy.TmpfsMount{}
	before.Effective.PreLaunch, after.Effective.PreLaunch = []sandboxpolicy.PreLaunchBlock{}, nil
	assert.Empty(t, relaunchChanges(&before, &after))
}
