package agentd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/portowner"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ---------------------------------------------------------------------------
// The access-control invariant, asserted structurally
// ---------------------------------------------------------------------------

// `--ui-server` has no authentication of any kind (TCL-1055), so the ownership
// proof in verifiedCopilotAPIPort is the entire access-control story — and it
// is a rule about CALL SITES that the function cannot enforce from inside
// itself. A second path that read the port out of the record and dialled it
// would not be a shortcut; it would be the hole.
//
// The two tests below are that rule written down. They are deliberately
// structural rather than behavioural: a behavioural test can only cover the
// paths someone thought to write, while the failure being guarded against is a
// path nobody thought about. A new dialling site trips this and has to argue
// for itself in review.
//
// They are not a security boundary — anyone editing the code can edit the test
// — they are a tripwire on an invariant whose violation is otherwise silent and
// looks like working code.

// copilotAPIGuardedSelector is one qualified name this package is allowed to
// use only from named files. Qualified rather than bare, because "Dial" alone
// matches every net.Dialer in the daemon and would make the guard noise.
type copilotAPIGuardedSelector struct {
	pkg    string
	symbol string
}

// copilotAPIGuardedFiles maps a guarded name to the files allowed to name it.
var copilotAPIGuardedFiles = map[copilotAPIGuardedSelector][]string{
	// The port record may only be read by the accessor that proves ownership
	// before returning it, and by the release path, which reads it to decide
	// whether to delete it and never to connect to it.
	{pkg: "db", symbol: "GetCopilotAPIRuntime"}: {
		"copilot_api_reachability.go", "copilot_api_port.go",
	},
	// Dialling the endpoint is the bootstrap's job alone, and the bootstrap's
	// address comes from verifiedCopilotAPIPort.
	{pkg: "copilotapi", symbol: "Dial"}:      {"copilot_api_bootstrap.go"},
	{pkg: "copilotapi", symbol: "DialRetry"}: {"copilot_api_bootstrap.go"},
}

// TestNothingReachesTheCopilotAPIPortOutsideTheVerifiedAccessor is the explicit
// "the unverified paths do not exist" check TCL-1054 asked TCL-1056 to write.
func TestNothingReachesTheCopilotAPIPortOutsideTheVerifiedAccessor(t *testing.T) {
	// Not every guarded name is in use today — copilotapi.Dial is listed so a
	// future non-retrying dial is caught the moment it appears, not after it
	// ships. What must be in use is the one call that IS the channel, and its
	// absence would mean this whole guard had quietly stopped watching anything.
	assert.Equal(t,
		[]string{"copilot_api_bootstrap.go"},
		copilotAPISymbolFiles(t, copilotAPIGuardedSelector{pkg: "copilotapi", symbol: "DialRetry"}),
		"the bootstrap is the one place that opens the channel; if it no longer "+
			"dials, this guard is protecting nothing")

	for guarded, allowed := range copilotAPIGuardedFiles {
		found := copilotAPISymbolFiles(t, guarded)
		for _, file := range found {
			assert.Contains(t, allowed, file,
				"%s.%s is named in %s, which is not one of the files allowed to reach the "+
					"Copilot API endpoint (%v). This endpoint has NO authentication, so "+
					"every byte sent to it must come from a port verifiedCopilotAPIPort "+
					"returned. If this new call site is legitimate, route it through that "+
					"accessor and then widen copilotAPIGuardedFiles deliberately",
				guarded.pkg, guarded.symbol, file, allowed)
		}
	}
}

// TestTheCopilotAPIBootstrapDialsOnlyAVerifiedPort pins the one dialling site
// to the accessor, so the allow-list above cannot be satisfied by a bootstrap
// that quietly stopped verifying.
func TestTheCopilotAPIBootstrapDialsOnlyAVerifiedPort(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(".", "copilot_api_bootstrap.go"))
	require.NoError(t, err)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "copilot_api_bootstrap.go", source, 0)
	require.NoError(t, err)

	var body *ast.FuncDecl
	ast.Inspect(file, func(node ast.Node) bool {
		if decl, ok := node.(*ast.FuncDecl); ok && decl.Name.Name == "bootstrapCopilotAPISession" {
			body = decl
		}
		return true
	})
	require.NotNil(t, body, "bootstrapCopilotAPISession must exist for this guard to mean anything")

	text := string(source[fset.Position(body.Pos()).Offset:fset.Position(body.End()).Offset])
	assert.Contains(t, text, "verifiedCopilotAPIPort(",
		"the only function that dials the endpoint must take its port from the accessor "+
			"that proves the listener belongs to the agent's pane")
	assert.Contains(t, text, "handle.StillOwned()",
		"the connection must be re-proved after it is established: the pre-dial proof is "+
			"one-shot and leaves a TOCTOU window that only a post-connect re-read closes")
}

// copilotAPISymbolFiles returns the non-test files in this package that name
// the qualified selector pkg.Symbol.
func copilotAPISymbolFiles(t *testing.T, guarded copilotAPIGuardedSelector) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		require.NoError(t, err)
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, name, source, 0)
		require.NoError(t, err)

		named := false
		ast.Inspect(parsed, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != guarded.symbol {
				return true
			}
			if qualifier, ok := selector.X.(*ast.Ident); ok && qualifier.Name == guarded.pkg {
				named = true
			}
			return true
		})
		if named && !slices.Contains(files, name) {
			files = append(files, name)
		}
	}
	return files
}

// ---------------------------------------------------------------------------
// The spawn-boundary refusal
// ---------------------------------------------------------------------------

// The boundary wrapper's job is to turn the session-package refusal into a
// named spawn failure. The reasoning is tested next to the rule it enforces;
// what matters here is that the operator gets a code they can act on and that a
// launch which did not ask for the drive is untouched.
func TestCopilotAPIFolderTrustFailureAtTheSpawnBoundary(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	environment := &sandboxpolicy.Snapshot{
		Effective: sandboxpolicy.EffectiveProfile{
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: harness.CopilotHomeEnvVar, Value: home},
				{Name: "HOME", Value: home},
			},
		},
	}

	t.Run("a send-keys spawn is untouched", func(t *testing.T) {
		assert.Nil(t, copilotAPIFolderTrustFailure(spawnParams{
			Harness: harness.CopilotName, Cwd: cwd, EffectiveSandbox: environment,
		}))
	})

	t.Run("an untrusted API spawn is refused with a named reason", func(t *testing.T) {
		fail := copilotAPIFolderTrustFailure(spawnParams{
			Harness: harness.CopilotName, CopilotAPI: true, Cwd: cwd,
			EffectiveSandbox: environment,
		})
		require.NotNil(t, fail)
		assert.Equal(t, "copilot_api_untrusted_launch_dir", fail.Kind)
		assert.Contains(t, fail.Msg, "--trust-dir")
	})

	t.Run("a spawn that will pre-trust is admitted", func(t *testing.T) {
		assert.Nil(t, copilotAPIFolderTrustFailure(spawnParams{
			Harness: harness.CopilotName, CopilotAPI: true, TrustDir: true, Cwd: cwd,
			EffectiveSandbox: environment,
		}))
	})
}

// ---------------------------------------------------------------------------
// The live-handle registry
// ---------------------------------------------------------------------------

// fakeCopilotServer is the smallest thing that will complete copilotapi's
// handshake: enough to obtain a REAL client, so the registry's liveness
// question is answered by a real connection dying rather than by a flag a test
// set.
type fakeCopilotServer struct {
	listener net.Listener
	mu       sync.Mutex
	conns    []net.Conn
}

func newFakeCopilotServer(t *testing.T) *fakeCopilotServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &fakeCopilotServer{listener: listener}
	go server.accept()
	t.Cleanup(server.close)
	return server
}

func (s *fakeCopilotServer) port() int { return s.listener.Addr().(*net.TCPAddr).Port }

func (s *fakeCopilotServer) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go s.serve(conn)
	}
}

func (s *fakeCopilotServer) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		length := 0
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if name, value, ok := strings.Cut(line, ":"); ok &&
				strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
				_, _ = fmt.Sscanf(strings.TrimSpace(value), "%d", &length)
			}
		}
		payload := make([]byte, length)
		if _, err := readFull(reader, payload); err != nil {
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(payload, &request); err != nil || len(request.ID) == 0 {
			continue
		}
		result := json.RawMessage(`{}`)
		if request.Method == copilotapi.MethodConnect {
			result = json.RawMessage(fmt.Sprintf(
				`{"ok":true,"protocolVersion":%d,"version":"1.0.78"}`,
				copilotapi.SupportedProtocolVersion))
		}
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": request.ID, "result": result,
		})
		_, _ = fmt.Fprintf(conn, "Content-Length: %d\r\n\r\n%s", len(body), body)
	}
}

func readFull(reader *bufio.Reader, buf []byte) (int, error) {
	read := 0
	for read < len(buf) {
		n, err := reader.Read(buf[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

func (s *fakeCopilotServer) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.conns {
		_ = conn.Close()
	}
}

func dialFakeCopilot(t *testing.T, server *fakeCopilotServer) *copilotapi.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := copilotapi.Dial(ctx, server.listener.Addr().String(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// "Is this agent API-connected" must be answered by the connection. The port
// record is a real value that answers a different question — what a launch was
// told to bind — and TCL-1051's proxy-value note names this surface as the next
// place that mistake would land.
func TestCopilotAPIConnectedFollowsTheConnectionNotTheRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	registry := &copilotAPISessionRegistry{}
	server := newFakeCopilotServer(t)
	client := dialFakeCopilot(t, server)

	// A perfectly good port record, and no connection. Anything sourcing
	// connectedness from the record would already say yes here.
	require.NoError(t, db.UpsertCopilotAPIRuntime(db.CopilotAPIRuntime{
		ConvID: "conv-1", Port: server.port(),
	}))
	assert.False(t, registry.Connected("conv-1"),
		"a recorded port is not a connection")

	registry.Adopt(&copilotAPISession{
		ConvID: "conv-1", SessionID: "sess-1", Port: server.port(), Client: client,
	})
	assert.True(t, registry.Connected("conv-1"))

	// The record is untouched by the connection ending, which is exactly why it
	// cannot be the source of this answer.
	require.NoError(t, client.Close())
	assert.False(t, registry.Connected("conv-1"),
		"a closed connection means not connected, whatever the record still says")
	runtime, err := db.GetCopilotAPIRuntime("conv-1")
	require.NoError(t, err)
	require.NotNil(t, runtime, "the record outlives the connection, as it should")
}

// A conversation outlives its launches, and a relaunch binds a new port. The
// registry must hand the successor's handle over the predecessor's rather than
// keeping a connection to a pane that no longer exists.
func TestCopilotAPIRegistryReplacesAPredecessorsHandle(t *testing.T) {
	registry := &copilotAPISessionRegistry{}
	server := newFakeCopilotServer(t)
	first := dialFakeCopilot(t, server)
	second := dialFakeCopilot(t, server)

	registry.Adopt(&copilotAPISession{ConvID: "conv-1", SessionID: "old", Client: first})
	registry.Adopt(&copilotAPISession{ConvID: "conv-1", SessionID: "new", Client: second})

	handle := registry.Handle("conv-1")
	require.NotNil(t, handle)
	assert.Equal(t, "new", handle.SessionID)

	assert.Eventually(t, func() bool {
		select {
		case <-first.Done():
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond,
		"the replaced handle's connection must be closed, not leaked")
}

// Drop is the release path's housekeeping. It must close the connection as well
// as forget it, or a retired conversation leaves a socket open for the lifetime
// of the daemon.
func TestCopilotAPIRegistryDropClosesTheConnection(t *testing.T) {
	registry := &copilotAPISessionRegistry{}
	server := newFakeCopilotServer(t)
	client := dialFakeCopilot(t, server)

	registry.Adopt(&copilotAPISession{ConvID: "conv-1", Client: client})
	registry.Drop("conv-1")

	assert.Nil(t, registry.Handle("conv-1"))
	assert.Eventually(t, func() bool {
		select {
		case <-client.Done():
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)
}

// StillOwned is the answer to "is the thing on the other end of this connection
// still my agent", and it needs BOTH halves. A live connection to a port this
// process does not own must not pass, or the check degrades into "is my socket
// open" — which an impostor's socket satisfies just as well.
func TestCopilotAPISessionStillOwnedNeedsBothHalves(t *testing.T) {
	server := newFakeCopilotServer(t)
	client := dialFakeCopilot(t, server)

	owned := &copilotAPISession{
		ConvID: "conv-1", Port: server.port(), PanePID: os.Getpid(), Client: client,
	}
	require.True(t, portowner.ProcessOwnsLoopbackPort(os.Getpid(), server.port()),
		"precondition: this test process owns the fake server's listener")
	assert.True(t, owned.StillOwned())

	foreign := &copilotAPISession{
		ConvID: "conv-1", Port: server.port(), PanePID: 1, Client: client,
	}
	assert.False(t, foreign.StillOwned(),
		"a live connection to a listener owned by someone else is exactly the case "+
			"this guards against")

	require.NoError(t, client.Close())
	assert.False(t, owned.StillOwned(),
		"ownership of the port says nothing once our own connection is gone")
}

// A bootstrap for a conversation nobody launched on the API drive must fail on
// the missing record rather than fall back to some other way of finding a port.
func TestBootstrapCopilotAPISessionRefusesAnUnrecordedConversation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := bootstrapCopilotAPISession(ctx, "conv-unknown")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no recorded Copilot API port")
}

// startCopilotAPIBootstrap must be a no-op for every launch that did not take
// the drive, and for one whose conversation id is not known yet — those record
// their port from the point they discover it and call this again there.
func TestStartCopilotAPIBootstrapIsQuietWithoutADriveOrAConversation(t *testing.T) {
	startCopilotAPIBootstrap("conv-1", false)
	startCopilotAPIBootstrap("", true)
	assert.Nil(t, copilotAPISessions.Handle("conv-1"))
}
