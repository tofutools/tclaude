package grouplabel

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestPrefixes_ShortestUniqueStart(t *testing.T) {
	prefixes := Prefixes([]string{"tclaude", "counterslop", "webshop", "workers", "web", "Wiki", "tclaude"})
	assert.Equal(t, "t", prefixes["tclaude"])
	assert.Equal(t, "c", prefixes["counterslop"])
	assert.Equal(t, "wo", prefixes["workers"])
	assert.Equal(t, "Wi", prefixes["Wiki"], "compared without case")
	assert.Equal(t, "web", prefixes["web"], "a name another starts with is given in full")
	assert.Equal(t, "webs", prefixes["webshop"])
	assert.Equal(t, "s", Prefixes([]string{"solo"})["solo"])
	assert.Empty(t, Prefixes(nil))
}

func TestLabel_String(t *testing.T) {
	assert.Equal(t, "t/lead", Label{Prefix: "t", Name: "lead"}.String())
	assert.Equal(t, "loner", Label{Name: "loner"}.String())
}

func TestTitleText_GatesTmuxFormats(t *testing.T) {
	assert.Equal(t, "t/lead", TitleText("t/lead"))
	assert.Equal(t, "(rm -rf ~)/lead", TitleText("#(rm -rf ~)/lead"), "no '#', so tmux expands nothing")
	assert.Equal(t, "a b c", TitleText("a\tb\n\x07\x1bc"), "one line, no control characters")
	assert.Equal(t, "100d", TitleText("100%d"), "no strftime")
	assert.Equal(t, "tclaudes-victim", TitleText("tclaude:s-victim"), "cannot pass for another session's focus token")
	assert.Equal(t, "x;Start-Process calc;", TitleText("x\u2019;Start-Process calc;\u2018"), "no PowerShell quote of any kind")
	assert.Equal(t, "dont", TitleText("don't"))
	assert.Len(t, []rune(TitleText(strings.Repeat("é", 200))), maxTitleRunes)
}

// Two sessions whose agents are both named lead, one spawned into tclaude and
// one in counterslop (and also tclaude): each reads as its group's lead.
func TestForSession_TwoLeads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	tclaude, err := db.CreateAgentGroup("tclaude", "")
	require.NoError(t, err)
	counterslop, err := db.CreateAgentGroup("counterslop", "")
	require.NoError(t, err)
	lead := func(session, conv string, groups ...int64) {
		require.NoError(t, db.SaveSession(&db.SessionRow{ID: session, TmuxSession: session, ConvID: conv, Status: "idle"}))
		_, _, err := db.EnsureAgentForConv(conv, "test")
		require.NoError(t, err)
		require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: conv, CustomTitle: "lead"}))
		for _, group := range groups {
			require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: group, ConvID: conv}))
		}
	}
	lead("s-one", "c1000000-0000-4000-8000-000000000001", tclaude)
	lead("s-two", "c1000000-0000-4000-8000-000000000002", tclaude, counterslop)
	lead("s-none", "c1000000-0000-4000-8000-000000000003")
	// A session whose conversation has a first prompt but no given name.
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "s-unnamed", TmuxSession: "s-unnamed", ConvID: "c1000000-0000-4000-8000-000000000004", Status: "idle"}))
	_, _, err = db.EnsureAgentForConv("c1000000-0000-4000-8000-000000000004", "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: "c1000000-0000-4000-8000-000000000004", FirstPrompt: "Please implement the foo"}))

	one, ok := ForSession("s-one")
	require.True(t, ok)
	assert.Equal(t, "t/lead", one.String())
	two, ok := ForSession("s-two")
	require.True(t, ok)
	// No spawn group recorded: its first group by name.
	assert.Equal(t, "c/lead", two.String())
	none, ok := ForSession("s-none")
	require.True(t, ok)
	assert.Equal(t, "lead", none.String(), "an ungrouped agent has no prefix")
	_, ok = ForSession("s-missing")
	assert.False(t, ok)
	_, ok = ForSession("s-unnamed")
	assert.False(t, ok, "a first prompt is not a name")
}
