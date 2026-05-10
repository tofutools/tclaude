package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// bridgeAgentClientToMux routes agent.DaemonRequest through h (with
// AsHumanPeer auth) so a CLI invocation runs end-to-end against the
// real daemon mux. Restores via t.Cleanup.
//
// This is the only "wiring" the CLI flow tests introduce. CCSim and
// TmuxSim are the boundary mocks established by newFlow; everything
// inside the daemon — DB writes, permission gates, group/membership
// rules, request decoding — runs unchanged. The bridge stands in for
// the production Unix-socket transport so we don't need a live socket
// for tests; the bytes flowing through are still the same JSON the
// real client would have sent.
func bridgeAgentClientToMux(t *testing.T, h http.Handler) {
	t.Helper()

	prevAvail := agent.DaemonAvailableImpl
	agent.DaemonAvailableImpl = func() bool { return true }

	prevReq := agent.DaemonRequestImpl
	agent.DaemonRequestImpl = func(method, path string, in, out any, _ agent.DaemonOpts) error {
		var body []byte
		if in != nil {
			b, err := json.Marshal(in)
			if err != nil {
				return err
			}
			body = b
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		if in != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		r = agentd.AsHumanPeer(r)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code >= 400 {
			return &agent.DaemonError{Status: rr.Code, Raw: rr.Body.Bytes()}
		}
		if out != nil && rr.Body.Len() > 0 {
			return json.Unmarshal(rr.Body.Bytes(), out)
		}
		return nil
	}

	t.Cleanup(func() {
		agent.DaemonAvailableImpl = prevAvail
		agent.DaemonRequestImpl = prevReq
	})
}

// chdirTo chdirs to dir and restores on cleanup.
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

// resolveSym normalises a path for equality comparison. On macOS,
// /tmp is a symlink to /private/tmp, so t.TempDir() and os.Getwd()
// after chdir disagree by surface form even though they point at the
// same inode. Resolve both before comparing.
func resolveSym(t *testing.T, p string) string {
	t.Helper()
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return rp
}

// Scenario: a human runs `tclaude agent spawn alpha worker` from a
// project tree, no -C / --cwd given. The CLI must capture os.Getwd()
// and pass it through the daemon, so the new CC instance starts where
// the human was — not wherever agentd was launched from.
//
// Real surface assertion: the SessionRow handleGroupSpawn caused
// (via simSpawner.SpawnNew) records the cwd that flowed through the
// HTTP body. If the CLI defaulting regresses, body.Cwd is "" and the
// row's Cwd reflects the daemon's cwd instead of the caller's.
//
// Pins the bug class fixed by commit d7b13e6.
func TestSpawnCLI_DefaultsCwdToCallersCwd(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)

	callerCwd := resolveSym(t, t.TempDir())
	chdirTo(t, callerCwd)

	stdout := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Alias: "worker"},
		stdout, new(bytes.Buffer),
	)
	if rc != 0 {
		t.Fatalf("RunSpawn rc=%d stdout=%s", rc, stdout.String())
	}
	if resp == nil || resp.ConvID == "" {
		t.Fatalf("RunSpawn returned empty response: %+v", resp)
	}

	rows, err := db.FindSessionsByConvID(resp.ConvID)
	if err != nil || len(rows) == 0 {
		t.Fatalf("no session row for conv %s: %v", resp.ConvID, err)
	}
	if got := resolveSym(t, rows[0].Cwd); got != callerCwd {
		t.Errorf("SessionRow.Cwd = %q, want %q (caller's cwd)", got, callerCwd)
	}
}

// Scenario: explicit `-C /some/path` must still override the auto-
// captured cwd. Regression guard against a future refactor that drops
// explicit-wins semantics.
func TestSpawnCLI_ExplicitCwdOverridesCallersCwd(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)

	callerCwd := resolveSym(t, t.TempDir())
	explicitCwd := resolveSym(t, t.TempDir())
	chdirTo(t, callerCwd)

	resp, rc := agent.RunSpawn(
		&agent.SpawnParams{Group: "alpha", Alias: "worker", Cwd: explicitCwd},
		new(bytes.Buffer), new(bytes.Buffer),
	)
	if rc != 0 || resp == nil {
		t.Fatalf("RunSpawn rc=%d resp=%v", rc, resp)
	}

	rows, err := db.FindSessionsByConvID(resp.ConvID)
	if err != nil || len(rows) == 0 {
		t.Fatalf("no session row for conv %s: %v", resp.ConvID, err)
	}
	if got := resolveSym(t, rows[0].Cwd); got != explicitCwd {
		t.Errorf("SessionRow.Cwd = %q, want %q (explicit -C override)", got, explicitCwd)
	}
}

// Scenario: `tclaude --join-group <group>` and `tclaude session new
// --join-group <group>` both route through RunJoinGroup. Same default-
// cwd semantics: caller's cwd flows into the spawn body when
// params.Dir is empty.
func TestJoinGroupCLI_DefaultsCwdToCallersCwd(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)

	callerCwd := resolveSym(t, t.TempDir())
	chdirTo(t, callerCwd)

	// Detached short-circuits before AttachToSession (which would
	// shell out to tmux for real).
	err := agent.RunJoinGroup(&session.NewParams{
		JoinGroup: "alpha",
		Detached:  true,
	})
	if err != nil {
		t.Fatalf("RunJoinGroup: %v", err)
	}

	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil || len(members) != 1 {
		t.Fatalf("expected 1 member in alpha, got %d (err=%v)", len(members), err)
	}
	rows, err := db.FindSessionsByConvID(members[0].ConvID)
	if err != nil || len(rows) == 0 {
		t.Fatalf("no session row for conv %s: %v", members[0].ConvID, err)
	}
	if got := resolveSym(t, rows[0].Cwd); got != callerCwd {
		t.Errorf("SessionRow.Cwd = %q, want %q (caller's cwd)", got, callerCwd)
	}
}
