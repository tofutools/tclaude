//go:build linux

package session

import (
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func testRouteHelper() *TclaudeLayerRouteHelper {
	return &TclaudeLayerRouteHelper{
		BinaryPath:       "/usr/local/bin/tclaude",
		SocketPath:       "/home/test/.tclaude/api/agentd.sock",
		AgentID:          "agt_test",
		ConvID:           "conv_test",
		LaunchGeneration: "launch_test",
		CredentialPath:   "/home/test/.tclaude/api/route-helper-credential.fifo",
		GroupIDs:         []int64{42},
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
	if !strings.Contains(wrapped, "--credential-fifo /home/test/.tclaude/api/route-helper-credential.fifo") {
		t.Fatalf("wrapped command did not carry the non-secret FIFO path: %s", wrapped)
	}
	if strings.Index(wrapped, "IFS= read -r route_helper_ready") > strings.Index(wrapped, "harness --run") {
		t.Fatalf("harness starts before the helper consumes its credential: %s", wrapped)
	}
}
