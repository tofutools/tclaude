package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// captureSpawnRequest sets up the daemon-client indirection so a CLI
// invocation routes through fake transport and we capture the body it
// would have POSTed. Returns a struct containing the captured request
// + a t.Cleanup hook that restores production transport.
type captured struct {
	mu     sync.Mutex
	method string
	path   string
	body   map[string]any
	hits   int
}

func (c *captured) get() (string, string, map[string]any, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.method, c.path, c.body, c.hits
}

func installFakeDaemon(t *testing.T) *captured {
	t.Helper()
	cap := &captured{}

	prevAvail := DaemonAvailableImpl
	DaemonAvailableImpl = func() bool { return true }

	prevReq := DaemonRequestImpl
	DaemonRequestImpl = func(method, path string, in, _ any, _ DaemonOpts) error {
		cap.mu.Lock()
		cap.method = method
		cap.path = path
		if m, ok := in.(map[string]any); ok {
			cap.body = m
		}
		cap.hits++
		cap.mu.Unlock()
		// Leave out untouched — runSpawn/runJoinGroup tolerate empty
		// response fields (they just skip optional pretty-print lines).
		return nil
	}

	t.Cleanup(func() {
		DaemonAvailableImpl = prevAvail
		DaemonRequestImpl = prevReq
	})
	return cap
}

// chdirTo chdirs into dir for the test and restores on cleanup. Used to
// pin a known os.Getwd() result.
func chdirTo(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %q: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// resolveSym mirrors what os.Getwd() returns on macOS where /tmp is a
// symlink to /private/tmp; t.TempDir() reports the symlinked-to path.
// The test asserts equality after resolving both sides.
func resolveSym(t *testing.T, p string) string {
	t.Helper()
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return rp
}

// Scenario: human runs `tclaude agent spawn alpha worker` from a
// project tree, no -C / --cwd specified. The CLI must capture
// os.Getwd() and pass it to the daemon so the new CC instance starts
// where the human was.
//
// Pins the bug class: pre-fix, the daemon received cwd="" and the new
// session inherited the daemon's cwd (almost never what the human
// wanted).
func TestSpawn_DefaultsCwdToCallersCwd(t *testing.T) {
	tmp := resolveSym(t, t.TempDir())
	chdirTo(t, tmp)

	cap := installFakeDaemon(t)

	rc := runSpawn(&spawnParams{Group: "alpha", Alias: "worker"},
		new(bytes.Buffer), new(bytes.Buffer))
	if rc != rcOK {
		t.Fatalf("runSpawn rc = %d, want %d", rc, rcOK)
	}

	_, path, body, hits := cap.get()
	if hits != 1 {
		t.Fatalf("daemon hits = %d, want 1", hits)
	}
	if path != "/v1/groups/alpha/spawn" {
		t.Errorf("path = %q, want /v1/groups/alpha/spawn", path)
	}
	gotCwd, _ := body["cwd"].(string)
	if resolveSym(t, gotCwd) != tmp {
		t.Errorf("body.cwd = %q, want %q (caller's cwd)", gotCwd, tmp)
	}
}

// Scenario: explicit `-C /some/path` must still override the auto-
// captured cwd. Regression guard against a future refactor that drops
// the explicit-wins semantics.
func TestSpawn_ExplicitCwdOverridesCallersCwd(t *testing.T) {
	tmp := resolveSym(t, t.TempDir())
	chdirTo(t, tmp)

	cap := installFakeDaemon(t)

	const explicit = "/explicit/override"
	rc := runSpawn(&spawnParams{Group: "alpha", Alias: "worker", Cwd: explicit},
		new(bytes.Buffer), new(bytes.Buffer))
	if rc != rcOK {
		t.Fatalf("runSpawn rc = %d, want %d", rc, rcOK)
	}

	_, _, body, _ := cap.get()
	gotCwd, _ := body["cwd"].(string)
	if gotCwd != explicit {
		t.Errorf("body.cwd = %q, want %q (explicit -C override)", gotCwd, explicit)
	}
}

// Scenario: `tclaude --join-group <group>` and
// `tclaude session new --join-group <group>` route through
// runJoinGroup. Same default-cwd behavior must apply: caller's cwd
// flows into the spawn body when params.Dir is empty.
func TestJoinGroup_DefaultsCwdToCallersCwd(t *testing.T) {
	tmp := resolveSym(t, t.TempDir())
	chdirTo(t, tmp)

	cap := installFakeDaemon(t)

	// Detached so runJoinGroup short-circuits before AttachToSession
	// (which would shell out to tmux).
	err := runJoinGroup(&session.NewParams{JoinGroup: "alpha", Detached: true})
	if err != nil {
		t.Fatalf("runJoinGroup: %v", err)
	}

	_, path, body, hits := cap.get()
	if hits != 1 {
		t.Fatalf("daemon hits = %d, want 1", hits)
	}
	if path != "/v1/groups/alpha/spawn" {
		t.Errorf("path = %q, want /v1/groups/alpha/spawn", path)
	}
	gotCwd, _ := body["cwd"].(string)
	if resolveSym(t, gotCwd) != tmp {
		t.Errorf("body.cwd = %q, want %q (caller's cwd)", gotCwd, tmp)
	}
}

// Explicit -C on `--join-group` keeps overriding.
func TestJoinGroup_ExplicitCwdOverridesCallersCwd(t *testing.T) {
	tmp := resolveSym(t, t.TempDir())
	chdirTo(t, tmp)

	cap := installFakeDaemon(t)

	const explicit = "/explicit/override"
	err := runJoinGroup(&session.NewParams{
		JoinGroup: "alpha",
		Detached:  true,
		Dir:       explicit,
	})
	if err != nil {
		t.Fatalf("runJoinGroup: %v", err)
	}

	_, _, body, _ := cap.get()
	gotCwd, _ := body["cwd"].(string)
	if gotCwd != explicit {
		t.Errorf("body.cwd = %q, want %q (explicit -C override)", gotCwd, explicit)
	}
}
