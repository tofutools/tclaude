package session

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Golden pins for the rendered outer-layer commands: every variant of the
// layer and server command renderers, with every input non-empty, on each
// network posture the plan can carry. A refactor of the renderers must leave
// each pin byte-identical. Regenerate with TCLAUDE_UPDATE_LAYER_PINS=1 only
// for an intended change to a rendered boundary.

const layerPinsUpdateEnv = "TCLAUDE_UPDATE_LAYER_PINS"

var (
	hostUsrSymlink      = regexp.MustCompile(` --symlink usr/\S+ /\S+`)
	darwinLaunchPayload = regexp.MustCompile(`--launch [A-Za-z0-9_-]+`)
)

// layerPinInput is one launch's render input, with fixed extras on top of what
// the spec derived so every slot of the renderer carries something.
type layerPinInput struct {
	name string
	in   tclaudeLayerRenderInput
}

// layerPinEnv is the pins' isolated environment: a short root that holds
// HOME, TMUX_TMPDIR and an extra unix socket, and a normaliser that replaces
// this run's paths with stable placeholders.
type layerPinEnv struct {
	root      string
	socket    string
	normalise func(string) string
}

func newLayerPinEnv(t *testing.T) layerPinEnv {
	t.Helper()
	// The root is short and directly under /tmp: a unix socket path has a
	// short length limit, and Seatbelt renders every ancestor of a protected
	// path, so a test temp dir's runner-specific parents would leak in.
	root, err := os.MkdirTemp("/tmp", "lpin")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	// macOS's /tmp is a symlink; the renderer names the canonical path.
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	// Seatbelt renders the runtime TMPDIR, and carves it only under the
	// standard per-user runtime tree.
	tmpdir := filepath.Join(root, "tmp")
	if runtime.GOOS == "darwin" {
		tmpdir = "/private/var/folders/ab/runtime/T"
	} else {
		require.NoError(t, os.MkdirAll(tmpdir, 0o700))
	}
	t.Setenv("TMPDIR", tmpdir)
	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))
	t.Setenv("HOME", home)
	// The sandbox shell's PATH is rendered into the command.
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	tmux := filepath.Join(root, "tmux")
	require.NoError(t, os.MkdirAll(tmux, 0o700))
	t.Setenv("TMUX_TMPDIR", tmux)
	floor := sandboxpolicy.AgentdSocketFloor()
	require.NotEmpty(t, floor)
	require.NoError(t, os.MkdirAll(filepath.Dir(floor[0]), 0o700))
	require.NoError(t, os.WriteFile(floor[0], nil, 0o600))

	socket := filepath.Join(root, "pin.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	exe, _ := os.Executable()
	normalise := func(s string) string {
		if exe != "" {
			s = strings.ReplaceAll(s, exe, "$EXE")
		}
		s = strings.ReplaceAll(s, root, "$ROOT")
		// A constructed root mirrors the host's merged-/usr symlinks, which
		// differ between hosts (/lib32, /libx32, ...): host layout, not render.
		s = hostUsrSymlink.ReplaceAllString(s, "")
		// Per-user directories are named for the user.
		uid := strconv.Itoa(os.Getuid())
		s = strings.ReplaceAll(s, "tmux-"+uid, "tmux-$UID")
		return strings.ReplaceAll(s, "claude-"+uid, "claude-$UID")
	}
	// The Darwin proxy launcher carries its spec as base64 JSON; its paths
	// are normalised decoded, so the pin shows the spec it launches.
	normaliseLaunch := func(out string) string {
		return darwinLaunchPayload.ReplaceAllStringFunc(normalise(out), func(m string) string {
			data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(m, "--launch "))
			if err != nil {
				return m
			}
			return "--launch decoded(" + normalise(string(data)) + ")"
		})
	}
	return layerPinEnv{root: root, socket: socket, normalise: normaliseLaunch}
}

// spec builds a launch spec named name under the root, on snapshot's plan
// (host-open when nil), with mutate applied to the launch input.
func (e layerPinEnv) spec(
	t *testing.T, name string, snapshot *sandboxpolicy.Snapshot, mutate func(*TclaudeLayerLaunchInput),
) (TclaudeLayerLaunchSpec, error) {
	t.Helper()
	dir := filepath.Join(e.root, name)
	cwd, state := filepath.Join(dir, "cwd"), filepath.Join(dir, "state")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	require.NoError(t, os.MkdirAll(state, 0o700))
	if snapshot == nil {
		empty := sandboxpolicy.EmptySnapshot()
		snapshot = &empty
	}
	input := TclaudeLayerLaunchInput{HarnessName: "claude", Cwd: cwd, StateRoot: state, Snapshot: snapshot}
	if mutate != nil {
		mutate(&input)
	}
	return BuildTclaudeLayerLaunchSpec(input)
}

// layerPinFixture builds the renderer pin inputs, each with fixed extras on
// top of what its spec derived so every slot of the renderer carries
// something.
func layerPinFixture(t *testing.T) ([]layerPinInput, func(string) string) {
	t.Helper()
	env := newLayerPinEnv(t)
	extra := func(dir string) string {
		path := filepath.Join(env.root, "extra", dir)
		require.NoError(t, os.MkdirAll(path, 0o700))
		return path
	}
	var inputs []layerPinInput
	for _, c := range []struct {
		name     string
		snapshot *sandboxpolicy.Snapshot
	}{
		{"host-open", nil},
		{"packet", engineLaunchSnapshot(sandboxpolicy.NetworkEnginePacket)},
		{"proxy", engineLaunchSnapshot(sandboxpolicy.NetworkEngineProxy)},
	} {
		spec, err := env.spec(t, c.name, c.snapshot, nil)
		require.NoError(t, err, c.name)
		in, err := tclaudeLayerSpecRenderInput(spec)
		require.NoError(t, err, c.name)
		in.Phase0WriteDirs = append(in.Phase0WriteDirs, extra("phase0"))
		in.PrivateWriteDirs = append(in.PrivateWriteDirs, TclaudeLayerPrivateWriteDir{
			Parent: extra("private"), Current: extra("private/current"),
		})
		in.FinalHideDirs = append(in.FinalHideDirs, extra("hide"))
		in.ReadOnlyBinds = append(in.ReadOnlyBinds, TclaudeLayerReadOnlyBind{
			Source: extra("ro"), Target: extra("ro"),
		})
		in.SocketPaths = append(in.SocketPaths, env.socket)
		inputs = append(inputs, layerPinInput{name: c.name, in: in})
	}
	return inputs, env.normalise
}

// layerPinRender records a render's result, error included, as one pin line.
func layerPinRender(normalise func(string) string, out string, err error) string {
	if err != nil {
		return "error: " + normalise(err.Error())
	}
	return "ok: " + normalise(out)
}

// checkLayerPins compares the rendered pins with the golden file.
func checkLayerPins(t *testing.T, name string, pins map[string]string, order []string) {
	t.Helper()
	var b strings.Builder
	for _, key := range order {
		fmt.Fprintf(&b, "== %s\n%s\n", key, pins[key])
	}
	golden := filepath.Join("testdata", name+"_"+runtime.GOOS+".golden")
	if os.Getenv(layerPinsUpdateEnv) == "1" {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(golden, []byte(b.String()), 0o644))
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil || string(want) != b.String() {
		// A platform's golden is recorded where it runs (macOS on CI): the
		// render is logged whole, to be reviewed and committed as the golden.
		t.Logf("%s does not hold this render; its content would be:\n-----BEGIN GOLDEN-----\n%s-----END GOLDEN-----",
			golden, b.String())
	}
	require.NoError(t, err, "missing golden; regenerate with %s=1", layerPinsUpdateEnv)
	gotLines := strings.Split(b.String(), "\n")
	wantLines := strings.Split(string(want), "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		var got, exp string
		if i < len(gotLines) {
			got = gotLines[i]
		}
		if i < len(wantLines) {
			exp = wantLines[i]
		}
		require.Equalf(t, exp, got, "rendered layer command differs from %s at line %d", golden, i+1)
	}
}
