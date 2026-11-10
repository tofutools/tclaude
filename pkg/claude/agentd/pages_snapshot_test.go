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

	allow, h := snapshotRequest(origin+"/checkout?a=1", "Document", origin, "tok", headers)
	require.True(t, allow)
	require.NotNil(t, h)
	var got []string
	for _, kv := range h {
		if strings.EqualFold(kv["name"], stageSnapshotHeader) {
			got = append(got, kv["value"])
		}
	}
	assert.Equal(t, []string{"tok"}, got, "exactly one token header, the daemon's")

	allow, h = snapshotRequest(origin+"/app.js", "Script", origin, "tok", headers)
	assert.True(t, allow)
	assert.Nil(t, h, "subresources carry their own fetch metadata")
	for _, other := range []string{
		"http://169.254.169.254/latest/meta-data/", "http://127.0.0.1:8080/admin", "http://evil.localhost:7777/",
		"http://f-0123456789abcdef0123456789abcdef.ui.localhost:7778/", "https://f-0123456789abcdef0123456789abcdef.ui.localhost:7777/",
	} {
		for _, kind := range []string{"Document", "Image", "XHR"} {
			allow, h = snapshotRequest(other, kind, origin, "tok", headers)
			assert.False(t, allow, "%s %s: the snapshot renders only the page's own origin", kind, other)
			assert.Nil(t, h)
		}
	}
	allow, h = snapshotRequest(origin+"/", "Document", origin, "", headers)
	assert.True(t, allow)
	assert.Nil(t, h)
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
