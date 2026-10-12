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
