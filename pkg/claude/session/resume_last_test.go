package session

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestNewestResumableConv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	now := time.Now()
	entry := func(id string, age time.Duration) convops.SessionEntry {
		// MessageCount stays 0: the index doesn't populate it.
		return convops.SessionEntry{SessionID: id, FileMtime: now.Add(-age)}
	}
	retired := entry("retired", time.Minute)
	_, err := db.AllocateAgent(retired.SessionID, "test")
	require.NoError(t, err)
	_, err = db.RetireAgent(retired.SessionID, "test", "test")
	require.NoError(t, err)
	archived := entry("archived", 2*time.Minute)
	archived.ArchivedAt = now.Format(time.RFC3339)
	sidechain := entry("sidechain", 3*time.Minute)
	sidechain.IsSidechain = true
	older := entry("older", time.Hour)
	newest := entry("newest", 5*time.Minute)

	got, skipped, err := newestResumableConv([]convops.SessionEntry{older, retired, archived, sidechain, newest})
	require.NoError(t, err)
	assert.Equal(t, "newest", got)
	assert.Equal(t, "retired", skipped)

	got, skipped, err = newestResumableConv([]convops.SessionEntry{retired})
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, "retired", skipped)
}

func TestParseResumePassthrough(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		selector string
		last     bool
		rest     []string
		none     bool
	}{
		{name: "continue", args: []string{"--continue"}, last: true},
		{name: "short continue keeps the rest", args: []string{"-c", "--model", "opus"}, last: true, rest: []string{"--model", "opus"}},
		{name: "resume with id", args: []string{"--resume", "abc"}, selector: "abc"},
		{name: "short resume with id", args: []string{"-r", "abc", "--verbose"}, selector: "abc", rest: []string{"--verbose"}},
		{name: "resume equals", args: []string{"--resume=abc"}, selector: "abc"},
		{name: "bare resume is the picker", args: []string{"--resume"}, none: true},
		{name: "resume before a flag is the picker", args: []string{"--resume", "--verbose"}, none: true},
		{name: "fork changes the meaning", args: []string{"--continue", "--fork-session"}, none: true},
		{name: "print stays with claude", args: []string{"-p", "--continue"}, none: true},
		{name: "both is claude's error to report", args: []string{"-c", "-r", "abc"}, none: true},
		{name: "unrelated", args: []string{"--model", "opus"}, none: true},
		{name: "empty", none: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseResumePassthrough(tc.args)
			if tc.none {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.selector, got.convSelector)
			assert.Equal(t, tc.last, got.last)
			assert.Equal(t, tc.rest, got.rest)
		})
	}
}

func TestAdoptResumePassthrough(t *testing.T) {
	setArgs := func(t *testing.T, args ...string) {
		t.Helper()
		previous := os.Args
		os.Args = append([]string{"tclaude"}, args...)
		t.Cleanup(func() { os.Args = previous })
	}

	t.Run("continue becomes resume-last and leaves the pass-through", func(t *testing.T) {
		cwd := setupResumeSelector(t, nil)
		setArgs(t, "--", "--continue", "--model", "opus")

		params := &NewParams{Dir: cwd}
		require.NoError(t, adoptResumePassthrough(params))
		assert.True(t, params.ResumeLast)
		assert.Equal(t, []string{"tclaude", "--", "--model", "opus"}, os.Args)
	})

	t.Run("a known id moves onto the resume path", func(t *testing.T) {
		cwd := setupResumeSelector(t, nil)
		seedResumeSelectorConv(t, resumeSelectorConv, cwd)
		setArgs(t, "--", "--resume", resumeSelectorConv[:8])

		params := &NewParams{Dir: cwd}
		require.NoError(t, adoptResumePassthrough(params))
		assert.Equal(t, resumeSelectorConv[:8], params.Resume)
		assert.Equal(t, []string{"tclaude", "--"}, os.Args)
	})

	t.Run("an unknown selector stays with claude", func(t *testing.T) {
		cwd := setupResumeSelector(t, nil)
		setArgs(t, "--", "--resume", "search term")

		params := &NewParams{Dir: cwd}
		require.NoError(t, adoptResumePassthrough(params))
		assert.Empty(t, params.Resume)
		assert.Equal(t, []string{"tclaude", "--", "--resume", "search term"}, os.Args)
	})

	t.Run("another harness keeps its own flags", func(t *testing.T) {
		cwd := setupResumeSelector(t, nil)
		setArgs(t, "--", "--continue")

		params := &NewParams{Dir: cwd, Harness: "pi"}
		require.NoError(t, adoptResumePassthrough(params))
		assert.False(t, params.ResumeLast)
		assert.Equal(t, []string{"tclaude", "--", "--continue"}, os.Args)
	})
}

func TestResolveResumeLast(t *testing.T) {
	t.Run("no conversation is an error", func(t *testing.T) {
		cwd := setupResumeSelector(t, nil)
		err := resolveResumeLast(&NewParams{ResumeLast: true, Dir: cwd})
		require.ErrorContains(t, err, "no conversation to resume")
	})
	for name, params := range map[string]*NewParams{
		"resume":     {ResumeLast: true, Resume: "abc"},
		"session id": {ResumeLast: true, SessionID: "abc"},
		"global":     {ResumeLast: true, Global: true},
		"harness":    {ResumeLast: true, Harness: "codex"},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			require.Error(t, resolveResumeLast(params))
		})
	}
}
