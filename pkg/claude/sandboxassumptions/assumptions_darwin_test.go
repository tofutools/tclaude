//go:build darwin

package sandboxassumptions

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/text/unicode/norm"
)

const (
	darwinSandboxExec         = "/usr/bin/sandbox-exec"
	darwinAssumptionHelperEnv = "TCLAUDE_SANDBOX_ASSUMPTION_HELPER"
)

func TestSeatbeltAssumptions(t *testing.T) {
	if os.Getenv(assumptionsGateEnv) != "1" {
		t.Skip("set TCLAUDE_SANDBOX_ASSUMPTIONS=1 on a macOS host with Seatbelt")
	}
	info, err := os.Stat(darwinSandboxExec)
	if err != nil {
		t.Fatalf("stat %s: %v", darwinSandboxExec, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Fatalf("%s is not an executable regular file", darwinSandboxExec)
	}

	runDarwinAssumption(t, "SandboxExecEnforcesParameterizedDeny",
		"session.probeDarwinSeatbeltCapability, session.resolveBwrapBinary (darwin), "+
			"session.tclaudeLayerCommand",
		assumeSandboxExecDeny)
	runDarwinAssumption(t, "SpecificAllowCanReopenBroadDeny",
		"session.renderSeatbeltProfile, session.compileSeatbeltDenyRegions, "+
			"session.appendSeatbeltDenyRule",
		assumeSeatbeltSpecificAllow)
	runDarwinAssumption(t, "FileReadDenyDoesNotBlockUnixConnect",
		"session.appendSeatbeltUnixConnectDenyRule",
		assumeFileReadDoesNotDenyUnixConnect)
	runDarwinAssumption(t, "OutboundExceptionAndBindDeny",
		"session.appendSeatbeltNetworkDenyExceptAgentd and "+
			"session.appendSeatbeltIsolatedNetworkRules",
		assumeOutboundExceptionAndBindDeny)
	runDarwinAssumption(t, "CaseAndNFCFollowFileIdentity",
		"session.seatbeltSamePath, session.seatbeltPathContains, "+
			"session.darwinSeatbeltLstatIdentity",
		assumeCaseAndNFCIdentity)
	runDarwinAssumption(t, "DefaultVolumeFoldsCaseAndNormalization",
		"sandboxpolicy.GuardContainsOrEqual, sandboxpolicy.CanonicalHostSpelling, "+
			"sandboxpolicy.volumeFoldsCase, harness.CodexSandboxCwdConflict",
		assumeDefaultVolumeFolds)
	runDarwinAssumption(t, "SymlinkPredicateChecksAliasAndTarget",
		"session.expandSeatbeltAliasRegions and session.canonicalSeatbeltOwnedPath",
		assumeSymlinkPredicateIdentity)
	runDarwinAssumption(t, "RuntimeWriteCarveoutsCanBeStrictlyRedenied",
		"session.appendSeatbeltDenyRule baseline carveouts and strict runtime policy denies",
		assumeRuntimeWriteCarveouts)
	runDarwinAssumption(t, "HiddenEntriesRemainEnumerable",
		"session.tclaudeLayerLaunchOSSandbox darwin disclosure and Seatbelt smoke assertions",
		assumeHiddenEntriesEnumerable)
}

func runDarwinAssumption(
	t *testing.T,
	name, reliance string,
	test func(*testing.T),
) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Logf("production reliance: %s", reliance)
		test(t)
	})
}

func assumeSandboxExecDeny(t *testing.T) {
	t.Helper()
	root := canonicalTempDir(t)
	probe := filepath.Join(root, "denied-write")
	profile := `(version 1)
(allow default)
(deny file-write* (literal (param "PROBE")))
`
	runSeatbeltHelper(t, profile, map[string]string{"PROBE": probe},
		"write-denied", map[string]string{"ASSUME_PATH": probe}, nil)
}

func assumeSeatbeltSpecificAllow(t *testing.T) {
	t.Helper()
	root := canonicalTempDir(t)
	path := filepath.Join(root, "allowed-later")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write dominance fixture: %v", err)
	}
	profile := `(version 1)
(allow default)
(deny file-read* (subpath (param "ROOT")))
(allow file-read* (literal (param "FILE")))
`
	runSeatbeltHelper(t, profile, map[string]string{"ROOT": root, "FILE": path},
		"read-allowed", map[string]string{"ASSUME_PATH": path}, nil)
}

func assumeFileReadDoesNotDenyUnixConnect(t *testing.T) {
	t.Helper()
	root := shortDarwinTempDir(t)
	socket := filepath.Join(root, "echo.sock")
	stop := startUnixEchoServer(t, socket)
	defer stop()

	fileOnly := `(version 1)
(allow default)
(deny file-read* (literal (param "SOCKET")))
`
	runSeatbeltHelper(t, fileOnly, map[string]string{"SOCKET": socket},
		"unix-roundtrip", map[string]string{"ASSUME_SOCKET": socket}, nil)

	fileAndConnect := `(version 1)
(allow default)
(deny file-read* (literal (param "SOCKET")))
(deny network-outbound
  (remote unix-socket (literal (param "SOCKET"))))
`
	runSeatbeltHelper(t, fileAndConnect, map[string]string{"SOCKET": socket},
		"unix-connect-denied", map[string]string{"ASSUME_SOCKET": socket}, nil)
}

func assumeOutboundExceptionAndBindDeny(t *testing.T) {
	t.Helper()
	root := shortDarwinTempDir(t)
	allowed := filepath.Join(root, "allowed.sock")
	blocked := filepath.Join(root, "blocked.sock")
	listenPath := filepath.Join(root, "listener.sock")
	stopAllowed := startUnixEchoServer(t, allowed)
	defer stopAllowed()
	stopBlocked := startUnixEchoServer(t, blocked)
	defer stopBlocked()

	profile := `(version 1)
(allow default)
(deny network-bind)
(deny network-outbound
  (require-not
    (remote unix-socket (literal (param "ALLOWED")))))
`
	params := map[string]string{"ALLOWED": allowed}
	runSeatbeltHelper(t, profile, params,
		"unix-roundtrip", map[string]string{"ASSUME_SOCKET": allowed}, nil)
	runSeatbeltHelper(t, profile, params,
		"unix-connect-denied", map[string]string{"ASSUME_SOCKET": blocked}, nil)
	runSeatbeltHelper(t, profile, params,
		"unix-listen-denied", map[string]string{"ASSUME_SOCKET": listenPath}, nil)
}

func assumeCaseAndNFCIdentity(t *testing.T) {
	t.Helper()
	root := canonicalTempDir(t)
	assertSeatbeltSpellingIdentity(
		t,
		filepath.Join(root, "CaseProbe"),
		filepath.Join(root, "caseprobe"),
	)
	assertSeatbeltSpellingIdentity(
		t,
		filepath.Join(root, norm.NFD.String("é-probe")),
		filepath.Join(root, norm.NFC.String("é-probe")),
	)
}

func assertSeatbeltSpellingIdentity(t *testing.T, canonical, candidate string) {
	t.Helper()
	if err := os.WriteFile(canonical, []byte("canonical"), 0o600); err != nil {
		t.Fatalf("write canonical spelling %q: %v", canonical, err)
	}
	canonicalInfo, err := os.Lstat(canonical)
	if err != nil {
		t.Fatalf("lstat canonical spelling %q: %v", canonical, err)
	}
	candidateInfo, candidateErr := os.Lstat(candidate)
	sameIdentity := candidateErr == nil && os.SameFile(canonicalInfo, candidateInfo)
	if !sameIdentity && !samePathBytes(canonical, candidate) {
		if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
			t.Fatalf("write distinct candidate spelling %q: %v", candidate, err)
		}
	}
	profile := `(version 1)
(allow default)
(deny file-read* (literal (param "DENY")))
`
	mode := "read-allowed"
	if sameIdentity || samePathBytes(canonical, candidate) {
		mode = "read-denied"
	}
	runSeatbeltHelper(t, profile, map[string]string{"DENY": candidate},
		mode, map[string]string{"ASSUME_PATH": canonical}, nil)
	t.Logf("spelling identity: canonical=%q candidate=%q same_file=%t",
		canonical, candidate, sameIdentity)
}

func assumeSymlinkPredicateIdentity(t *testing.T) {
	t.Helper()
	root := canonicalTempDir(t)
	target := filepath.Join(root, "target")
	alias := filepath.Join(root, "alias")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatalf("create symlink alias: %v", err)
	}
	profile := `(version 1)
(allow default)
(deny file-read* (literal (param "DENY")))
`
	// Seatbelt checks the path operation under both the symlink spelling and
	// its resolved target. Production expands both identities so neither the
	// alias nor direct-target spelling can bypass policy.
	runSeatbeltHelper(t, profile, map[string]string{"DENY": alias},
		"read-denied", map[string]string{"ASSUME_PATH": alias}, nil)
	runSeatbeltHelper(t, profile, map[string]string{"DENY": alias},
		"read-allowed", map[string]string{"ASSUME_PATH": target}, nil)
	runSeatbeltHelper(t, profile, map[string]string{"DENY": target},
		"read-denied", map[string]string{"ASSUME_PATH": alias}, nil)
}

func assumeRuntimeWriteCarveouts(t *testing.T) {
	t.Helper()
	runtimeTemp := canonicalTempDir(t)
	inherited, err := os.Create(filepath.Join(runtimeTemp, "inherited"))
	if err != nil {
		t.Fatalf("create inherited fd fixture: %v", err)
	}
	defer func() { _ = inherited.Close() }()
	profile := `(version 1)
(allow default)
(deny file-write*
  (require-all
    (require-not (literal "/dev/null"))
    (require-not (literal "/dev/ptmx"))
    (require-not (literal "/dev/fd"))
    (require-not (subpath "/dev/fd"))
    (require-not (regex #"^/dev/(tty|pty)[A-Za-z0-9]+$"))
    (require-not (literal (param "TMPDIR")))
    (require-not (subpath (param "TMPDIR")))))
`
	runSeatbeltHelper(t, profile, map[string]string{"TMPDIR": runtimeTemp},
		"runtime-writes-allowed",
		map[string]string{
			"ASSUME_RUNTIME_TMP":  runtimeTemp,
			"ASSUME_INHERITED_FD": "3",
		},
		[]*os.File{inherited},
	)

	strict := profile + `
(deny file-write* (subpath (param "TMPDIR")))
`
	runSeatbeltHelper(t, strict, map[string]string{"TMPDIR": runtimeTemp},
		"runtime-temp-denied",
		map[string]string{"ASSUME_RUNTIME_TMP": runtimeTemp},
		nil,
	)
}

func assumeHiddenEntriesEnumerable(t *testing.T) {
	t.Helper()
	root := canonicalTempDir(t)
	hidden := filepath.Join(root, "hidden")
	if err := os.MkdirAll(hidden, 0o700); err != nil {
		t.Fatalf("create hidden fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "private"), []byte("private"), 0o600); err != nil {
		t.Fatalf("write hidden fixture: %v", err)
	}
	profile := `(version 1)
(allow default)
(deny file-read*
  (require-any
    (literal (param "HIDDEN"))
    (subpath (param "HIDDEN"))))
`
	runSeatbeltHelper(t, profile, map[string]string{"HIDDEN": hidden},
		"hidden-enumerable",
		map[string]string{
			"ASSUME_PARENT": root,
			"ASSUME_PATH":   filepath.Join(hidden, "private"),
		},
		nil,
	)
}

func TestSeatbeltAssumptionHelper(t *testing.T) {
	mode := os.Getenv(darwinAssumptionHelperEnv)
	if mode == "" {
		t.Skip("Seatbelt assumption helper subprocess")
	}
	switch mode {
	case "write-denied":
		if err := os.WriteFile(os.Getenv("ASSUME_PATH"), []byte("no"), 0o600); err == nil {
			t.Fatal("Seatbelt deny-write unexpectedly permitted the write")
		}
	case "read-denied":
		if _, err := os.ReadFile(os.Getenv("ASSUME_PATH")); err == nil {
			t.Fatal("Seatbelt deny-read unexpectedly permitted the read")
		}
	case "read-allowed":
		if _, err := os.ReadFile(os.Getenv("ASSUME_PATH")); err != nil {
			t.Fatalf("Seatbelt unexpectedly denied the read: %v", err)
		}
	case "unix-roundtrip":
		seatbeltHelperUnixRoundTrip(t)
	case "unix-connect-denied":
		socket := os.Getenv("ASSUME_SOCKET")
		conn, err := net.DialTimeout("unix", socket, time.Second)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("Seatbelt unexpectedly permitted connect to %s", socket)
		}
	case "unix-listen-denied":
		socket := os.Getenv("ASSUME_SOCKET")
		_ = os.Remove(socket)
		listener, err := net.Listen("unix", socket)
		if err == nil {
			_ = listener.Close()
			t.Fatalf("Seatbelt unexpectedly permitted bind/listen at %s", socket)
		}
	case "runtime-writes-allowed":
		seatbeltHelperRuntimeWrites(t)
	case "runtime-temp-denied":
		probe := filepath.Join(os.Getenv("ASSUME_RUNTIME_TMP"), "strict-deny")
		if err := os.WriteFile(probe, []byte("no"), 0o600); err == nil {
			_ = os.Remove(probe)
			t.Fatal("narrow runtime-temp deny did not override the baseline carveout")
		}
	case "hidden-enumerable":
		entries, err := os.ReadDir(os.Getenv("ASSUME_PARENT"))
		if err != nil {
			t.Fatalf("enumerate hidden entry parent: %v", err)
		}
		found := false
		for _, entry := range entries {
			if entry.Name() == "hidden" {
				found = true
			}
		}
		if !found {
			t.Fatal("Seatbelt unexpectedly removed hidden entry from parent enumeration")
		}
		if _, err := os.ReadFile(os.Getenv("ASSUME_PATH")); err == nil {
			t.Fatal("enumerable hidden entry remained readable")
		}
	default:
		t.Fatalf("unknown Seatbelt assumption helper mode %q", mode)
	}
}

func seatbeltHelperUnixRoundTrip(t *testing.T) {
	t.Helper()
	socket := os.Getenv("ASSUME_SOCKET")
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatalf("connect to Unix echo server: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set Unix round-trip deadline: %v", err)
	}
	const nonce = "seatbelt-assumption-roundtrip"
	if _, err := conn.Write([]byte(nonce)); err != nil {
		t.Fatalf("write Unix echo request: %v", err)
	}
	reply := make([]byte, len(nonce))
	_, err = io.ReadFull(conn, reply)
	if err != nil {
		t.Fatalf("read Unix echo reply: %v", err)
	}
	if string(reply) != nonce {
		t.Fatalf("Unix echo reply %q, want %q", reply, nonce)
	}
}

func seatbeltHelperRuntimeWrites(t *testing.T) {
	t.Helper()
	devNull, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open /dev/null through baseline carveout: %v", err)
	}
	if _, err := devNull.Write([]byte("probe")); err != nil {
		_ = devNull.Close()
		t.Fatalf("write /dev/null through baseline carveout: %v", err)
	}
	_ = devNull.Close()

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("open pty through baseline carveout: %v", err)
	}
	_ = ptmx.Close()
	_ = tty.Close()

	inherited := filepath.Join("/dev/fd", os.Getenv("ASSUME_INHERITED_FD"))
	inheritedFile, err := os.OpenFile(inherited, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open inherited fd through /dev/fd carveout: %v", err)
	}
	if _, err := inheritedFile.Write([]byte("probe")); err != nil {
		_ = inheritedFile.Close()
		t.Fatalf("write inherited fd through /dev/fd carveout: %v", err)
	}
	_ = inheritedFile.Close()

	probe := filepath.Join(os.Getenv("ASSUME_RUNTIME_TMP"), "runtime-write")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		t.Fatalf("write runtime temp through baseline carveout: %v", err)
	}
	_ = os.Remove(probe)
}

func runSeatbeltHelper(
	t *testing.T,
	profile string,
	params map[string]string,
	mode string,
	env map[string]string,
	extraFiles []*os.File,
) {
	t.Helper()
	args := []string{"-p", profile}
	for key, value := range params {
		args = append(args, "-D"+key+"="+value)
	}
	args = append(args,
		"--",
		os.Args[0],
		"-test.run=^TestSeatbeltAssumptionHelper$",
	)
	cmd := exec.Command(darwinSandboxExec, args...)
	cmd.Env = append(os.Environ(), darwinAssumptionHelperEnv+"="+mode)
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.ExtraFiles = extraFiles
	runCommand(t, cmd, 10*time.Second)
}

func startUnixEchoServer(t *testing.T, path string) func() {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("start Unix echo listener: %v", err)
	}
	ctx, cancel := contextWithCancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if unixListener, ok := listener.(*net.UnixListener); ok {
				_ = unixListener.SetDeadline(time.Now().Add(200 * time.Millisecond))
			}
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				select {
				case <-ctx:
					return
				default:
					continue
				}
			}
			go echoUnixConnection(conn)
		}
	}()
	return func() {
		cancel()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("Unix echo server did not stop")
		}
		_ = os.Remove(path)
	}
}

func shortDarwinTempDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "tclaude-sa-")
	if err != nil {
		t.Fatalf("create short Darwin temp directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove short Darwin temp directory: %v", err)
		}
	})
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("canonicalize short Darwin temp directory: %v", err)
	}
	return canonical
}

func echoUnixConnection(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	payload := make([]byte, 128)
	n, err := conn.Read(payload)
	if err != nil {
		return
	}
	_, _ = conn.Write(payload[:n])
}

func contextWithCancel() (<-chan struct{}, func()) {
	done := make(chan struct{})
	var closed bool
	return done, func() {
		if closed {
			return
		}
		closed = true
		close(done)
	}
}

func samePathBytes(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// assumeDefaultVolumeFolds is the hard, non-skippable macOS evidence behind
// TCL-981's case-insensitive containment work.
//
// The sandbox validator's spelling logic (sandboxpolicy.GuardContainsOrEqual and
// CanonicalHostSpelling) is volume-adaptive by design: it must merge
// case/normalization variants on a folding volume and keep them apart on a
// case-sensitive one. Its unit tests are correspondingly volume-adaptive, which
// means that on a case-sensitive runner they would quietly assert the
// case-sensitive branch and pass without ever exercising the fix.
//
// This assumption is what makes that impossible. It asserts, against the real
// CI filesystem, that the default macOS volume DOES fold — so the adaptive
// tests running in the macOS test shards are genuinely taking their folding
// branch. If a future runner image ships a case-sensitive boot volume, this
// fails loudly here instead of hollowing out the sandbox coverage in silence.
//
// It also pins the two properties the validator's seams depend on: that a
// case-flipped spelling of an existing directory reaches the same inode (which
// is exactly how sandboxpolicy.volumeFoldsCase probes a volume), and that NFD
// and NFC spellings do the same (which is the platform fact
// sandboxpolicy.volumeFoldsNormalization asserts on darwin).
func assumeDefaultVolumeFolds(t *testing.T) {
	t.Helper()
	root := canonicalTempDir(t)

	dir := filepath.Join(root, "TclaudeCaseProbe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create case probe directory: %v", err)
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("lstat case probe directory: %v", err)
	}

	// This is the probe sandboxpolicy.volumeFoldsCase performs in production.
	flipped := filepath.Join(root, "tCLAUDEcASEpROBE")
	flippedInfo, err := os.Lstat(flipped)
	if err != nil {
		t.Fatalf("the default macOS volume at %q did not resolve the case-flipped "+
			"spelling %q (%v).\n"+
			"tclaude's sandbox spelling tests are volume-adaptive, so a case-sensitive "+
			"volume here means the macOS test shards silently stop exercising the "+
			"case-insensitive containment path (TCL-981). Fix the runner volume or "+
			"stage this evidence on a case-insensitive image.",
			root, flipped, err)
	}
	if !os.SameFile(dirInfo, flippedInfo) {
		t.Fatalf("case-flipped spelling %q resolved to a different object than %q; "+
			"the default macOS volume is case-sensitive", flipped, dir)
	}

	// Normalization insensitivity is a separate property, and darwin's
	// volumeFoldsNormalization returns true as a platform fact. Pin that fact.
	nfc := filepath.Join(root, norm.NFC.String("Café-probe"))
	if err := os.MkdirAll(nfc, 0o755); err != nil {
		t.Fatalf("create normalization probe directory: %v", err)
	}
	nfcInfo, err := os.Lstat(nfc)
	if err != nil {
		t.Fatalf("lstat normalization probe directory: %v", err)
	}
	nfd := filepath.Join(root, norm.NFD.String("Café-probe"))
	if samePathBytes(nfc, nfd) {
		t.Fatalf("the NFC and NFD spellings are byte-identical; the probe proves nothing")
	}
	nfdInfo, err := os.Lstat(nfd)
	if err != nil {
		t.Fatalf("the default macOS volume did not resolve the NFD spelling %q (%v); "+
			"sandboxpolicy.volumeFoldsNormalization asserts APFS/HFS+ normalization "+
			"insensitivity as a platform fact on darwin", nfd, err)
	}
	if !os.SameFile(nfcInfo, nfdInfo) {
		t.Fatalf("NFD spelling %q resolved to a different object than NFC %q; "+
			"the darwin normalization seam's platform assumption is wrong", nfd, nfc)
	}

	t.Logf("default macOS volume at %q folds both case and Unicode normalization", root)
}
