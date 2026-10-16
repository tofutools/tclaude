package agentd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPRBodyExcerpt(t *testing.T) {
	body := "<!-- template hint -->\r\n## Background / Purpose\r\n\r\n" +
		"> Follow-up to [#3074](https://github.com/x/y/pull/3074): keep `focus`.\n\n" +
		"![shot](https://img)\n```go\nfmt.Println(\"no\")\n```\n" +
		"| a | b |\n|---|---|\n---\n" +
		"1. First *point*\n- [x] Done <b>item</b>\n\n🤖 Generated"
	assert.Equal(t, "Follow-up to #3074: keep focus. First *point* Done item 🤖 Generated",
		prBodyExcerpt(body, 420))

	assert.Equal(t, "", prBodyExcerpt("## Only a heading\n", 420))

	long := strings.Repeat("word ", 200)
	got := prBodyExcerpt(long, 50)
	assert.True(t, strings.HasSuffix(got, "…"))
	assert.LessOrEqual(t, len([]rune(got)), 51)
	assert.NotContains(t, got, "wor…", "cut at a word boundary")
}

func TestParsePRDetailsDraftAndZeroTimes(t *testing.T) {
	d, ok := parsePRDetails(`{"title":" T ","state":"OPEN","isDraft":true,"mergedAt":"0001-01-01T00:00:00Z","commits":[]}`)
	assert.True(t, ok)
	assert.Equal(t, "T", d.Title)
	assert.Equal(t, "draft", d.State)
	assert.Equal(t, "", d.MergedAt)
	_, ok = parsePRDetails("not json")
	assert.False(t, ok)
}
