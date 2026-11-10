package legacy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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
	// Reading records where each conversation came from.
	for i, key := range []string{"fixer", "fixer-2"} {
		c := got.Convs[i]
		assert.Equal(t, key, c.AgentKey)
		assert.Equal(t, "claude", c.Harness)
		assert.Equal(t, ClaudeHistoryFormat, c.HistoryFormat)
		got.Convs[i].AgentKey, got.Convs[i].Harness, got.Convs[i].HistoryFormat = "", "", ""
	}
	assert.Equal(t, exp, got)
}

// A foreign member in another harness keeps its harness and native
// history; one with no conversation imports as configuration.
func TestForeignGroupPAFKeepsEachMembersHarness(t *testing.T) {
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindGroup, paf.Limits{})
	native := paf.AgentPath("cody", "native/codex-rollout-jsonl/history.jsonl")
	require.NoError(t, w.AddAgent("cody", paf.Agent{Name: "cody", Native: &paf.Native{Format: "codex-rollout-jsonl", Path: native, Harness: "codex", SourceConvID: convA}}, paf.Environment{Harness: paf.Harness{Name: "codex"}}))
	require.NoError(t, w.WriteFile(native, "application/jsonl", []byte("{}\n")))
	require.NoError(t, w.AddAgent("gem", paf.Agent{Name: "gem"}, paf.Environment{Harness: paf.Harness{Name: "gemini"}}))
	require.NoError(t, w.WriteJSON(paf.GroupPath, paf.Group{Name: "g", Members: []paf.GroupMember{{Agent: "cody"}, {Agent: "gem"}}}))
	_, err := w.Close(paf.Producer{Name: "other"})
	require.NoError(t, err)
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)
	exp, err := ToGroupExport(a, 1<<20)
	require.NoError(t, err)
	require.Len(t, exp.Convs, 2)
	assert.Equal(t, convA, exp.Convs[0].ConvID)
	assert.Equal(t, "codex", exp.Convs[0].Harness)
	assert.Equal(t, "codex-rollout-jsonl", exp.Convs[0].HistoryFormat)
	assert.Equal(t, "{}\n", string(exp.Convs[0].Content))
	assert.Equal(t, "gemini", exp.Convs[1].Harness)
	assert.True(t, exp.Convs[1].Missing)
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

func TestForeignGroupPAFRefusesDuplicateMembers(t *testing.T) {
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindGroup, paf.Limits{})
	native := paf.AgentPath("a", "native/claude-jsonl/history.jsonl")
	require.NoError(t, w.AddAgent("a", paf.Agent{Name: "a", Native: &paf.Native{Format: ClaudeHistoryFormat, Path: native, SourceConvID: convA}}, paf.Environment{Harness: paf.Harness{Name: "claude"}}))
	require.NoError(t, w.WriteFile(native, "application/jsonl", []byte("{}\n")))
	require.NoError(t, w.WriteJSON(paf.GroupPath, paf.Group{Name: "g", Members: []paf.GroupMember{{Agent: "a"}, {Agent: "a"}}}))
	_, err := w.Close(paf.Producer{Name: "other"})
	require.NoError(t, err)
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)
	_, err = ToGroupExport(a, 1<<20)
	assert.ErrorContains(t, err, "listed twice")
}

func TestGroupExportBlankTitleAndLargeManifest(t *testing.T) {
	exp := sampleGroupExport()
	exp.Convs[0].Title = "  "
	// Message history larger than the metadata bound.
	exp.Messages = append(exp.Messages, groupexport.Message{ID: 8, FromConv: convA, ToConv: convB, Body: strings.Repeat("x", 2<<20)})
	dir := testutil.CanonicalTempDir(t)
	raw, err := groupexport.Marshal(exp)
	require.NoError(t, err)
	file := filepath.Join(dir, "team.zip")
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	o, err := OpenFile(file, dir, paf.Producer{Name: "tclaude"}, paf.Options{Purpose: paf.PurposeInspect})
	require.NoError(t, err)
	defer func() { _ = o.Close() }()
	got, err := ToGroupExport(o.Archive, 64<<20)
	require.NoError(t, err)
	assert.Len(t, got.Messages, 2)

	_, err = OpenFile(file, dir, paf.Producer{Name: "tclaude"}, paf.Options{Purpose: paf.PurposeInspect, Limits: paf.Limits{MaxArchiveBytes: 1 << 20}})
	assert.ErrorContains(t, err, "expands past")
}

// A converted member's stored report travels in the group PAF even though a
// group PAF carries no neutral context, and a reader accepts it.
func TestGroupExportCarriesStoredLossReports(t *testing.T) {
	exp := sampleGroupExport()
	var buf bytes.Buffer
	require.NoError(t, WriteGroupExport(exp, &buf, GroupWriteOptions{
		Producer:      paf.Producer{Name: "tclaude", Version: "test"},
		EarlierLosses: map[string]*paf.Losses{convA: {Source: "codex", Target: "claude", Items: []paf.LossItem{}}},
	}))
	a, err := paf.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)
	l, ok := a.Losses("fixer")
	require.True(t, ok)
	assert.Equal(t, "codex", l.Source)
	_, ok = a.Losses("fixer-2")
	assert.False(t, ok)
}
