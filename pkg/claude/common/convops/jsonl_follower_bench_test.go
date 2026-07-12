package convops

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// buildLargeTranscript writes a transcript of approximately targetBytes to a
// temp file, modelling a busy streaming agent: each turn carries a ~2 KB
// tool-result blob, so a multi-MB transcript is a few thousand turns. Every
// turn stamps the branch (as Claude Code does), so the branch fold and the
// last-wins fields are exercised at scale. Returns the path.
func buildLargeTranscript(b *testing.B, targetBytes int) string {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, followerTestConvID+".jsonl")

	blob := make([]byte, 2048)
	for i := range blob {
		blob[i] = 'x'
	}
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	written := 0
	i := 0
	for written < targetBytes {
		ts := fmt.Sprintf("2026-03-01T%02d:%02d:%02dZ", i/3600%24, i/60%60, i%60)
		var line string
		if i%2 == 0 {
			line = fmt.Sprintf(
				`{"type":"user","cwd":"/proj","gitBranch":"main","timestamp":%q,"message":{"role":"user","content":"turn %d %s"}}`,
				ts, i, blob) + "\n"
		} else {
			line = fmt.Sprintf(
				`{"type":"assistant","cwd":"/proj","gitBranch":"main","timestamp":%q,"message":{"role":"assistant","content":"reply %d %s"}}`,
				ts, i, blob) + "\n"
		}
		n, err := f.WriteString(line)
		if err != nil {
			b.Fatal(err)
		}
		written += n
		i++
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	return path
}

// appendOneTurn appends a single ~2 KB turn — the "one new turn arrived"
// event that triggers a debounced reindex.
func appendOneTurn(b *testing.B, path string, seq int) {
	b.Helper()
	blob := make([]byte, 2048)
	for i := range blob {
		blob[i] = 'y'
	}
	line := fmt.Sprintf(
		`{"type":"user","cwd":"/proj","gitBranch":"main","timestamp":"2026-03-02T00:00:00Z","message":{"role":"user","content":"new %d %s"}}`,
		seq, blob) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkReindexFullReparse measures the pre-follower cost: a full
// parseJSONLSession of the whole multi-MB transcript on every reindex. The
// append is excluded from the timer so the number is purely the reparse.
func BenchmarkReindexFullReparse(b *testing.B) {
	path := buildLargeTranscript(b, 3*1024*1024)
	seq := 0
	for b.Loop() {
		b.StopTimer()
		appendOneTurn(b, path, seq)
		seq++
		b.StartTimer()
		if entry, _ := parseJSONLSession(path, followerTestConvID); entry == nil {
			b.Fatal("nil entry")
		}
	}
}

// BenchmarkReindexIncremental measures the follower: after priming on the
// multi-MB base, each reindex reads only the freshly-appended turn. Same
// growing file and same excluded append as the full-reparse benchmark, so
// the two numbers are directly comparable.
func BenchmarkReindexIncremental(b *testing.B) {
	path := buildLargeTranscript(b, 3*1024*1024)
	f := newConvFollower(followerTestConvID)
	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	if _, _, err := f.refresh(path, info); err != nil { // prime the cursor
		b.Fatal(err)
	}
	seq := 0
	for b.Loop() {
		b.StopTimer()
		appendOneTurn(b, path, seq)
		seq++
		info, err := os.Stat(path)
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		entry, _, err := f.refresh(path, info)
		if err != nil || entry == nil {
			b.Fatalf("refresh: entry=%v err=%v", entry, err)
		}
	}
}
