package agentd

// Spawner abstracts `tclaude session new` invocations so flow tests
// can substitute a behavior-accurate simulator for the real
// fork-exec subprocess. Production wires LiveSpawner; tests assign
// a fake to Spawn at setup with t.Cleanup-restoration.
//
// harness is the harness name to launch (e.g. "claude", "codex"); "" or
// "claude" omits the --harness flag and spawns Claude Code. It threads
// through to `tclaude session new --harness <h>` so a daemon-owned Codex
// agent comes up under the Codex CLI (JOH-160).
//
// sandbox is the launch-time OS-sandbox mode for harnesses that take one
// (Codex's --sandbox); "" omits the flag. The daemon resolves it to the
// harness's secure default (Codex: workspace-write) before spawning, so a
// daemon-owned Codex agent runs sandboxed by default (JOH-192).
type Spawner interface {
	SpawnNew(label, cwd, effort, model, harness, sandbox string) error
	SpawnResume(convID, cwd, effort, model, harness, sandbox string) error
}

// Spawn is the package-wide Spawner every caller hits via the
// SpawnDetachedTclaude{New,Resume} facades. Production starts on
// LiveSpawner; tests overwrite during their setup. Single global var =
// goroutine-unsafe across parallel tests on the same package — flow
// tests don't t.Parallel.
var Spawn Spawner = LiveSpawner{}

// LiveSpawner is the production impl: forks `tclaude session new -d`
// for spawn, `-r <conv> -d` for resume. Exported so tests can wrap
// it (e.g., a recording proxy).
type LiveSpawner struct{}

func (LiveSpawner) SpawnNew(label, cwd, effort, model, harness, sandbox string) error {
	return liveSpawnNew(label, cwd, effort, model, harness, sandbox)
}
func (LiveSpawner) SpawnResume(convID, cwd, effort, model, harness, sandbox string) error {
	return liveSpawnResume(convID, cwd, effort, model, harness, sandbox)
}
