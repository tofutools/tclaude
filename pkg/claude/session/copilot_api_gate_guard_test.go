package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Structural guards for TCL-1084's four wiring facts. Each asserts the property
// that actually broke rather than the one that was easiest to express — the
// lesson from TCL-1076's cold review, where a guard that only checked "the gate
// is mentioned" let `_, _ = gate(…, true)` through with the whole package green.
//
// What these CANNOT see, stated here rather than left for the next reviewer:
//
//   - Source order is not control flow. A gate sitting inside a branch that is
//     never taken satisfies every ordering check below. The behavioural table in
//     copilot_api_daemon_test.go is what covers semantics.
//   - They are scoped to the functions named below. A launch path added in a new
//     function is not enumerated here, because the honest enumeration for THAT
//     would be "no launch renders --ui-server without the daemon" — and
//     `--ui-server` is rendered in pkg/claude/harness from a SpawnSpec field, one
//     package away from anything this file can see.

func guardParse(t *testing.T, filename, src string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	var (
		file *ast.File
		err  error
	)
	if src == "" {
		file, err = parser.ParseFile(fset, filename, nil, parser.SkipObjectResolution)
	} else {
		file, err = parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	}
	require.NoError(t, err, "parse %s", filename)
	return file
}

func guardFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name != nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// calleeIdent names the function a call expression targets, for both `f(x)` and
// `pkg.F(x)`. Both spellings must be recognised or a guard is beaten by moving
// the callee behind a package qualifier.
func calleeIdent(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		if fn.Sel != nil {
			return fn.Sel.Name
		}
	}
	return ""
}

// callOrder returns the source position of the first call to each named callee
// inside fn, and whether it was found at all.
func callOrder(fn *ast.FuncDecl, names ...string) map[string]token.Pos {
	found := map[string]token.Pos{}
	if fn == nil {
		return found
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calleeIdent(call)
		for _, want := range names {
			if name == want {
				if _, seen := found[want]; !seen {
					found[want] = call.Lparen
				}
			}
		}
		return true
	})
	return found
}

// errorResultIsBound reports whether every assignment whose right-hand side calls
// callee binds the LAST result — the error — to something other than `_`.
//
// It checks the last result specifically, not "any non-blank result", because the
// weaker version is what the cold review beat: `drive, notice, _ := gate(...)`
// satisfies "some result is bound" while discarding the refusal, and errcheck does
// not catch it either (check-blank is off in .golangci.yml). Every refusal would
// become a silent send-keys downgrade — exactly what assert-and-refuse exists to
// prevent — while this guard's own comment claimed the refusal was bound.
func errorResultIsBound(fn *ast.FuncDecl, callee string) bool {
	bound := false
	ast.Inspect(fn, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) == 0 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || calleeIdent(call) != callee {
			return true
		}
		last, ok := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident)
		if !ok || last.Name != "_" {
			bound = true
		}
		return true
	})
	return bound
}

// callIsBareStatement reports whether callee is called as a statement whose result
// is checked by an enclosing if — `if err := f(x); err != nil`. The applier returns
// only an error, so that is its idiomatic shape and the assignment walker above
// would not see it.
func callIsBareStatement(fn *ast.FuncDecl, callee string) bool {
	checked := false
	ast.Inspect(fn, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || stmt.Init == nil {
			return true
		}
		assign, ok := stmt.Init.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && calleeIdent(call) == callee {
			if stmt.Cond != nil {
				checked = true
			}
		}
		return true
	})
	return checked
}

// The headline wiring fact: the drive gate runs, its error is bound, and it runs
// BEFORE any port is resolved. The ordering is not cosmetic — a launch that is
// about to be refused must not bind and release a loopback port on the way, which
// is the pointless exposure the ticket is about.
func TestRunNewGatesTheDriveBeforeResolvingAPort(t *testing.T) {
	fn := guardFunc(guardParse(t, "new.go", ""), "runNew")
	require.NotNil(t, fn, "runNew must exist for this guard to mean anything")

	order := callOrder(fn, "applyCopilotAPIDriveDecision", "ResolveCopilotAPIPort")
	gate, gated := order["applyCopilotAPIDriveDecision"]
	port, resolved := order["ResolveCopilotAPIPort"]

	require.True(t, gated,
		"runNew must consult applyCopilotAPIDriveDecision: without it a hand-typed "+
			"--copilot-api binds an unauthenticated loopback endpoint nothing will dial")
	require.True(t, resolved, "runNew must still resolve the port for the agentd case")
	assert.Less(t, int(gate), int(port),
		"the gate must precede the port resolution, or a launch that is about to be "+
			"refused allocates a port first")
	assert.True(t,
		callIsBareStatement(fn, "applyCopilotAPIDriveDecision") ||
			errorResultIsBound(fn, "applyCopilotAPIDriveDecision"),
		"the refusal must be CHECKED, not discarded: a discarded error turns every "+
			"refusal into a silent send-keys downgrade, and errcheck does not catch a "+
			"blank assignment (check-blank is off)")
}

// The carryover must record which conversation an inferred drive came from, or the
// gate cannot tell an asserted drive from an inherited one and the whole
// assert-and-refuse / infer-and-disclose split collapses into one policy.
func TestTheCarryoverNotesAnInferredDrive(t *testing.T) {
	fn := guardFunc(guardParse(t, "relaunch_carryover.go", ""), "applyRecordedLaunchPosture")
	require.NotNil(t, fn)
	ok, why := noteCarriedDriveArgIsTheCarriedList(fn)
	assert.True(t, ok,
		"applyRecordedLaunchPosture must record a carried drive's origin from the list the "+
			"carry loop filled; without it every carried drive looks asserted and is refused "+
			"instead of disclosed: %s", why)
}

// The posture write must go through copilotAPIPostureToRecord rather than taking
// the address of the resolved value directly. This is the guard against a
// "simplification": `CopilotAPI: &params.CopilotAPI` is shorter, obviously
// correct-looking, and silently re-introduces a non-daemon launch authoring
// `false` over a conversation's recorded drive.
func TestThePostureWriteRoutesTheDriveThroughItsOwnDecision(t *testing.T) {
	file := guardParse(t, "new.go", "")
	viaHelper, direct := launchPostureCopilotAPIValues(file)
	require.Positive(t, viaHelper, "new.go must still build a LaunchPosture literal")
	assert.Zero(t, direct,
		"EVERY LaunchPosture.CopilotAPI must be copilotAPIPostureToRecord(params): taking "+
			"&params.CopilotAPI directly lets a launch that could not honour a carried "+
			"drive un-choose it for the conversation, and one compliant literal elsewhere "+
			"in the file does not make another one safe")
}

// launchPostureCopilotAPIValues reports, for every LaunchPosture literal that sets
// CopilotAPI, whether that value routes through copilotAPIPostureToRecord.
//
// Every literal, not "any literal". The cold review beat the OR version by leaving
// the real write as `&params.CopilotAPI` and adding a decoy literal elsewhere in the
// file that called the helper: one true disjunct, guard green, erasure restored.
func launchPostureCopilotAPIValues(file *ast.File) (viaHelper, direct int) {
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		name, ok := lit.Type.(*ast.Ident)
		if !ok || name.Name != "LaunchPosture" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "CopilotAPI" {
				continue
			}
			if call, ok := kv.Value.(*ast.CallExpr); ok &&
				calleeIdent(call) == "copilotAPIPostureToRecord" {
				viaHelper++
				continue
			}
			direct++
		}
		return true
	})
	return viaHelper, direct
}

// noteCarriedDriveArgIsTheCarriedList reports whether applyRecordedLaunchPosture
// passes noteCarriedCopilotDrive the list the carry loop actually appends to, after
// that loop has run.
//
// Asserting the CALL exists was not enough, and the cold review showed why by
// moving it above the loop — a plausible "group the setup together" tidy-up. The
// list is empty there, so the marker is never set, every carried drive classifies
// as ASSERTED, and a plain `session new -r <api-conv>` becomes a hard refusal
// instead of a disclosure. That is the false-refusal direction, the one thing this
// change must not do. `note(params, nil, convID)` and `note(params, carried, "")`
// are the same beat, so the arguments are checked too.
func noteCarriedDriveArgIsTheCarriedList(fn *ast.FuncDecl) (ok bool, why string) {
	if fn == nil {
		return false, "applyRecordedLaunchPosture not found"
	}
	var appended string
	var loopEnd token.Pos
	ast.Inspect(fn, func(n ast.Node) bool {
		rng, isRange := n.(*ast.RangeStmt)
		if !isRange {
			return true
		}
		ast.Inspect(rng.Body, func(inner ast.Node) bool {
			assign, isAssign := inner.(*ast.AssignStmt)
			if !isAssign || len(assign.Rhs) != 1 || len(assign.Lhs) != 1 {
				return true
			}
			call, isCall := assign.Rhs[0].(*ast.CallExpr)
			if !isCall || calleeIdent(call) != "append" {
				return true
			}
			if target, isIdent := assign.Lhs[0].(*ast.Ident); isIdent {
				appended = target.Name
				loopEnd = rng.End()
			}
			return true
		})
		return true
	})
	if appended == "" {
		return false, "no `x = append(x, ...)` found inside the carry loop"
	}
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall || calleeIdent(call) != "noteCarriedCopilotDrive" || len(call.Args) != 3 {
			return true
		}
		found = true
		if call.Lparen < loopEnd {
			why = "noteCarriedCopilotDrive is called BEFORE the carry loop finishes, " +
				"so the list it reads is still empty and no marker is ever set"
			return false
		}
		list, isIdent := call.Args[1].(*ast.Ident)
		if !isIdent || list.Name != appended {
			why = "noteCarriedCopilotDrive is not passed the list the loop appends to (" +
				appended + ")"
			return false
		}
		if convArg, isIdent := call.Args[2].(*ast.Ident); !isIdent || convArg.Name == "_" {
			why = "noteCarriedCopilotDrive's conversation argument is not an identifier"
			return false
		}
		return true
	})
	if !found {
		return false, "applyRecordedLaunchPosture never calls noteCarriedCopilotDrive"
	}
	return why == "", why
}

// The property that actually broke: ResolveCopilotAPIPort INVENTED a port. It
// bound 127.0.0.1:0, kept the number and closed the listener, so a hand-typed
// `--copilot-api` got a real endpoint that nothing would ever dial.
//
// Scoped to the function rather than the file or the package on purpose. The
// package legitimately calls net.Listen in several places — the Darwin route
// slots, the filtered-network gateway, the proxy bridge — so a package-wide ban
// would be a false statement, and a file-wide one is beaten by moving the code.
// What must hold is narrower and exactly the defect: this function returns a port
// it was GIVEN, never one it made.
func TestResolveCopilotAPIPortNeverManufacturesAPort(t *testing.T) {
	fn := guardFunc(guardParse(t, "copilot_api_port.go", ""), "ResolveCopilotAPIPort")
	require.NotNil(t, fn, "ResolveCopilotAPIPort must exist for this guard to mean anything")
	assert.Empty(t, netCallsIn(fn),
		"ResolveCopilotAPIPort must not create a listener: the drive's port comes from "+
			"the daemon that will dial it, and a locally chosen one binds an "+
			"unauthenticated endpoint with nothing behind it (TCL-1084)")
}

// netCallsIn lists calls on the `net` package inside fn.
func netCallsIn(fn *ast.FuncDecl) []string {
	var calls []string
	if fn == nil {
		return calls
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "net" && sel.Sel != nil {
			calls = append(calls, "net."+sel.Sel.Name)
		}
		return true
	})
	return calls
}

// Every checker above, driven against the shapes a future change would plausibly
// use — including verbatim the edit that beat TCL-1076's first guard. A guard
// whose failure mode is never exercised is a guard that has never been shown to
// fail, which is the difference between a check and a certificate.
func TestCopilotAPIGateGuardsCatchEachMutation(t *testing.T) {
	t.Run("a gate whose results are all discarded is reported", func(t *testing.T) {
		file := guardParse(t, "probe.go", `package session
func runNew(params *NewParams) error {
	_, _, _ = resolveCopilotAPIDriveForLaunch(copilotAPIDriveRequest{})
	port, err := ResolveCopilotAPIPort(true, 1)
	_, _ = port, err
	return nil
}`)
		fn := guardFunc(file, "runNew")
		require.NotNil(t, fn)
		assert.False(t, errorResultIsBound(fn, "resolveCopilotAPIDriveForLaunch"),
			"discarding every result must be reported: this is the edit that beat the "+
				"first version of TCL-1076's guard")
	})

	t.Run("a gate called after the port is resolved is reported", func(t *testing.T) {
		file := guardParse(t, "probe.go", `package session
func runNew(params *NewParams) error {
	port, err := ResolveCopilotAPIPort(true, 0)
	if err != nil {
		return err
	}
	drive, notice, err := resolveCopilotAPIDriveForLaunch(copilotAPIDriveRequest{})
	_, _, _ = drive, notice, port
	return err
}`)
		order := callOrder(guardFunc(file, "runNew"),
			"resolveCopilotAPIDriveForLaunch", "ResolveCopilotAPIPort")
		assert.Greater(t, int(order["resolveCopilotAPIDriveForLaunch"]),
			int(order["ResolveCopilotAPIPort"]),
			"the checker must be able to see the wrong order, or asserting the right "+
				"one proves nothing")
	})

	t.Run("a missing gate is reported", func(t *testing.T) {
		file := guardParse(t, "probe.go", `package session
func runNew(params *NewParams) error {
	_, err := ResolveCopilotAPIPort(true, 0)
	return err
}`)
		order := callOrder(guardFunc(file, "runNew"), "resolveCopilotAPIDriveForLaunch")
		_, gated := order["resolveCopilotAPIDriveForLaunch"]
		assert.False(t, gated, "an ungated launcher must be reported")
	})

	t.Run("a carryover that stops noting the origin is reported", func(t *testing.T) {
		file := guardParse(t, "probe.go", `package session
func applyRecordedLaunchPosture(params *NewParams, explicit explicitLaunchFields) error {
	return nil
}`)
		order := callOrder(guardFunc(file, "applyRecordedLaunchPosture"), "noteCarriedCopilotDrive")
		_, noted := order["noteCarriedCopilotDrive"]
		assert.False(t, noted, "a carryover that never notes the origin must be reported")
	})

	t.Run("the simplified posture write is reported", func(t *testing.T) {
		file := guardParse(t, "probe.go", `package session
func f(params *NewParams) {
	RecordLaunchPosture("s", nil, LaunchPosture{
		ContextWindowMax: &params.ContextWindowMax,
		CopilotAPI:       &params.CopilotAPI,
	})
}`)
		viaHelper, direct := launchPostureCopilotAPIValues(file)
		assert.Zero(t, viaHelper)
		assert.Equal(t, 1, direct,
			"&params.CopilotAPI must be reported: it is the shorter, plausible-looking "+
				"edit that re-introduces the record erasure")
	})

	t.Run("a compliant decoy literal does not excuse a direct write", func(t *testing.T) {
		// Verbatim the cold review's beat on the OR version of this checker: leave the
		// real write direct and add a literal elsewhere that calls the helper.
		file := guardParse(t, "probe.go", `package session
func f(params *NewParams) {
	RecordLaunchPosture("s", nil, LaunchPosture{
		CopilotAPI: &params.CopilotAPI,
	})
	_ = LaunchPosture{CopilotAPI: copilotAPIPostureToRecord(params)}
}`)
		viaHelper, direct := launchPostureCopilotAPIValues(file)
		assert.Equal(t, 1, viaHelper, "the decoy is counted")
		assert.Equal(t, 1, direct,
			"the direct write must still be reported: one compliant literal elsewhere in "+
				"the file does not make another one safe")
	})

	t.Run("a refusal discarded into a trailing blank is reported", func(t *testing.T) {
		// The cold review's beat on the previous "any non-blank result" check.
		file := guardParse(t, "probe.go", `package session
func runNew(params *NewParams) error {
	drive, notice, _ := resolveCopilotAPIDriveForLaunch(copilotAPIDriveRequest{})
	_, _ = drive, notice
	return nil
}`)
		fn := guardFunc(file, "runNew")
		require.NotNil(t, fn)
		assert.False(t, errorResultIsBound(fn, "resolveCopilotAPIDriveForLaunch"),
			"`drive, notice, _ :=` must be reported: it binds two results and throws the "+
				"refusal away, which the old any-non-blank check accepted")
		assert.False(t, callIsBareStatement(fn, "resolveCopilotAPIDriveForLaunch"))
	})

	t.Run("noting the drive before the carry loop is reported", func(t *testing.T) {
		// Verbatim the cold review's beat: a plausible "group the setup together" move
		// that leaves the list empty, so every carried drive is refused instead of
		// disclosed.
		file := guardParse(t, "probe.go", `package session
func applyRecordedLaunchPosture(params *NewParams, explicit explicitLaunchFields) error {
	carried := make([]string, 0, len(launchCarryoverFields))
	noteCarriedCopilotDrive(params, carried, convID)
	for _, field := range launchCarryoverFields {
		switch field.classify(field.carry(h, recorded, params)) {
		case carryApplied:
			carried = append(carried, "--"+field.flag)
		}
	}
	return nil
}`)
		ok, why := noteCarriedDriveArgIsTheCarriedList(guardFunc(file, "applyRecordedLaunchPosture"))
		assert.False(t, ok, "calling it before the loop must be reported")
		assert.Contains(t, why, "BEFORE the carry loop")
	})

	t.Run("noting the drive with a nil list is reported", func(t *testing.T) {
		file := guardParse(t, "probe.go", `package session
func applyRecordedLaunchPosture(params *NewParams, explicit explicitLaunchFields) error {
	carried := make([]string, 0, len(launchCarryoverFields))
	for _, field := range launchCarryoverFields {
		switch field.classify(field.carry(h, recorded, params)) {
		case carryApplied:
			carried = append(carried, "--"+field.flag)
		}
	}
	noteCarriedCopilotDrive(params, nil, convID)
	return nil
}`)
		ok, why := noteCarriedDriveArgIsTheCarriedList(guardFunc(file, "applyRecordedLaunchPosture"))
		assert.False(t, ok, "a nil list must be reported")
		assert.Contains(t, why, "not passed the list")
	})

	t.Run("a port allocation inside the resolver is reported", func(t *testing.T) {
		file := guardParse(t, "probe.go", `package session
func ResolveCopilotAPIPort(copilotAPI bool, requested int) (int, error) {
	if requested != 0 {
		return requested, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port, nil
}`)
		calls := netCallsIn(guardFunc(file, "ResolveCopilotAPIPort"))
		assert.NotEmpty(t, calls,
			"the pre-TCL-1084 body must be reported, or asserting its absence proves nothing")
		assert.Contains(t, strings.Join(calls, " "), "net.Listen")
	})
}

// A CONSEQUENCE of the gate rather than a target of it, pinned so it is a decision
// instead of a surprise: `session new --copilot-api --join-group <g>` is now
// refused, where before it launched and silently ignored the flag.
//
// It is refused because the gate (new.go:941) precedes the --join-group handoff
// (new.go:967), and a join-group launch is neither daemon-owned nor carrying. That
// is the right answer for this surface: agent.RunJoinGroup builds its SpawnRequest
// with Name/Role/Descr/Cwd/Effort/Model/Harness/TimeoutSeconds and then sets only
// FastMode and WriteProofToken — CopilotAPI is never forwarded — so the flag could
// not be honoured here even in principle. Refusing an assertion this surface
// discards beats launching while ignoring it.
//
// The ordering is what makes it so, so the ordering is what this asserts.
func TestTheDriveGatePrecedesTheJoinGroupHandoff(t *testing.T) {
	fn := guardFunc(guardParse(t, "new.go", ""), "runNew")
	require.NotNil(t, fn)
	order := callOrder(fn, "applyCopilotAPIDriveDecision", "JoinGroupHandler")
	gate, gated := order["applyCopilotAPIDriveDecision"]
	join, handed := order["JoinGroupHandler"]
	require.True(t, gated)
	require.True(t, handed, "runNew must still hand off --join-group")
	assert.Less(t, int(gate), int(join),
		"the gate must precede the join-group handoff: that surface drops --copilot-api "+
			"on the floor, so an explicit drive must be refused rather than ignored")
}
