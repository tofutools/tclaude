//go:build rewire

package agentd_test

import (
	"testing"
	"time"
)

// TestSpawn_RenamesAndResumes pins the canonical spawn lifecycle:
// human spawns into a group, the post-init goroutine fires
// `/rename <alias>` into the new pane, and a subsequent resume on
// an offline conv actually shells out a fresh `tclaude session new -r`
// (rather than silently reporting "skipped:already_online" against
// a stale row — the bug class the testing-strategy doc calls out).
func TestSpawn_RenamesAndResumes(t *testing.T) {
	f := newFlow(t)

	// Given: an empty group called "alpha".
	f.HaveGroup("alpha")

	// When: the human spawns a worker.
	spawn := f.AsHuman().Spawn("alpha", "worker")

	// Then: the post-init goroutine injects /rename worker. Up to
	// ~2.5s in production timing (waitForConvAlive's poll + ready
	// delay + injectTextAndSubmit's two paste-mode pauses); 5s
	// gives slack.
	f.AssertSentContains(spawn.TmuxTarget(), "/rename worker", 5*time.Second)

	// When: the conv goes offline (simulating a CC crash / human
	// closing the pane), and the human resumes it.
	f.MarkOffline(spawn.TmuxSession)
	resume := f.AsHuman().Resume(spawn.ConvID)

	// Then: the resume actually exercised the spawn path.
	f.AssertResumeSpawned(resume)
}
