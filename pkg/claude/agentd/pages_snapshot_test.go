package agentd

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotTokenGoesOnlyOnThePagesOwnDocuments(t *testing.T) {
	const origin = "http://f-0123456789abcdef0123456789abcdef.ui.localhost:7777"
	headers := map[string]string{"User-Agent": "x", stageSnapshotHeader: "forged"}

	h := snapshotHeadersFor(origin+"/checkout?a=1", "Document", origin, "tok", headers)
	require.NotNil(t, h)
	var got []string
	for _, kv := range h {
		if strings.EqualFold(kv["name"], stageSnapshotHeader) {
			got = append(got, kv["value"])
		}
	}
	assert.Equal(t, []string{"tok"}, got, "exactly one token header, the daemon's")

	assert.Nil(t, snapshotHeadersFor(origin+"/app.js", "Script", origin, "tok", headers), "subresources carry their own fetch metadata")
	assert.Nil(t, snapshotHeadersFor("http://evil.localhost:7777/", "Document", origin, "tok", headers), "another origin never sees the token")
	assert.Nil(t, snapshotHeadersFor("http://f-0123456789abcdef0123456789abcdef.ui.localhost:7778/", "Document", origin, "tok", headers),
		"nor the same host on another port")
	assert.Nil(t, snapshotHeadersFor(origin+"/", "Document", origin, "", headers))
}

func TestFindChromeFailsClearlyWithoutOne(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TCLAUDE_CHROME", "")
	_, err := findChrome()
	if err == nil {
		t.Skip("an app-bundle Chrome is installed on this host")
	}
	assert.True(t, errors.Is(err, errNoChrome))
	assert.Contains(t, err.Error(), "never downloads")

	t.Setenv("TCLAUDE_CHROME", "/nonexistent/chrome")
	_, err = findChrome()
	assert.ErrorContains(t, err, "TCLAUDE_CHROME")
}

func TestPageMarksSVGEscapesText(t *testing.T) {
	svg := pageMarksSVG([]pageMark{
		{Kind: "text", Color: "#e03131", Points: [][2]int{{1, 2}}, Text: `</svg><script>alert(1)</script>`},
		{Kind: "pen", Color: `red" onload="x`, Points: [][2]int{{1, 2}, {3, 4}}},
	})
	assert.NotContains(t, svg, "<script>")
	assert.Contains(t, svg, "&lt;script&gt;")
	assert.NotContains(t, svg, "onload", "an unchecked colour falls back")
	assert.Contains(t, svg, `points="1,2 3,4"`)
}
