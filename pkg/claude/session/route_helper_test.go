//go:build linux

package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testutil"
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
	proxyBridgeConstructedRootFixture(t)

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
	if strings.Count(rendered, "--preserve-fds") != 1 {
		t.Fatalf("descriptor preservation belongs only to the tclaude relay, not bubblewrap: %s", rendered)
	}
	t.Run("installed bubblewrap accepts the rendered options", func(t *testing.T) {
		bwrapBinary, err := exec.LookPath("bwrap")
		if err != nil {
			t.Skip("bubblewrap is not installed")
		}
		command, err := WrapTclaudeLayerSpec(bwrapBinary, spec, "exec harness")
		if err != nil {
			t.Fatal(err)
		}
		start := strings.Index(command, clcommon.ShellQuoteArg(bwrapBinary)+" ")
		if start < 0 {
			t.Fatal("rendered command has no bubblewrap invocation")
		}
		options := command[start:]
		end := strings.Index(options, sandboxExecShellPrefix())
		if end < 0 {
			t.Fatal("rendered command has no sandbox shell separator")
		}
		// Parse all options before --help. The help action exits before
		// namespace creation, so this also runs inside a restricted sandbox.
		output, err := exec.Command("/bin/sh", "-c", options[:end]+" --help").CombinedOutput()
		if err != nil {
			t.Fatalf("bubblewrap rejected rendered launch options: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "usage:") {
			t.Fatalf("expected bubblewrap help, got %s", output)
		}
	})
	if strings.Contains(rendered, "--credential-fifo") || strings.Contains(rendered, "--credential-path") {
		t.Fatalf("route helper render retained pathname credential contract: %s", rendered)
	}
	if strings.Contains(rendered, "--ro-bind "+helper.HandoffSocketPath) {
		t.Fatalf("route helper handoff endpoint was mounted into the sandbox: %s", rendered)
	}
}

func TestRouteHelperHostDiscoverySelector(t *testing.T) {
	helper := testRouteHelper()
	helper.GroupIDs = []int64{0}
	effective := sandboxpolicy.EffectiveProfile{NetworkAccess: sandboxpolicy.NetworkAccessNone}
	if err := validateTclaudeLayerRouteHelper(effective, helper); err != nil {
		t.Fatal(err)
	}
	wrapped, err := wrapTclaudeLayerRouteHelper("/usr/local/bin/tclaude", *helper, "harness --run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wrapped, "--group-id 0") {
		t.Fatalf("missing host discovery selector: %s", wrapped)
	}
}

func TestRouteHelperAcceptsLateConversation(t *testing.T) {
	helper := testRouteHelper()
	helper.ConvID = ""
	effective := sandboxpolicy.EffectiveProfile{NetworkAccess: sandboxpolicy.NetworkAccessNone}
	if err := validateTclaudeLayerRouteHelper(effective, helper); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapTclaudeLayerRouteHelper(helper.BinaryPath, *helper, "harness --run"); err != nil {
		t.Fatal(err)
	}
}

// This subprocess entry runs the production helper command/parser rather than
// a script that ignores its arguments. The wrapper owns its lifetime.
func TestRouteHelperCommandProcess(t *testing.T) {
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		t.Skip("helper subprocess only")
	}
	args := os.Args[separator+1:]
	if len(args) < 2 || args[0] != "session" || args[1] != tclaudeLayerRouteHelperCommand {
		t.Fatal("unexpected helper command prefix")
	}
	command := tclaudeLayerRouteHelperCmd()
	command.SetArgs(args[2:])
	command.SetOut(os.Stdout)
	command.SetErr(os.Stderr)
	if err := command.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestRouteHelperLateConversationStartsHarness(t *testing.T) {
	root := testutil.CanonicalTempDir(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, "helper.sh")
	script := "#!/bin/sh\nexec " + clcommon.ShellQuoteArg(binary) + " -test.run=^TestRouteHelperCommandProcess$ -- \"$@\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	helper := *testRouteHelper()
	helper.BinaryPath = launcher
	helper.ConvID = ""
	helper.GroupIDs = []int64{0}
	helper.SocketPath = filepath.Join(root, "not-yet-enrolled.sock")
	wrapped, err := wrapTclaudeLayerRouteHelper(binary, helper, "printf harness-started; test ! -e /proc/self/fd/4")
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
	_, err = credentialWrite.WriteString("launch-scoped-test-credential")
	if err != nil {
		t.Fatal(err)
	}
	if err := credentialWrite.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", wrapped)
	command.ExtraFiles = []*os.File{dummy, credentialRead}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("late-enrolled helper prevented harness startup: %v\n%s", err, output)
	}
	if string(output) != "harness-started" {
		t.Fatalf("harness did not start: %q", output)
	}
}
