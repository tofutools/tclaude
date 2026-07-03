package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// pinTmuxNameStyle pins the configured naming style for one test without
// writing a real config file.
func pinTmuxNameStyle(t *testing.T, style string) {
	t.Helper()
	prev := tmuxNameStyleFn
	tmuxNameStyleFn = func() string { return style }
	t.Cleanup(func() { tmuxNameStyleFn = prev })
}

// The historical default: an explicit label verbatim, else the 8-char id
// prefix — the working dir plays no part. (These assertions carry over from
// the pre-style-knob ShortTmuxBase test.)
func TestTmuxNameBase_IDStyle(t *testing.T) {
	pinTmuxNameStyle(t, config.TmuxNameStyleID)
	full := "d0e9fa14-1234-4abc-9def-0123456789ab"
	assert.Equal(t, "d0e9fa14", TmuxNameBase(full, "", "/home/x/myrepo"), "a long id renders as its 8-char prefix; id style ignores the dir")
	assert.Equal(t, "spwn-ab12cd", TmuxNameBase(full, "spwn-ab12cd", "/home/x/myrepo"), "an explicit label wins verbatim, never truncated")
	assert.Equal(t, "abc", TmuxNameBase("abc", "", ""), "an already-short id is left as-is")
}

func TestTmuxNameBase_DirStyle(t *testing.T) {
	pinTmuxNameStyle(t, config.TmuxNameStyleDir)
	full := "d0e9fa14-1234-4abc-9def-0123456789ab"
	assert.Equal(t, "myrepo", TmuxNameBase(full, "", "/home/x/myrepo"), "dir style names the session after the working dir's basename")
	assert.Equal(t, "spwn-ab12cd", TmuxNameBase(full, "spwn-ab12cd", "/home/x/myrepo"), "an explicit label still beats the dir")
	assert.Equal(t, "my-repo-v1-2", TmuxNameBase(full, "", "/home/x/my.repo v1.2"), "dots and spaces sanitize to dashes (tmux rejects '.'/':' in names)")
	assert.Equal(t, "d0e9fa14", TmuxNameBase(full, "", "/"), "a dir with no usable basename falls back to the id prefix")
	assert.Equal(t, "d0e9fa14", TmuxNameBase(full, "", ""), "an empty dir falls back to the id prefix")
}

func TestSanitizeTmuxName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"myrepo", "myrepo"},
		{"already-dashed", "already-dashed"},
		{"under_score", "under_score"},
		{"my.repo", "my-repo"},          // tmux rejects '.' in session names
		{"a:b", "a-b"},                  // tmux rejects ':' in session names
		{"with space", "with-space"},    // spaces make -t targets fragile
		{"a..b--c", "a-b-c"},            // runs collapse
		{"..dots..", "dots"},            // leading/trailing junk trims away
		{"åäö", ""},                     // nothing survives → caller falls back
		{"répo-cité", "r-po-cit"},       // non-ASCII runes dash out
		{".", ""},                       // filepath.Base of an empty cwd
		{"", ""},
		{strings.Repeat("x", 40), strings.Repeat("x", 32)}, // length cap
		{strings.Repeat("x", 31) + "-y", strings.Repeat("x", 31)}, // no trailing dash after the cut
	}
	for _, c := range cases {
		assert.Equal(t, c.want, sanitizeTmuxName(c.in), "sanitizeTmuxName(%q)", c.in)
	}
}
