package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// seanceFixture materialises a conv_index row for convID with a real
// (existing) ProjectPath as its launch dir and a placeholder .jsonl, so
// ResolveLocation reports a cwd and FreshConvRowResolved returns the
// cached row without a disk rescan evicting it.
func seanceFixture(t *testing.T, convID, harnessName string) (cwd string) {
	t.Helper()
	cwd = t.TempDir()
	dir := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(dir, 0o755), "mkdir")
	fullPath := filepath.Join(dir, convID+".jsonl")
	require.NoError(t, os.WriteFile(fullPath, []byte(""), 0o600), "write fixture")
	mtime := time.Now().Unix()
	require.NoError(t, os.Chtimes(fullPath, time.Unix(mtime, 0), time.Unix(mtime, 0)), "chtimes")
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  dir,
		ProjectPath: cwd,
		FullPath:    fullPath,
		FileMtime:   mtime,
		CustomTitle: "departed",
		Harness:     harnessName,
		IndexedAt:   time.Now(),
	}), "UpsertConvIndex")
	return cwd
}

// --target addresses a SPECIFIC dead generation and must NOT redirect
// forward to the live successor (the usual selector behaviour) — else a
// séance would have the agent consult itself.
func TestSeance_TargetDoesNotRedirectForward(t *testing.T) {
	setupTestDB(t)
	const old = "aaaaaaaa-1111-1111-1111-111111111111"
	const cur = "bbbbbbbb-2222-2222-2222-222222222222"
	seanceFixture(t, old, "claude")
	seanceFixture(t, cur, "claude")
	require.NoError(t, db.RecordConvSuccession(old, cur, "reincarnate"), "edge old->cur")

	var stderr bytes.Buffer
	target, rc := resolveSeanceTarget(&seanceParams{Target: "aaaaaaaa"}, &stderr)
	require.Equal(t, rcOK, rc, "rc; stderr=%s", stderr.String())
	assert.Equal(t, old, target, "must stay on the addressed generation, not redirect to the successor")
}

// With no --target, the séance targets the caller's own predecessor.
func TestSeance_DefaultResolvesPredecessor(t *testing.T) {
	setupTestDB(t)
	const old = "cccccccc-1111-1111-1111-111111111111"
	const me = "dddddddd-2222-2222-2222-222222222222"
	require.NoError(t, db.RecordConvSuccession(old, me, "reincarnate"), "edge old->me")
	t.Setenv("TCLAUDE_SESSION_ID", me)

	var stderr bytes.Buffer
	target, rc := resolveSeanceTarget(&seanceParams{Back: 1}, &stderr)
	require.Equal(t, rcOK, rc, "rc; stderr=%s", stderr.String())
	assert.Equal(t, old, target, "default targets the immediate predecessor")
}

// A first-generation agent (never reincarnated) has no one to consult.
func TestSeance_NoPredecessorIsAClearError(t *testing.T) {
	setupTestDB(t)
	t.Setenv("TCLAUDE_SESSION_ID", "eeeeeeee-2222-2222-2222-222222222222")
	var stderr bytes.Buffer
	_, rc := resolveSeanceTarget(&seanceParams{Back: 1}, &stderr)
	assert.Equal(t, rcNotFound, rc, "rc")
	assert.Contains(t, stderr.String(), "no predecessor", "explains the empty grave")
}

// --print-cmd resolves everything and prints the resume command + cwd
// without running anything (the free, no-cost targeting check).
func TestSeance_PrintCmd_BuildsHeadlessResumeArgv(t *testing.T) {
	setupTestDB(t)
	const dead = "ffffffff-1111-1111-1111-111111111111"
	cwd := seanceFixture(t, dead, "claude")

	var stdout, stderr bytes.Buffer
	rc := runSeance(&seanceParams{
		Question: "what was the auth bug you were chasing?",
		Target:   dead,
		PrintCmd: true,
	}, strings.NewReader(""), &stdout, &stderr)
	require.Equal(t, rcOK, rc, "rc; stderr=%s", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "--resume "+dead, "resumes the dead conv")
	assert.Contains(t, out, "-p", "headless print mode")
	assert.Contains(t, out, "cwd:         "+cwd, "resumes from the predecessor's launch dir")
	assert.Contains(t, out, "what was the auth bug", "carries the question")
}

// The actual-run path builds the right plan and hands the captured
// answer back to the caller's stdout — verified through the swappable
// seanceRun boundary so no real harness is spawned.
func TestSeance_Run_InvokesRunnerWithResumePlan(t *testing.T) {
	setupTestDB(t)
	const dead = "99999999-1111-1111-1111-111111111111"
	cwd := seanceFixture(t, dead, "claude")

	var captured seancePlan
	prev := seanceRun
	seanceRun = func(p seancePlan) error {
		captured = p
		_, _ = p.Stdout.Write([]byte("It was a nil session token on resume.\n"))
		return nil
	}
	t.Cleanup(func() { seanceRun = prev })

	var stdout, stderr bytes.Buffer
	rc := runSeance(&seanceParams{
		Question: "what did you learn?",
		Target:   dead,
		Timeout:  "30s",
	}, strings.NewReader(""), &stdout, &stderr)
	require.Equal(t, rcOK, rc, "rc; stderr=%s", stderr.String())

	assert.Equal(t, cwd, captured.Cwd, "runs in the predecessor's launch dir")
	assert.Equal(t, 30*time.Second, captured.Timeout, "honours --timeout")
	assert.Contains(t, strings.Join(captured.Argv, " "), "--resume "+dead, "resume argv")
	assert.Contains(t, stdout.String(), "nil session token", "answer reaches the successor")
}

// An unparseable --timeout is rejected before any summoning happens.
func TestSeance_RejectsBadTimeout(t *testing.T) {
	setupTestDB(t)
	var stdout, stderr bytes.Buffer
	rc := runSeance(&seanceParams{
		Question: "hi",
		Target:   "whatever",
		Timeout:  "soon",
	}, strings.NewReader(""), &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc, "rc")
	assert.Contains(t, stderr.String(), "invalid --timeout", "explains why")
}
