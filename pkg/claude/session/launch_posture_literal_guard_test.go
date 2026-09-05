//ci:whole-tree
//
// This guard parses every production file in the module, not only what
// session imports, so PR CI's package selection cannot tell whether a
// change reached it. The marker runs it on every change.

package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every LaunchPosture literal must NAME every field, and the reason is TCL-1076
// rather than tidiness.
//
// An omitted field in a Go composite literal is that field's zero, and
// RecordLaunchPosture writes what it is given. So an omitted field is not an
// abstention — it is an assertion of a default, straight onto the conversation's
// durable record. That is exactly how `tclaude conv resume` came to write
// `copilot_api=false` over a conversation that had chosen the Copilot API drive
// (since TCL-1058 the record that decides whether a message travels over RPC or
// is TYPED into the pane) and `context_window_max=0` over a configured meter
// denominator: nobody wrote those values, and nobody had to.
//
// A behavioural test cannot cover this, because the failure is a field nobody
// thought about — including the author of the next relaunch surface, who will
// copy an existing literal that was complete on the day it was copied. Driving
// the expected field set off the struct by REFLECTION is what makes the guard
// survive that: adding a field to LaunchPosture fails every literal that does
// not mention it, at compile-time-adjacent cost, with the diff pointing at the
// decision.
//
// Scope: non-test files only. The bug class is a production relaunch surface
// asserting a posture it never resolved; a test literal cannot ship that, and
// forcing exhaustiveness on fixtures would buy noise instead of protection. Said
// out loud because an unexplained exclusion in a structural guard is precisely
// what a review should distrust.
//
// # What it catches, and what it does not
//
// A cold review probed this guard with the idioms a future change would plausibly
// use. It catches the keyed literal in every spelling that matters — bare
// `LaunchPosture{…}`, qualified `session.LaunchPosture{…}`, `&session.LaunchPosture{…}`
// — in every package under the module root, and it catches a positional literal.
// Two further shapes are caught by the extra checks below rather than by the
// literal walk: an uninitialised `var p session.LaunchPosture` (which asserts
// every zero and names nothing), and a type alias or defined type over
// LaunchPosture (which would otherwise let a literal dodge the type match).
//
// It does NOT catch two shapes, and saying so is the point of this paragraph — an
// unstated gap is how a guard becomes a certificate:
//
//   - copy-then-modify: `p := somePosture; p.CopilotAPI = nil`. Nothing here can
//     tell that from any other struct copy without type information.
//   - construction by conversion from another struct type.
//
// Both would need a types-based pass (go/packages), which costs a full build per
// run. The behavioural tests in pkg/claude/conv are what cover the semantics for
// the surfaces that exist; this guard covers the shape of new ones.
func TestEveryLaunchPostureLiteralNamesEveryField(t *testing.T) {
	root := repoRootForLiteralGuard(t)
	want := launchPostureFieldNames()
	require.NotEmpty(t, want)

	type foundLiteral struct {
		file     string
		line     int
		missing  []string
		unkeyed  bool
		declared bool // an uninitialised `var p LaunchPosture` rather than a literal
	}
	var literals []foundLiteral
	var aliases []string

	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "testdata", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, source, 0)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CompositeLit:
				if !isLaunchPostureType(typed.Type) {
					return true
				}
				named, unkeyed := launchPostureLiteralFields(typed)
				var missing []string
				for _, field := range want {
					if !slices.Contains(named, field) {
						missing = append(missing, field)
					}
				}
				literals = append(literals, foundLiteral{
					file: rel, line: fset.Position(typed.Lbrace).Line,
					missing: missing, unkeyed: unkeyed,
				})
			case *ast.ValueSpec:
				// `var p session.LaunchPosture` with no value asserts every zero and
				// names nothing — the same bug as an incomplete literal, wearing a
				// different syntax, and the shape a surface reaches for when it wants
				// to fill in a few fields later. Reported as a literal missing
				// everything, which is exactly what it is.
				if len(typed.Values) > 0 || !isLaunchPostureType(typed.Type) {
					return true
				}
				literals = append(literals, foundLiteral{
					file: rel, line: fset.Position(typed.Pos()).Line, missing: want, declared: true,
				})
			case *ast.TypeSpec:
				// An alias or defined type over LaunchPosture would let a literal of
				// the new name slip past isLaunchPostureType entirely.
				if isLaunchPostureType(typed.Type) {
					aliases = append(aliases, rel+":"+typed.Name.Name)
				}
			}
			return true
		})
		return nil
	}))

	assert.Emptyf(t, aliases, "LaunchPosture is aliased or redefined at %v. A literal of the new "+
		"name is invisible to this guard, so the alias has to go or this guard has to learn "+
		"about it", aliases)

	// The positive control, and it has to be here: every assertion below is
	// satisfied by finding NOTHING — which is what a renamed type, a moved
	// package, or a walk rooted in the wrong directory produces. These two files
	// are the production relaunch surfaces that write the record today.
	files := make([]string, 0, len(literals))
	for _, lit := range literals {
		files = append(files, lit.file)
	}
	for _, expected := range []string{
		filepath.Join("pkg", "claude", "session", "new.go"),
		filepath.Join("pkg", "claude", "conv", "watch.go"),
	} {
		assert.Containsf(t, files, expected,
			"no LaunchPosture literal found in %s. Either it moved — in which case this "+
				"guard is now watching nothing and must be re-pointed — or the record is no "+
				"longer written from there. Found: %v", expected, files)
	}

	for _, lit := range literals {
		assert.Falsef(t, lit.unkeyed,
			"%s:%d builds a LaunchPosture positionally. Field order is not a decision anyone "+
				"reviews; name the fields", lit.file, lit.line)
		assert.Falsef(t, lit.declared,
			"%s:%d declares an uninitialised LaunchPosture. Every field is then its zero, "+
				"asserted onto the conversation's durable record, with nothing at the site "+
				"saying so. Build it as a keyed literal", lit.file, lit.line)
		if lit.declared {
			continue
		}
		assert.Emptyf(t, lit.missing,
			"%s:%d omits LaunchPosture field(s) %v. An omitted field is not an abstention: it "+
				"is this launch ASSERTING that field's zero onto the conversation's durable "+
				"record, which is how a plain-CLI resume erased a conversation's chosen "+
				"Copilot drive (TCL-1076). Name every field — pass nil for one this surface "+
				"does not resolve, or the resolved value if it does",
			lit.file, lit.line, lit.missing)
	}
}

// The guard's own positive control: a literal that omits a field must be
// REPORTED, and a literal that names everything must not be. Without this the
// test above passes for a checker that never finds a missing field, which is the
// vacuously-green shape this series keeps meeting.
func TestLaunchPostureLiteralCheckerReportsAnIncompleteLiteral(t *testing.T) {
	const complete = `package p

import "github.com/tofutools/tclaude/pkg/claude/session"

func f() session.LaunchPosture {
	return session.LaunchPosture{
		AutoMemory:        true,
		PeerMessaging:     false,
		ContextFeatures:   nil,
		AutoCompactWindow: "",
		FastMode:          "",
		RemoteControl:     false,
		ContextWindowMax:  nil,
		CopilotAPI:        nil,
		CodexAppServer:    nil,
	}
}
`
	const incomplete = `package p

import "github.com/tofutools/tclaude/pkg/claude/session"

func f() session.LaunchPosture {
	return session.LaunchPosture{AutoMemory: true}
}
`
	const positional = `package p

func f() LaunchPosture {
	return LaunchPosture{true, false, nil, "", "", false, nil, nil, nil}
}
`
	want := launchPostureFieldNames()

	missing, unkeyed := checkLaunchPostureSource(t, complete)
	assert.Empty(t, missing, "a literal naming every field must not be reported")
	assert.False(t, unkeyed)

	missing, unkeyed = checkLaunchPostureSource(t, incomplete)
	assert.False(t, unkeyed)
	assert.Len(t, missing, len(want)-1,
		"every field but the one named must be reported missing, or the checker is not "+
			"actually comparing against the struct")
	assert.Contains(t, missing, "CopilotAPI",
		"the field whose omission caused TCL-1076 must be among them")

	_, unkeyed = checkLaunchPostureSource(t, positional)
	assert.True(t, unkeyed, "a positional literal must be reported: it names no decision at all")
}

// The two shapes the literal walk alone would miss, each driven through the same
// matchers the scan uses. Both were found by probing the guard rather than by
// reading it, which is the only way this kind of blind spot surfaces.
func TestLaunchPostureGuardCatchesTheNonLiteralShapes(t *testing.T) {
	t.Run("an uninitialised declaration asserts every zero", func(t *testing.T) {
		const source = `package p

import "github.com/tofutools/tclaude/pkg/claude/session"

func f() session.LaunchPosture {
	var p session.LaunchPosture
	return p
}
`
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, "probe.go", source, 0)
		require.NoError(t, err)
		found := false
		ast.Inspect(parsed, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok || len(spec.Values) > 0 || !isLaunchPostureType(spec.Type) {
				return true
			}
			found = true
			return true
		})
		assert.True(t, found,
			"`var p session.LaunchPosture` must be reported: every field is its zero and the "+
				"site names none of them")
	})

	t.Run("an alias would hide a literal from the type match", func(t *testing.T) {
		for _, source := range []string{
			"package p\n\ntype posture = LaunchPosture\n",
			"package p\n\ntype posture LaunchPosture\n",
		} {
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, "probe.go", source, 0)
			require.NoError(t, err)
			found := false
			ast.Inspect(parsed, func(node ast.Node) bool {
				spec, ok := node.(*ast.TypeSpec)
				if ok && isLaunchPostureType(spec.Type) {
					found = true
				}
				return true
			})
			assert.True(t, found, "an alias/defined type over LaunchPosture must be reported")
		}
	})
}

// checkLaunchPostureSource runs the same two checks the walk above runs, over
// one in-memory file, so the checker itself can be shown to fail.
func checkLaunchPostureSource(t *testing.T, source string) (missing []string, unkeyed bool) {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "guard_probe.go", source, 0)
	require.NoError(t, err)
	want := launchPostureFieldNames()
	found := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if !ok || !isLaunchPostureType(lit.Type) {
			return true
		}
		found = true
		named, positional := launchPostureLiteralFields(lit)
		if positional {
			unkeyed = true
		}
		for _, field := range want {
			if !slices.Contains(named, field) {
				missing = append(missing, field)
			}
		}
		return true
	})
	require.True(t, found, "the probe source must contain a LaunchPosture literal")
	sort.Strings(missing)
	return missing, unkeyed
}

// isLaunchPostureType matches both spellings a literal can take: `LaunchPosture`
// inside this package and `session.LaunchPosture` outside it. Matching on the
// selector's name rather than a resolved type is deliberate — the guard parses
// files rather than type-checking them, and a `session` alias for another
// package holding a LaunchPosture type is not a shape this repo has.
func isLaunchPostureType(expr ast.Expr) bool {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name == "LaunchPosture"
	case *ast.SelectorExpr:
		return typed.Sel != nil && typed.Sel.Name == "LaunchPosture"
	}
	return false
}

// launchPostureLiteralFields reports the field names a literal names, and
// whether it was built positionally (in which case it names none).
func launchPostureLiteralFields(lit *ast.CompositeLit) (named []string, unkeyed bool) {
	for _, element := range lit.Elts {
		kv, ok := element.(*ast.KeyValueExpr)
		if !ok {
			unkeyed = true
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			unkeyed = true
			continue
		}
		named = append(named, key.Name)
	}
	return named, unkeyed
}

// launchPostureFieldNames is the expected set, taken from the struct itself so a
// field added to LaunchPosture cannot be silently left out of every literal.
func launchPostureFieldNames() []string {
	typ := reflect.TypeOf(LaunchPosture{})
	names := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		names = append(names, typ.Field(i).Name)
	}
	sort.Strings(names)
	return names
}

// repoRootForLiteralGuard walks up from the package directory to the module
// root, so the guard covers every package that can build a LaunchPosture rather
// than only this one.
func repoRootForLiteralGuard(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for range 10 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "walked past the filesystem root without finding go.mod")
		dir = parent
	}
	t.Fatal("could not locate the module root above the package directory")
	return ""
}
