package agentd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"os/exec"
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
//
// # WHAT THESE GUARDS COVER: DIALLING, NOT DRIVING
//
// Read this before concluding they cover more, because everyone did (TCL-1075).
//
// They constrain who may OPEN a channel. They say nothing about what is done
// with an already-established *copilotapi.Client — there is no guard on client
// methods at all, and there deliberately is not one. The invariant people
// believe these enforce is "every byte comes from a re-proved connection", and
// that is two properties with very different natures:
//
//   - PROVENANCE — the client came from a verified dial. This is closed
//     STRUCTURALLY rather than by any test. A client exists in exactly four
//     shapes: the registry's copilotAPISession.Client, the state consumer's
//     non-transmitting field, and the two dialling sites' own locals (the
//     bootstrap's, which it passes to openCopilotAPISession, and the
//     reconnect's, which is pinned separately to issue no mutating call). The
//     dialling sites construct the handle and the registry stores it, so every
//     transmitting client in the daemon traces back to a dial these guards pin.
//     A new file cannot reach an unproved endpoint because there is nothing for
//     it to reach one WITH.
//   - FRESHNESS — ownership was re-proved shortly BEFORE this particular send.
//     This is temporal, and no syntax walk can assert it. It is enforced by
//     routing every verb through copilotAPIDrive, which re-proves — by
//     convention and review, not structurally.
//
// So a guard on client methods would constrain WHERE the calls are, which is
// already closed for a reason needing no test, while leaving untouched the thing
// that would actually break. That is why widening was considered and declined.
//
// The cost of the belief was not hypothetical: the one send in the package whose
// proof did not share a call stack with it — the compaction, which proved on one
// goroutine and sent on another — sat there the whole time these guards were
// read as covering it. It is fixed, and the test that pins it is behavioural,
// because the property that would break is not expressible here.

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
	// Enumerating the records is the same rule one step earlier: it is how a
	// caller learns an endpoint exists at all. It is listed even though it
	// returns conv ids rather than ports, because the reason to keep it beside
	// the verified accessor is that a caller holding the record set is one field
	// away from holding the numbers.
	{pkg: "db", symbol: "ListCopilotAPIRuntimeConvIDs"}: {
		"copilot_api_reachability.go",
	},
	// Dialling the endpoint.
	//
	// TCL-1056 wrote this rule with ONE entitled file, and with the rule that a
	// second one must argue for itself in review rather than appear. TCL-1074 is
	// that argument, made deliberately:
	//
	// copilot_api_reconnect.go re-establishes a channel that an agentd restart
	// lost, for a pane that never stopped running. It cannot go through the
	// bootstrap, because a bootstrap OPENS a session and this must not: the
	// session already exists, and every call that would open one is destructive
	// or disturbing in at least one of the cases a reconnect cannot tell apart in
	// advance (see that file's header). What it inherits unchanged is the part
	// the guard is about — its address comes from verifiedCopilotAPIPort, and it
	// re-proves ownership on the live connection before it reads anything.
	//
	// Both entitled files are pinned individually below, so an entry that stopped
	// verifying could not hide behind the other one's compliance.
	{pkg: "copilotapi", symbol: "Dial"}: {"copilot_api_bootstrap.go"},
	{pkg: "copilotapi", symbol: "DialRetry"}: {
		"copilot_api_bootstrap.go", "copilot_api_reconnect.go",
	},
}

// TestNothingReachesTheCopilotAPIPortOutsideTheVerifiedAccessor is the explicit
// "the unverified paths do not exist" check TCL-1054 asked TCL-1056 to write.
func TestNothingReachesTheCopilotAPIPortOutsideTheVerifiedAccessor(t *testing.T) {
	// Not every guarded name is in use today — copilotapi.Dial is listed so a
	// future non-retrying dial is caught the moment it appears, not after it
	// ships. What must be in use are the calls that ARE the channel, and their
	// absence would mean this whole guard had quietly stopped watching anything.
	//
	// Asserted as the exact set rather than as membership: a file dropping out is
	// as much a break as one appearing. The reconnect losing its dial would mean
	// an agentd restart silently stopped re-establishing channels, which is a
	// working-looking daemon and mute agents.
	assert.ElementsMatch(t,
		[]string{"copilot_api_bootstrap.go", "copilot_api_reconnect.go"},
		copilotAPISymbolFiles(t, copilotAPIGuardedSelector{pkg: "copilotapi", symbol: "DialRetry"}),
		"the bootstrap opens the channel and the reconnect re-opens it after an agentd "+
			"restart; if either no longer dials, this guard is protecting less than it says")

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

// TestEveryCopilotAPIDiallerVerifiesItsPort pins EACH dialling site to the
// accessor, so the allow-list above cannot be satisfied by an entitled file
// that quietly stopped verifying.
//
// Per-function rather than per-file, and each entry named individually: two
// entitled files means the guard must not be satisfiable by one of them
// complying on the other's behalf. Both must take their address from
// verifiedCopilotAPIPort and both must re-prove ownership on the live
// connection, because the reasons are the same for both — the endpoint has no
// authentication, and the pre-dial proof is one-shot.
func TestEveryCopilotAPIDiallerVerifiesItsPort(t *testing.T) {
	diallers := map[string]string{
		"copilot_api_bootstrap.go": "bootstrapCopilotAPISession",
		// A restart is the moment a recorded port is MOST likely to have been
		// reused by something else, so the reconnect needs this at least as much
		// as the bootstrap does.
		"copilot_api_reconnect.go": "reconnectCopilotAPISession",
	}
	for filename, function := range diallers {
		t.Run(function, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join(".", filename))
			require.NoError(t, err)

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, filename, source, 0)
			require.NoError(t, err)

			var body *ast.FuncDecl
			ast.Inspect(file, func(node ast.Node) bool {
				if decl, ok := node.(*ast.FuncDecl); ok && decl.Name.Name == function {
					body = decl
				}
				return true
			})
			require.NotNil(t, body, "%s must exist for this guard to mean anything", function)

			assert.True(t, callAlwaysEvaluated(body.Body, "verifiedCopilotAPIPort"),
				"%s must take its port from verifiedCopilotAPIPort, which proves the "+
					"listener belongs to the agent's pane — and must do so where it "+
					"always runs. A call parked on a branch, or a mention in a comment, "+
					"is not a proof", function)
			assert.True(t, callAlwaysEvaluated(body.Body, "handle.StillOwned"),
				"%s must re-prove ownership on the live connection: the pre-dial proof "+
					"is one-shot and leaves a TOCTOU window that only a post-connect "+
					"re-read closes. Measured before this guard was rewritten: deleting "+
					"this call entirely and leaving only a comment naming it kept the "+
					"whole agentd package green", function)
		})
	}
}

// alwaysEvaluatedExpr reports whether a call to name sits inside node in a
// position that is evaluated whenever node itself is.
//
// It is a DESCENT THAT REFUSES TO ENTER SKIPPABLE POSITIONS, not a subtree walk
// that looks for a name and then reasons about where it landed. That difference
// is the whole correctness argument, and it was bought twice: a walk can only
// ever be as complete as its author's list of places to exclude, and both this
// guard family's beats came from a position nobody had enumerated. A descent
// that must justify every step cannot be beaten by a construction nobody thought
// of, because the default is to refuse.
//
// The rule it implements:
//
//	A CALL IS UNCONDITIONALLY EVALUATED ONLY IF NO OPERATOR BETWEEN IT AND THE
//	STATEMENT ROOT CAN SKIP IT.
//
// So it never enters a function literal — defining a body does not run it — and
// for && and || it enters only the leftmost operand, since every other operand is
// conditional on that one. Everything else (parens, unary, selectors, call
// arguments, composite-literal elements) always evaluates, so it descends.
//
// Both refusals are measured rather than assumed. Against the version that used
// ast.Inspect: `cb := func() { handle.StillOwned() }` with cb never invoked, and
// `if x != nil && handle.StillOwned()`, were both reported as always evaluated.
// The first is the same beat that defeated the revoke guard's FuncDecl-only walk
// earlier in this ticket family — re-imported here because the earlier fix
// patched one guard rather than changing how these walks are written.
func alwaysEvaluatedExpr(node ast.Node, name string) bool {
	if node == nil {
		return false
	}
	switch typed := node.(type) {
	case *ast.FuncLit:
		return false
	case *ast.BinaryExpr:
		if typed.Op == token.LAND || typed.Op == token.LOR {
			return alwaysEvaluatedExpr(typed.X, name)
		}
		return alwaysEvaluatedExpr(typed.X, name) || alwaysEvaluatedExpr(typed.Y, name)
	case *ast.CallExpr:
		if calleeName(typed) == name {
			return true
		}
		if alwaysEvaluatedExpr(typed.Fun, name) {
			return true
		}
		for _, argument := range typed.Args {
			if alwaysEvaluatedExpr(argument, name) {
				return true
			}
		}
		return false
	case *ast.ParenExpr:
		return alwaysEvaluatedExpr(typed.X, name)
	case *ast.UnaryExpr:
		return alwaysEvaluatedExpr(typed.X, name)
	case *ast.StarExpr:
		return alwaysEvaluatedExpr(typed.X, name)
	case *ast.SelectorExpr:
		return alwaysEvaluatedExpr(typed.X, name)
	case *ast.IndexExpr:
		return alwaysEvaluatedExpr(typed.X, name) || alwaysEvaluatedExpr(typed.Index, name)
	case *ast.TypeAssertExpr:
		return alwaysEvaluatedExpr(typed.X, name)
	case *ast.CompositeLit:
		for _, element := range typed.Elts {
			if alwaysEvaluatedExpr(element, name) {
				return true
			}
		}
		return false
	case *ast.KeyValueExpr:
		return alwaysEvaluatedExpr(typed.Value, name)
	case *ast.ExprStmt:
		return alwaysEvaluatedExpr(typed.X, name)
	case *ast.AssignStmt:
		for _, rhs := range typed.Rhs {
			if alwaysEvaluatedExpr(rhs, name) {
				return true
			}
		}
		return false
	case *ast.ReturnStmt:
		for _, result := range typed.Results {
			if alwaysEvaluatedExpr(result, name) {
				return true
			}
		}
		return false
	}
	return false
}

// calleeName is the called function's name, QUALIFIED by its receiver when the
// receiver is a plain identifier.
//
// Qualified deliberately. Matching on the bare method name let a proof about an
// unrelated value satisfy a guard about a specific one: under review,
// `(&copilotAPISession{}).StillOwned()` — ownership re-proved on a zero-value
// struct — passed the dialler guard and stood in for the endpoint's entire
// access-control story. A name is only evidence when it is the name of the thing
// being asserted about.
func calleeName(call *ast.CallExpr) string {
	switch callee := call.Fun.(type) {
	case *ast.Ident:
		return callee.Name
	case *ast.SelectorExpr:
		if receiver, ok := callee.X.(*ast.Ident); ok {
			return receiver.Name + "." + callee.Sel.Name
		}
		return ""
	}
	return ""
}

// callAlwaysEvaluated reports whether a call to name runs whenever body runs.
//
// # Why position and not presence
//
// Because this guard used to match SOURCE TEXT — it sliced the function body's
// bytes and asked whether the string appeared in them. That range includes
// comments, so a comment naming the call satisfied it. Measured rather than
// argued: deleting reconnectCopilotAPISession's post-connect StillOwned re-proof
// outright and leaving behind only a comment that named it left the ENTIRE agentd
// package green, with the TOCTOU window the check exists to close wide open.
//
// These are the ownership properties of an endpoint with NO authentication, so
// presence is not a strong enough claim to make about them.
//
// # What it cannot see
//
// Reachability. An early return above the statement leaves the call correctly
// positioned and never executed, and no static check of call shape sees that. It
// is also conservative in the other direction: a call reached only through a
// loop, a select, a defer or a go statement is refused even where it does in fact
// run, because entering those would mean reasoning about control flow this does
// not model. A guard that rejects correct code is a real cost — the next person
// edits it out — so that conservatism is a bounded, deliberate one rather than an
// oversight.
func callAlwaysEvaluated(body *ast.BlockStmt, name string) bool {
	if body == nil {
		return false
	}
	for _, statement := range body.List {
		if labeled, ok := statement.(*ast.LabeledStmt); ok {
			statement = labeled.Stmt
		}
		switch typed := statement.(type) {
		case *ast.ExprStmt, *ast.AssignStmt, *ast.ReturnStmt:
			if alwaysEvaluatedExpr(typed, name) {
				return true
			}
		case *ast.IfStmt:
			if alwaysEvaluatedExpr(typed.Init, name) ||
				alwaysEvaluatedExpr(typed.Cond, name) {
				return true
			}
		case *ast.SwitchStmt:
			if alwaysEvaluatedExpr(typed.Init, name) ||
				alwaysEvaluatedExpr(typed.Tag, name) {
				return true
			}
		}
	}
	return false
}

// TestTheCopilotAPIReconnectIssuesNoMutatingCall is the reconnect's own
// invariant, and it is the reason that path is allowed to exist at all.
//
// A reconnect rejoins a conversation it must not change. `session.create` at a
// COLD id starts it fresh and would discard the agent's history; `session.
// resume` appends an event and re-applies options; `session.setForeground`
// moves what the human is looking at. None of the three is safe in every case a
// reconnect cannot tell apart in advance, so the path issues exactly one read
// and nothing else.
//
// Structural rather than behavioural for the usual reason: the failure being
// guarded against is a call nobody thought about. A behavioural test would need
// to already know which call was added.
func TestTheCopilotAPIReconnectIssuesNoMutatingCall(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(".", "copilot_api_reconnect.go"))
	require.NoError(t, err)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "copilot_api_reconnect.go", source, 0)
	require.NoError(t, err)

	// Every method called on the connection, by name. Comments are excluded by
	// construction — the walk sees expressions, not prose — which matters here
	// because the file's own docs discuss the forbidden calls at length.
	//
	// BOTH spellings are matched, and the second is the one that matters. A
	// syntax-only walk cannot see that a receiver is a *copilotapi.Client, so
	// this keys on how the receiver is WRITTEN — and an earlier version keyed
	// only on a local named `client`, which is not how this package usually
	// reaches a connection at all. `handle.Client.Send(...)` is the idiom in
	// copilot_api_drive.go and copilot_api_state.go, i.e. exactly the spelling a
	// future mutating call would arrive in. A guard that reports clean over the
	// calls it cannot see is worse than no guard, because it still reads as
	// protection.
	//
	// So: any call whose receiver chain ENDS in an identifier named `client` or a
	// field named `Client`. The positive control below is what keeps the matcher
	// itself honest — if it stops matching, the test says so instead of passing.
	// Aliases first. Keying on the receiver's SPELLING means one assignment
	// escapes the whole guard: `conn := client` and then `conn.SetForeground...`
	// was measured under review to leave this green with the reconnect issuing a
	// mutating RPC. So any local that takes its value from a client-shaped
	// expression becomes a client name too, transitively.
	clientNames := map[string]bool{"client": true}
	for changed := true; changed; {
		changed = false
		ast.Inspect(file, func(node ast.Node) bool {
			assign, ok := node.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != len(assign.Rhs) {
				return true
			}
			for i, rhs := range assign.Rhs {
				target, ok := assign.Lhs[i].(*ast.Ident)
				if !ok || clientNames[target.Name] {
					continue
				}
				var isClient bool
				switch source := rhs.(type) {
				case *ast.Ident:
					isClient = clientNames[source.Name]
				case *ast.SelectorExpr:
					isClient = source.Sel.Name == "Client" || source.Sel.Name == "client"
				}
				if isClient {
					clientNames[target.Name] = true
					changed = true
				}
			}
			return true
		})
	}

	var onTheClient []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch receiver := selector.X.(type) {
		case *ast.Ident: // client.X(...), or any alias of it
			if !clientNames[receiver.Name] {
				return true
			}
		case *ast.SelectorExpr: // handle.Client.X(...), c.client.X(...)
			if receiver.Sel.Name != "Client" && receiver.Sel.Name != "client" {
				return true
			}
		default:
			return true
		}
		onTheClient = append(onTheClient, selector.Sel.Name)
		return true
	})

	// The positive control. Without it every assertion below would pass just as
	// well against a file that had stopped calling the client entirely — which is
	// exactly the shape of vacuously-green test this series keeps finding.
	assert.Contains(t, onTheClient, "IsProcessing",
		"the drivability probe is the one call this path makes; if it is gone, this "+
			"test is asserting the absence of calls in a file that makes none")

	allowed := []string{"IsProcessing", "Close"}
	for _, method := range onTheClient {
		assert.Contains(t, allowed, method,
			"copilot_api_reconnect.go calls Client.%s. A reconnect must issue NO mutating "+
				"RPC: it rejoins a live conversation and cannot tell in advance whether the "+
				"session is one the server still holds or one that only exists on disk, where "+
				"`session.create` starts it FRESH. If a second call is genuinely needed, argue "+
				"the zero-mutating-RPC property again before widening this list", method)
	}
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
	// calls records every request the server was asked to answer, in order, so
	// a test can assert WHICH typed call carried a delivery instead of only
	// that some bytes moved.
	calls []fakeCopilotCall
	// failures maps a method to the error message it should fail with,
	// letting a test drive the "the channel is there and the call failed"
	// case, which is the one where a fallback to keystrokes would be tempting.
	failures map[string]string
	// disconnects names methods whose next request should lose its connection
	// before a reply. The listener stays up, so tests can distinguish a dead
	// channel that reconnects from a dead server that cannot.
	disconnects map[string]bool
	// results answers named methods with a canned payload. A method with no
	// entry still gets an empty object, which is what the registry tests need
	// and what a consumer decoding only fields it uses tolerates.
	results map[string]string
	// writeMu serialises frame writes across the serve loop and push.
	writeMu sync.Mutex
}

// fakeCopilotCall is one request the fake server answered.
type fakeCopilotCall struct {
	Method string
	Params json.RawMessage
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

// answer sets the raw JSON result for a method.
func (s *fakeCopilotServer) answer(method, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.results == nil {
		s.results = map[string]string{}
	}
	s.results[method] = result
}

// callCount reports how many times a method has been asked for.
func (s *fakeCopilotServer) callCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, call := range s.calls {
		if call.Method == method {
			count++
		}
	}
	return count
}

// push sends a notification to every connected client, standing in for the
// server's own event stream.
func (s *fakeCopilotServer) push(method, params string) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": json.RawMessage(params),
	})
	s.mu.Lock()
	conns := append([]net.Conn(nil), s.conns...)
	s.mu.Unlock()
	for _, conn := range conns {
		s.writeFrame(conn, body)
	}
}

// writeFrame serialises writes to one connection. The serve loop and push both
// write frames, from different goroutines, and two interleaved writes would
// corrupt the stream rather than merely reorder it.
func (s *fakeCopilotServer) writeFrame(conn net.Conn, body []byte) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = fmt.Fprintf(conn, "Content-Length: %d\r\n\r\n%s", len(body), body)
}

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
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(payload, &request); err != nil || len(request.ID) == 0 {
			continue
		}
		s.mu.Lock()
		s.calls = append(s.calls, fakeCopilotCall{Method: request.Method, Params: request.Params})
		failure, failed := s.failures[request.Method]
		disconnect := s.disconnects[request.Method]
		delete(s.disconnects, request.Method)
		canned, cannedOK := s.results[request.Method]
		s.mu.Unlock()
		if disconnect {
			_ = conn.Close()
			return
		}

		envelope := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		switch {
		case failed:
			envelope["error"] = map[string]any{
				"code": copilotapi.CodeInternalError, "message": failure,
			}
		case request.Method == copilotapi.MethodConnect:
			envelope["result"] = json.RawMessage(fmt.Sprintf(
				`{"ok":true,"protocolVersion":%d,"version":"1.0.78"}`,
				copilotapi.SupportedProtocolVersion))
		case cannedOK:
			// After the handshake and an explicit failure, so a test can canned
			// a method the switch below also has a default for, but cannot
			// accidentally break the connect the client needs to exist at all.
			envelope["result"] = json.RawMessage(canned)
		case request.Method == copilotapi.MethodSessionSend:
			envelope["result"] = json.RawMessage(`{"messageId":"msg-1"}`)
		case request.Method == copilotapi.MethodSessionCompact:
			// A REALISTIC success, not `{}`. The bare object decodes to
			// success:false, and Compact now treats that as the in-band refusal
			// it is — so a default of `{}` would have made every compaction test
			// exercise the failure path while reading as the success one. The
			// refusal itself is pinned where the conversion lives, in
			// copilotapi's own tests.
			envelope["result"] = json.RawMessage(
				`{"success":true,"tokensRemoved":-213,"messagesRemoved":5}`)
		default:
			envelope["result"] = json.RawMessage(`{}`)
		}
		body, _ := json.Marshal(envelope)
		s.writeFrame(conn, body)
	}
}

// methodsCalled returns the request methods the server has answered, in order.
func (s *fakeCopilotServer) methodsCalled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	methods := make([]string, 0, len(s.calls))
	for _, call := range s.calls {
		methods = append(methods, call.Method)
	}
	return methods
}

// paramsFor returns the params of the first call to method, or nil.
func (s *fakeCopilotServer) paramsFor(method string) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, call := range s.calls {
		if call.Method == method {
			return call.Params
		}
	}
	return nil
}

// failMethod makes the server answer method with a JSON-RPC error.
func (s *fakeCopilotServer) failMethod(method, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures == nil {
		s.failures = map[string]string{}
	}
	s.failures[method] = message
}

func (s *fakeCopilotServer) disconnectMethodOnce(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disconnects == nil {
		s.disconnects = map[string]bool{}
	}
	s.disconnects[method] = true
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

	// A real, live pid whose subtree genuinely excludes the listener: a child we
	// spawn here, which owns nothing.
	//
	// Both obvious shortcuts are wrong, in opposite directions. pid 1 is
	// rejected by portowner before it ever compares an inode, so the arm would
	// pass without exercising the comparison it names. os.Getppid() is worse: an
	// ANCESTOR's subtree contains this test binary, so the listener really does
	// belong to it and StillOwned answers true — correctly. The pid has to be a
	// non-ancestor.
	stranger := exec.Command("sleep", "60")
	require.NoError(t, stranger.Start())
	t.Cleanup(func() {
		_ = stranger.Process.Kill()
		_ = stranger.Wait()
	})
	require.False(t, portowner.ProcessOwnsLoopbackPort(stranger.Process.Pid, server.port()),
		"precondition: the stranger must not own the listener, or the arm below "+
			"proves nothing")

	foreign := &copilotAPISession{
		ConvID: "conv-1", Port: server.port(), PanePID: stranger.Process.Pid, Client: client,
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
	_, err := bootstrapCopilotAPISession(ctx, "conv-unknown", copilotAPILaunchFresh, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no recorded Copilot API port")
}

// startCopilotAPIBootstrap must be a no-op for every launch that did not take
// the drive, and for one whose conversation id is not known yet — those record
// their port from the point they discover it and call this again there.
// Called through runCopilotAPIBootstrap rather than the startCopilotAPIBootstrap
// variable ON PURPOSE: TestMain swaps that variable for a binary-wide no-op, so
// a test driving it would pass with both guard clauses deleted.
func TestStartCopilotAPIBootstrapIsQuietWithoutADriveOrAConversation(t *testing.T) {
	called := make(chan string, 4)
	original := bootstrapCopilotAPISessionFn
	bootstrapCopilotAPISessionFn = func(
		_ context.Context, convID string, _ copilotAPILaunchKind, _ string,
	) (*copilotAPISession, error) {
		called <- convID
		return nil, errors.New("should not have been reached")
	}
	t.Cleanup(func() { bootstrapCopilotAPISessionFn = original })

	runCopilotAPIBootstrap("conv-1", false, copilotAPILaunchFresh, "", 1)
	runCopilotAPIBootstrap("", true, copilotAPILaunchFresh, "", 1)

	// The positive control. Without it the two negatives above would pass just
	// as well against a function that never calls the seam at all, which is the
	// exact failure this test was written to stop being.
	runCopilotAPIBootstrap("conv-2", true, copilotAPILaunchFresh, "", 1)
	select {
	case got := <-called:
		assert.Equal(t, "conv-2", got,
			"the only launch that asked for the drive AND named a conversation")
	case <-time.After(10 * time.Second):
		t.Fatal("an API launch with a conversation never reached the bootstrap")
	}

	// Drained after the control, so anything the guarded calls let through has
	// had at least as long to arrive as the control took.
	select {
	case got := <-called:
		t.Fatalf("a guarded call reached the bootstrap: conv_id=%q", got)
	default:
	}
	assert.Nil(t, copilotAPISessions.Handle("conv-1"))
}

// TestCopilotAPICompactionProvesOwnershipOnTheGoroutineThatSends is the
// behavioural half of TCL-1075, and it exists because the structural guards
// above cannot express it.
//
// The compaction used to prove ownership in compactCopilotAPISession and then
// hand the handle to a goroutine that sent on it. A proof taken on one
// goroutine says nothing about the moment a different one sends, so that was
// the single send in the package reaching the endpoint on an unproved
// connection — under guards everyone read as preventing exactly that.
//
// Asserted as opposite responses to opposite inputs, since "no compact was
// sent" is satisfied just as well by a function that does nothing at all.
func TestCopilotAPICompactionProvesOwnershipOnTheGoroutineThatSends(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()
	t.Cleanup(resetCopilotAPIStateForTest)

	// A live pid whose subtree genuinely excludes the listener — the same
	// non-ancestor requirement StillOwned's own test documents.
	stranger := exec.Command("sleep", "60")
	require.NoError(t, stranger.Start())
	t.Cleanup(func() {
		_ = stranger.Process.Kill()
		_ = stranger.Wait()
	})

	// Neither arm canneds a reply. The fake's defaults for compact and send are
	// already realistic, and a hand-written one here is how the success arm
	// stops being one: a compact payload without "success":true decodes to an
	// in-band refusal, so the arm would take the failure branch while reading as
	// the success path. The fake documents that trap where its default lives.
	t.Run("refuses when ownership can no longer be proved", func(t *testing.T) {
		server := newFakeCopilotServer(t)
		client := dialFakeCopilot(t, server)

		require.False(t, portowner.ProcessOwnsLoopbackPort(stranger.Process.Pid, server.port()),
			"precondition: the stranger must not own the listener")
		copilotAPISessions.Adopt(&copilotAPISession{
			ConvID: "conv-unproved", SessionID: "sess-1", Client: client,
			Port: server.port(), PanePID: stranger.Process.Pid,
		})
		t.Cleanup(func() { copilotAPISessions.Drop("conv-unproved") })

		// Called directly rather than through compactCopilotAPISession, because
		// the property under test belongs to this function: it must refuse on its
		// OWN account rather than trust a handle a caller proved earlier.
		runCopilotAPICompaction("conv-unproved", "carry on")

		assert.Zero(t, server.callCount("session.history.compact"),
			"a compaction was sent to a port whose ownership could not be proved")
		assert.Zero(t, server.callCount("session.send"),
			"the follow-up must not be sent over a connection we just refused to "+
				"compact on — it is different bytes to the same stranger")
	})

	t.Run("compacts and follows up when ownership holds", func(t *testing.T) {
		server := newFakeCopilotServer(t)
		client := dialFakeCopilot(t, server)

		require.True(t, portowner.ProcessOwnsLoopbackPort(os.Getpid(), server.port()),
			"precondition: this test process owns the fake server's listener")
		copilotAPISessions.Adopt(&copilotAPISession{
			ConvID: "conv-owned", SessionID: "sess-1", Client: client,
			Port: server.port(), PanePID: os.Getpid(),
		})
		t.Cleanup(func() { copilotAPISessions.Drop("conv-owned") })

		runCopilotAPICompaction("conv-owned", "carry on")

		assert.Equal(t, 1, server.callCount("session.history.compact"),
			"the refusal above must be the ownership check failing, not this "+
				"function declining to do anything")
		assert.Equal(t, 1, server.callCount("session.send"),
			"the follow-up must still be submitted after a compaction")
	})
}
