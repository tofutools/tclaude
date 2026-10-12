package managedinstructions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func instructionTestHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
}
func TestManagedInstructionGroupCollisionAndRules(t *testing.T) {
	instructionTestHome(t)
	a, err := db.CreateAgentGroup("alpha", "")
	require.NoError(t, err)
	b, err := db.CreateAgentGroup("beta", "")
	require.NoError(t, err)
	_, err = db.PutManagedInstruction(0, "rule", "user", []byte("user rule"), false)
	require.NoError(t, err)
	_, err = db.PutManagedInstruction(a, "rule", "group", []byte("group rule"), false)
	require.NoError(t, err)
	putSkill := func(group int64, text string) {
		raw, e := json.Marshal(skillbundle.Bundle{Format: skillbundle.Format, Name: "review", Files: []skillbundle.File{{Path: "SKILL.md", Data: []byte(text)}}})
		require.NoError(t, e)
		_, e = db.PutManagedInstruction(group, "skill", "review", raw, false)
		require.NoError(t, e)
	}
	putSkill(0, "user skill")
	putSkill(a, "alpha skill")
	groups := []*db.AgentGroup{{ID: a, Name: "alpha"}, {ID: b, Name: "beta"}}
	items, err := Resolve(groups)
	require.NoError(t, err)
	require.Len(t, items, 3)
	require.Equal(t, "user", items[0].Scope)
	require.Equal(t, a, items[2].GroupID)
	putSkill(b, "beta skill")
	_, err = Resolve(groups)
	require.ErrorContains(t, err, "alpha")
	require.ErrorContains(t, err, "beta")
}
func TestManagedInstructionMaterializeRegisteredBytesOnly(t *testing.T) {
	instructionTestHome(t)
	bundle := skillbundle.Bundle{Format: skillbundle.Format, Name: "review", Files: []skillbundle.File{{Path: "SKILL.md", Data: []byte("Review carefully")}, {Path: "bin/check", Data: []byte("#!/bin/sh\nexit 0\n"), Executable: true}}}
	raw, err := json.Marshal(bundle)
	require.NoError(t, err)
	items := []db.ManagedInstruction{{Kind: "skill", Name: "review", Content: raw}, {Kind: "rule", Name: "safe", Content: []byte("Do not weaken receiver rules")}}
	launch, err := Materialize(items)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(launch.Root) })
	encoded, err := json.Marshal(items)
	require.NoError(t, err)
	require.NoError(t, db.SaveManagedInstructionLaunch(db.ManagedInstructionLaunch{AgentID: "fixture", Root: launch.Root, Body: launch.Body, Resources: string(encoded)}))
	require.NoError(t, VerifyLaunchRoot(launch.Root))
	require.True(t, HasNativeSkills(launch.Root))
	require.Contains(t, launch.Body, "Do not weaken receiver rules")
	require.NoDirExists(t, filepath.Join(os.Getenv("HOME"), ".claude", "skills"))
	require.NoDirExists(t, filepath.Join(os.Getenv("HOME"), ".agents", "skills"))
	rootFile := filepath.Join(launch.Root, "skills", "review", "SKILL.md")
	require.NoError(t, os.Chmod(rootFile, 0600))
	require.NoError(t, os.WriteFile(rootFile, []byte("tampered"), 0600))
	require.ErrorContains(t, VerifyLaunchRoot(launch.Root), "changed")
}

func TestManagedInstructionImportExactDirectory(t *testing.T) {
	root := testutil.CanonicalTempDir(t)
	skill := filepath.Join(root, ".release-skill")
	require.NoError(t, os.MkdirAll(skill, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("Release fixture"), 0600))
	raw, err := Import("skill", "release", skill)
	require.NoError(t, err)
	bundle, err := skillbundle.Decode(raw)
	require.NoError(t, err)
	require.Equal(t, "release", bundle.Name)
	require.Len(t, bundle.Files, 1)
	require.Equal(t, "Release fixture", string(bundle.Files[0].Data))
	require.NoError(t, os.Symlink(filepath.Join(skill, "SKILL.md"), filepath.Join(skill, "alias")))
	_, err = Import("skill", "release", skill)
	require.ErrorContains(t, err, "symlink")
}

func TestManagedInstructionHookApprovalParsesFrontmatter(t *testing.T) {
	for _, metadata := range []string{"hooks: {}", `"hooks": {}`, "hooks : {}", "base: &hooks {hooks: {}}\n<<: *hooks", "hooks: [invalid", "description: safe\n---missing"} {
		t.Run(metadata, func(t *testing.T) {
			require.True(t, RequiresCodeApproval(skillbundle.File{Path: "SKILL.md", Data: []byte("---\n" + metadata + "\n---\nGuidance")}))
		})
	}
	require.False(t, RequiresCodeApproval(skillbundle.File{Path: "SKILL.md", Data: []byte("---\ndescription: Safe prose\n---\nGuidance")}))
}

func TestManagedInstructionReceiverPrecedenceAndCarriedDedup(t *testing.T) {
	receiver := []db.ManagedInstruction{{Kind: "rule", Name: "safety", Scope: "user", Content: []byte("Receiver instruction")}}
	carried := []db.ManagedInstruction{{Kind: "rule", Name: "old", Scope: "carried", Content: []byte("Carried instruction")}}
	merged, err := Merge(receiver, carried)
	require.NoError(t, err)
	repeated, err := Merge(receiver, merged)
	require.NoError(t, err)
	require.Len(t, repeated, 2)
	launch, err := Materialize(repeated)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(launch.Root) })
	require.Contains(t, launch.Body, "win every conflict with carried guidance")
	require.Contains(t, launch.Body, "Receiver instruction")
	require.Contains(t, launch.Body, "Carried instruction")
}

func TestManagedInstructionDedupKeepsLocalAuthorityAndCarriedLifetime(t *testing.T) {
	local := db.ManagedInstruction{Kind: "rule", Name: "local", Scope: "user", Content: []byte("same guidance")}
	carried := db.ManagedInstruction{Kind: "rule", Name: "remote", Scope: "carried", Content: []byte("same guidance"), Carried: true}
	merged, err := Merge([]db.ManagedInstruction{local}, []db.ManagedInstruction{carried})
	require.NoError(t, err)
	require.Len(t, merged, 1)
	require.Equal(t, "user", merged[0].Scope, "deduplication must not demote the receiver's authority")
	require.True(t, merged[0].Carried, "the identical carried bytes must survive a later local edit")
	previous := merged[0]
	previous.Scope = "carried"
	local.Content = []byte("updated receiver guidance")
	refreshed, err := Merge([]db.ManagedInstruction{local}, []db.ManagedInstruction{previous})
	require.NoError(t, err)
	require.Len(t, refreshed, 2)
	require.Equal(t, "user", refreshed[0].Scope)
	require.Equal(t, "carried", refreshed[1].Scope)
}
