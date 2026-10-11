package legacy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const (
	convA = "aaaaaaaa-1111-2222-3333-444444444444"
	convB = "bbbbbbbb-1111-2222-3333-444444444444"
	convC = "cccccccc-1111-2222-3333-444444444444"
)

func sampleGroupExport() *groupexport.Export {
	return &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		ExportedAt:    "2026-10-11T19:40:00.123456789Z",
		SchemaVersion: 181,
		SourceGroup:   "team",
		SourceHome:    "/home/me",
		SourceOS:      "linux",
		Group:         groupexport.Group{Descr: "the team", DefaultContext: "be kind", MaxMembers: 5, CreatedAt: "2026-10-01T00:00:00Z"},
		Members: []groupexport.Member{
			{ConvID: convA, Role: "lead", JoinedAt: "2026-10-01T00:00:00Z"},
			{ConvID: convB, Role: "worker", JoinedAt: "2026-10-01T00:00:00Z"},
			{ConvID: convC, Role: "worker", JoinedAt: "2026-10-01T00:00:00Z"},
		},
		Owners:   []groupexport.Owner{{ConvID: convA, GrantedAt: "2026-10-01T00:00:00Z", GrantedBy: "human"}},
		Messages: []groupexport.Message{{ID: 7, FromConv: convA, ToConv: convB, Body: "hi", CreatedAt: "2026-10-02T00:00:00Z"}},
		Convs: []groupexport.Conv{
			{ConvID: convA, SourceCwd: "/home/me/w", Title: "Fixer", Content: []byte(`{"type":"user","sessionId":"` + convA + `"}` + "\n")},
			// Two titles that derive the same agent key.
			{ConvID: convB, SourceCwd: "/home/me/w", Title: "fixer", Content: []byte{}},
			{ConvID: convC, Title: "gone", Missing: true},
		},
	}
}

func TestGroupExportRoundTripsThroughPAF(t *testing.T) {
	exp := sampleGroupExport()
	var buf bytes.Buffer
	require.NoError(t, WriteGroupExport(exp, &buf, GroupWriteOptions{Producer: paf.Producer{Name: "tclaude", Version: "test"}}))
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)

	assert.Equal(t, paf.KindGroup, a.Manifest.Kind)
	assert.Equal(t, []string{"fixer", "fixer-2", "gone"}, a.Manifest.Agents)
	g := a.Group()
	assert.Equal(t, "team", g.Name)
	assert.Equal(t, "the team", g.Description)
	require.Len(t, g.Members, 3)
	var owner MemberExt
	_, err = g.Members[0].Extensions.Decode(paf.ExtTclaude, &owner)
	require.NoError(t, err)
	assert.True(t, owner.Owner)
	gone, _ := a.Agent("gone")
	assert.Nil(t, gone.Native, "a conversation missing at export has no native blob")

	got, err := ToGroupExport(a, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, exp, got)
}

func TestForeignGroupPAFRefusesOtherHarnesses(t *testing.T) {
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindGroup, paf.Limits{})
	require.NoError(t, w.AddAgent("cody", paf.Agent{Name: "cody"}, paf.Environment{Harness: paf.Harness{Name: "codex"}}))
	require.NoError(t, w.WriteJSON(paf.GroupPath, paf.Group{Name: "g", Members: []paf.GroupMember{{Agent: "cody"}}}))
	_, err := w.Close(paf.Producer{Name: "other"})
	require.NoError(t, err)
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)
	_, err = ToGroupExport(a, 1<<20)
	assert.ErrorContains(t, err, `group member "cody" has no Claude Code history`)
}

func TestOpenFileReadsGroupExportZip(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	raw, err := groupexport.Marshal(sampleGroupExport())
	require.NoError(t, err)
	file := filepath.Join(dir, "team.zip")
	require.NoError(t, os.WriteFile(file, raw, 0o600))

	o, err := OpenFile(file, dir, paf.Producer{Name: "tclaude"}, paf.Options{Purpose: paf.PurposeInspect})
	require.NoError(t, err)
	defer func() { _ = o.Close() }()
	assert.Equal(t, SourceGroupExport, o.Source)
	assert.Equal(t, paf.KindGroup, o.Manifest.Kind)
	assert.Len(t, o.Manifest.Agents, 3)
}
