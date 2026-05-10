package agentd

// Spawner abstracts `tclaude session new` invocations so flow tests
// can substitute a behavior-accurate simulator for the real
// fork-exec subprocess. Production wires LiveSpawner; tests assign
// a fake to Spawn at setup with t.Cleanup-restoration.
//
// This is the interface-based alternative to rewire's compile-time
// mocking: pure Go, no toolchain dependency, no build tag, runs
// under bare `go test`. The trade-off is a small surface change at
// the call site: `SpawnDetachedTclaudeNew(...)` now delegates to
// `Spawn.SpawnNew(...)`.
type Spawner interface {
	SpawnNew(label, cwd string) error
	SpawnResume(convID, cwd string) error
}

// Spawn is the package-wide Spawner every caller hits via the
// SpawnDetachedTclaude{New,Resume} facades. Production starts on
// LiveSpawner; tests overwrite during their setup. Single global var =
// goroutine-unsafe across parallel tests on the same package — same
// constraint the rewire approach has.
var Spawn Spawner = LiveSpawner{}

// LiveSpawner is the production impl: forks `tclaude session new -d`
// for spawn, `-r <conv> -d` for resume. Exported so tests can wrap
// it (e.g., a recording proxy).
type LiveSpawner struct{}

func (LiveSpawner) SpawnNew(label, cwd string) error    { return liveSpawnNew(label, cwd) }
func (LiveSpawner) SpawnResume(convID, cwd string) error { return liveSpawnResume(convID, cwd) }
