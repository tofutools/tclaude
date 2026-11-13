package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// fakeWatch stands in for the daemon's watch stream.
func fakeWatch(t *testing.T, events ...map[string]any) *map[string]any {
	t.Helper()
	var sent map[string]any
	var buf bytes.Buffer
	for _, ev := range events {
		require.NoError(t, json.NewEncoder(&buf).Encode(ev))
	}
	prev := pageWatchStream
	pageWatchStream = func(_ context.Context, method, path string, in any) (io.ReadCloser, error) {
		assert.Equal(t, "POST", method)
		assert.Equal(t, "/v1/pages/app/snapshot/watch", path)
		raw, _ := json.Marshal(in)
		_ = json.Unmarshal(raw, &sent)
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	}
	t.Cleanup(func() { pageWatchStream = prev })
	return &sent
}

func TestPageSnapshotWatchPrintsTicksWritesFramesAndTheFinalCapture(t *testing.T) {
	dir := t.TempDir()
	sent := fakeWatch(t,
		map[string]any{"type": "start", "session": "wabc", "every_ms": 250, "for_ms": 3000, "width": 1280, "height": 800, "path": "/", "marks_seq": 2},
		map[string]any{"type": "tick", "n": 1, "elapsed_ms": 40, "in_flight": 2, "requests": 2, "mutations": 12, "text_hash": "aaa", "text_len": 10,
			"layout_hash": "lll", "pixel_hash": "ppp", "title": "loading", "path": "/", "ready_state": "loading", "changed": []string{"net", "dom"}, "still_ms": 0, "unsettled": "it had not loaded", "frame": b64("f1")},
		map[string]any{"type": "tick", "n": 2, "elapsed_ms": 290, "loaded": true, "in_flight": 1, "long_lived": 1, "requests": 3, "mutations": 0, "text_hash": "bbb", "text_len": 99,
			"layout_hash": "mmm", "pixel_hash": "qqq", "title": "ready", "path": "/", "ready_state": "complete", "changed": []string{}, "still_ms": 1250, "calm_ms": 900, "elements": 42},
		map[string]any{"type": "final", "elapsed_ms": 3000, "reason": "for", "still_ms": 1500, "calm_ms": 2500, "png": b64("PNG")},
	)
	out := filepath.Join(dir, "app.png")
	var stdout, stderr bytes.Buffer
	rc := runPageSnapshot(&pageSnapshotParams{Out: out, Watch: true, Every: "250ms", For: "3", Frames: filepath.Join(dir, "frames"), FrameScale: 0.5}, "app", nil, &stdout, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.EqualValues(t, 250, (*sent)["every_ms"])
	assert.EqualValues(t, 3000, (*sent)["for_ms"], "plain seconds work too")
	assert.EqualValues(t, 1, (*sent)["frame_every"])
	assert.EqualValues(t, 0.5, (*sent)["frame_scale"])

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 5, stdout.String())
	assert.Contains(t, lines[0], "session wabc")
	assert.Contains(t, lines[1], "tclaude agent page snapshot app --session wabc")
	assert.NotContains(t, lines[1], "a line here", "no stdin to type into")
	frame := filepath.Join(dir, "frames", "frame-001.png")
	assert.Equal(t, `t=0.0s #1 calm=0.0s still=0.0s changed=net,dom doc=loading(not loaded) net=2/2 dom=+12 text=aaa/10 els=0 layout=lll px=ppp path=/ title="loading" frame=`+frame+
		` (not calm: it had not loaded)`, lines[2])
	assert.Equal(t, `t=0.3s #2 calm=0.9s still=1.2s changed=- doc=complete net=0+1long/3 dom=+0 text=bbb/99 els=42 layout=mmm px=qqq path=/ title="ready"`, lines[3])
	assert.Contains(t, lines[4], "Wrote "+out+" at t=3.0s")
	assert.Contains(t, lines[4], "calm for 2.5s, still for 1.5s")
	assert.Contains(t, lines[4], "marks of batch 2")
	got, err := os.ReadFile(frame)
	require.NoError(t, err)
	assert.Equal(t, "f1", string(got))
	got, err = os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "PNG", string(got))
}

func TestPageSnapshotWatchSaysHowItEnded(t *testing.T) {
	dir := t.TempDir()
	fakeWatch(t,
		map[string]any{"type": "start", "session": "wabc", "path": "/"},
		map[string]any{"type": "end", "elapsed_ms": 2100, "reason": "captured"},
	)
	var stdout, stderr bytes.Buffer
	rc := runPageSnapshot(&pageSnapshotParams{Out: filepath.Join(dir, "x.png"), Watch: true}, "app", nil, &stdout, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Contains(t, stdout.String(), "captured by `page snapshot --session wabc`")
	_, err := os.Stat(filepath.Join(dir, "x.png"))
	assert.True(t, os.IsNotExist(err), "the capture went to the --session command")

	fakeWatch(t,
		map[string]any{"type": "start", "session": "wabc", "path": "/"},
		map[string]any{"type": "error", "code": "left_origin", "error": "the page tried to leave"},
	)
	stdout.Reset()
	rc = runPageSnapshot(&pageSnapshotParams{Out: filepath.Join(dir, "x.png"), Watch: true}, "app", nil, &stdout, &stderr)
	assert.Equal(t, rcIOFailure, rc)
	assert.Contains(t, stderr.String(), "the page tried to leave")

	fakeWatch(t, map[string]any{"type": "start", "session": "wabc", "path": "/"})
	stderr.Reset()
	rc = runPageSnapshot(&pageSnapshotParams{Out: filepath.Join(dir, "x.png"), Watch: true}, "app", nil, &stdout, &stderr)
	assert.Equal(t, rcIOFailure, rc)
	assert.Contains(t, stderr.String(), "without a capture")
}

func TestPageSnapshotFlagsGoTogether(t *testing.T) {
	for _, p := range []pageSnapshotParams{
		{Every: "1s"},
		{Frames: "d"},
		{Keep: true},
		{Session: "w1", Watch: true},
		{Session: "w1", NoMarks: true},
		{Watch: true, Every: "soon"},
		{Watch: true, For: "-2s"},
		{Watch: true, FrameScale: 0.5},
	} {
		var stdout, stderr bytes.Buffer
		rc := runPageSnapshot(&p, "app", nil, &stdout, &stderr)
		assert.Equal(t, rcInvalidArg, rc, "%+v: %s", p, stderr.String())
	}
}
