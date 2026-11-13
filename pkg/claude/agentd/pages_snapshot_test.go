package agentd

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestDefaultSnapshotWaitsForThePageToGoStillAfterItLoads(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	curtain := pageProbe{InFlight: 1, Requests: 2, TextHash: "c", Layout: "c"}
	loaded := curtain
	loaded.Loaded = true
	painted := pageProbe{Loaded: true, Requests: 2, Mutations: 30, TextHash: "p", Layout: "p"}
	quiet := painted
	quiet.Mutations = 0

	var w pageSettleWait
	for _, step := range []struct {
		p  pageProbe
		at int
	}{{curtain, 0}, {loaded, 100}, {loaded, 1400}, {painted, 1500}, {quiet, 1600}, {quiet, 1900}} {
		capture, _ := w.observe(step.p, ms(step.at))
		require.False(t, capture, "at %dms: the data is still loading, or the page has just painted", step.at)
	}
	capture, s := w.observe(quiet, ms(2000))
	assert.True(t, capture, "still for 500ms after it painted")
	assert.True(t, s.Settled)
	assert.EqualValues(t, 2000, s.WaitedMS)

	// A long-poll does not keep the page from going still.
	var lp pageSettleWait
	poll := quiet
	poll.InFlight, poll.LongLived = 1, 1
	lp.observe(poll, 0)
	capture, _ = lp.observe(poll, ms(400))
	assert.False(t, capture, "at least pageSnapshotSettle after the load, and still for pageSnapshotQuiet")
	capture, s = lp.observe(poll, ms(500))
	assert.True(t, capture)
	assert.True(t, s.Settled)

	// A page that never goes still is captured at the cap, saying why.
	var busy pageSettleWait
	var at time.Duration
	for i := 0; ; i++ {
		p := painted
		p.TextHash = strconv.Itoa(i)
		capture, s = busy.observe(p, at)
		if capture {
			break
		}
		at += pageSnapshotProbeTick
		require.Less(t, at, 2*pageSnapshotSettleMax)
	}
	assert.False(t, s.Settled)
	assert.Equal(t, pageSnapshotSettleMax, at)
	assert.Contains(t, s.Why, "the DOM kept changing")
	assert.Contains(t, s.Why, "its text kept changing")

	// Before the load nothing is captured, however still the page looks.
	var never pageSettleWait
	for at := time.Duration(0); at < 2*pageSnapshotSettleMax; at += time.Second {
		capture, _ := never.observe(curtain, at)
		require.False(t, capture)
	}
}

func TestChangeTrackerSaysWhatChangedAndForHowLongNothingHas(t *testing.T) {
	var tr pageChangeTracker
	a := pageProbe{Loaded: true, Requests: 3, TextHash: "a", Layout: "a", Pixels: "a", Title: "t", Path: "/"}
	tr.observe(a, 0)
	assert.Empty(t, tr.observe(a, time.Second))
	assert.Equal(t, time.Second, tr.still(time.Second))
	b := a
	b.Pixels, b.Requests = "b", 4
	assert.Equal(t, []string{"net", "pixels"}, tr.observe(b, 2*time.Second))
	assert.Zero(t, tr.still(2*time.Second))
	c := b
	c.Pixels = "" // not asked: no change
	assert.Empty(t, tr.observe(c, 3*time.Second))
}
