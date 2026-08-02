//go:build linux

package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func testRouteHelper() *TclaudeLayerRouteHelper {
	return &TclaudeLayerRouteHelper{
		BinaryPath:        "/usr/local/bin/tclaude",
		SocketPath:        "/home/test/.tclaude/api/agentd.sock",
		AgentID:           "agt_test",
		ConvID:            "conv_test",
		LaunchGeneration:  "launch_test",
		HandoffSocketPath: "/home/test/.tclaude/data/route-helper-handoff.sock",
		CredentialFD:      4,
		GroupIDs:          []int64{42},
	}
}

func TestRouteHelperRefusesHostOpenPosture(t *testing.T) {
	err := validateTclaudeLayerRouteHelper(sandboxpolicy.EffectiveProfile{}, testRouteHelper())
	if err == nil || !strings.Contains(err.Error(), "host-open") {
		t.Fatalf("route helper host-open error = %v", err)
	}
}

func TestRouteHelperWrapsHarnessWithCleanupTrap(t *testing.T) {
	effective := sandboxpolicy.EffectiveProfile{NetworkAccess: sandboxpolicy.NetworkAccessNone}
	if err := validateTclaudeLayerRouteHelper(effective, testRouteHelper()); err != nil {
		t.Fatal(err)
	}
	wrapped, err := wrapTclaudeLayerRouteHelper("/usr/local/bin/tclaude", *testRouteHelper(), "harness --run")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/usr/local/bin/tclaude session",
		"tclaude-layer-route-helper",
		"--group-id 42",
		"route_helper_pid=$!",
		"trap route_helper_cleanup EXIT",
		"harness --run; route_helper_status=$?; exit $route_helper_status",
	} {
		if !strings.Contains(wrapped, want) {
			t.Fatalf("wrapped command missing %q: %s", want, wrapped)
		}
	}
	if strings.Contains(wrapped, "credential_test") {
		t.Fatalf("wrapped command leaked the bearer credential: %s", wrapped)
	}
	if !strings.Contains(wrapped, "--credential-fd 4") {
		t.Fatalf("wrapped command did not carry the inherited credential FD: %s", wrapped)
	}
	if strings.Contains(wrapped, "credential-fifo") || strings.Contains(wrapped, "handoff.sock") {
		t.Fatalf("wrapped helper command exposed a credential path: %s", wrapped)
	}
	if !strings.Contains(wrapped, "exec 4<&-") {
		t.Fatalf("wrapped command did not close the credential FD before harness start: %s", wrapped)
	}
	if strings.Index(wrapped, "IFS= read -r route_helper_ready") > strings.Index(wrapped, "harness --run") {
		t.Fatalf("harness starts before the helper consumes its credential: %s", wrapped)
	}
}

func TestRouteHelperWrapperClosesCredentialFDBeforeHarness(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ncat <&4 >/dev/null\nprintf 'ready\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	routeHelper := *testRouteHelper()
	routeHelper.BinaryPath = helper
	wrapped, err := wrapTclaudeLayerRouteHelper("/usr/local/bin/tclaude", routeHelper,
		"test ! -e /proc/self/fd/4")
	if err != nil {
		t.Fatal(err)
	}
	dummy, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer dummy.Close()
	credentialRead, credentialWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer credentialRead.Close()
	cmd := exec.Command("/bin/sh", "-c", wrapped)
	cmd.ExtraFiles = []*os.File{dummy, credentialRead}
	if err := cmd.Start(); err != nil {
		credentialWrite.Close()
		t.Fatal(err)
	}
	_, _ = credentialWrite.WriteString("descriptor-only-test")
	_ = credentialWrite.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wrapper/harness topology failed: %v", err)
	}
}

func TestRouteHelperRenderCarriesOnlyPreservedFDIntoSandbox(t *testing.T) {
	helper := *testRouteHelper()
	helper.BinaryPath, _ = os.Executable()
	effective := sandboxpolicy.EffectiveProfile{NetworkAccess: sandboxpolicy.NetworkAccessNone}
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: "claude",
		Cwd:         t.TempDir(),
		StateRoot:   t.TempDir(),
		Snapshot:    &snapshot,
		RouteHelper: &helper,
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := WrapTclaudeLayerSpec(helper.BinaryPath, spec, "exec harness")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tclaude-layer-route-helper-bootstrap",
		"--preserve-fds 1",
		"--credential-fd 4",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("route helper render missing %q: %s", want, rendered)
		}
	}
	if strings.Contains(rendered, "--credential-fifo") || strings.Contains(rendered, "--credential-path") {
		t.Fatalf("route helper render retained pathname credential contract: %s", rendered)
	}
	if strings.Contains(rendered, "--ro-bind "+helper.HandoffSocketPath) {
		t.Fatalf("route helper handoff endpoint was mounted into the sandbox: %s", rendered)
	}
}
