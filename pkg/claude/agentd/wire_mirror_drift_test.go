package agentd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The CLI (pkg/claude/agent) keeps its own copies of some daemon wire structs
// rather than importing them: they differ on purpose in a few field types,
// and sharing them would drag daemon types into the client. A copy that lacks
// a field silently drops it, and where the CLI sends a value back as a full
// replace that deletes data: tcl-cnua7d, where `templates edit --file` wiped
// a template's owner_scopes. This guard makes that drift a test failure. Each
// mirror has to carry every JSON field of its daemon struct, and the reverse,
// unless the field is listed below with the reason it differs.

type wireMirror struct {
	daemon, cli string
	// cliOmits are daemon fields the CLI copy deliberately leaves out.
	cliOmits map[string]string
	// cliAdds are CLI fields the daemon struct does not have.
	cliAdds map[string]string
}

var wireMirrors = []wireMirror{
	{daemon: "templateJSON", cli: "templateJSON"},
	{daemon: "templateAgentJSON", cli: "templateAgentJSON"},
	{daemon: "rhythmJSON", cli: "rhythmJSON"},
	{daemon: "workPatternEntryJSON", cli: "workPatternEntryJSON"},
	{daemon: "processPhaseJSON", cli: "processPhaseJSON"},
	{daemon: "jobJSON", cli: "cronJobJSON", cliOmits: map[string]string{
		"operator_authored": "the CLI only lists and shows jobs, and never sends a job back; " +
			"whether a job is operator-authored is the daemon's trust decision, not something to display",
	}},
}

func TestCLIWireMirrorsCarryEveryDaemonField(t *testing.T) {
	daemon := jsonFieldsByStruct(t, ".")
	cli := jsonFieldsByStruct(t, filepath.Join("..", "agent"))
	for _, m := range wireMirrors {
		t.Run(m.cli, func(t *testing.T) {
			d, ok := daemon[m.daemon]
			if !ok {
				t.Fatalf("no struct %s in pkg/claude/agentd; update wireMirrors", m.daemon)
			}
			c, ok := cli[m.cli]
			if !ok {
				t.Fatalf("no struct %s in pkg/claude/agent; update wireMirrors", m.cli)
			}
			for _, f := range d {
				if !slices.Contains(c, f) && m.cliOmits[f] == "" {
					t.Errorf("pkg/claude/agent %s lacks %q from the daemon's %s: add it, or list it in cliOmits with the reason", m.cli, f, m.daemon)
				}
			}
			for _, f := range c {
				if !slices.Contains(d, f) && m.cliAdds[f] == "" {
					t.Errorf("pkg/claude/agent %s has %q, which the daemon's %s does not: drop it, or list it in cliAdds with the reason", m.cli, f, m.daemon)
				}
			}
			for f := range m.cliOmits {
				if slices.Contains(c, f) {
					t.Errorf("%q is listed in cliOmits but %s now carries it: drop the entry", f, m.cli)
				}
			}
			for f := range m.cliAdds {
				if slices.Contains(d, f) {
					t.Errorf("%q is listed in cliAdds but the daemon's %s now has it: drop the entry", f, m.daemon)
				}
			}
		})
	}
}

// jsonFieldsByStruct reads the non-test Go files in dir and returns each
// struct type's JSON field names.
func jsonFieldsByStruct(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				out[ts.Name.Name] = structJSONFields(t, ts.Name.Name, st)
			}
		}
	}
	return out
}

func structJSONFields(t *testing.T, typeName string, st *ast.StructType) []string {
	t.Helper()
	var fields []string
	for _, field := range st.Fields.List {
		tag := ""
		if field.Tag != nil {
			raw, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				t.Fatalf("%s: tag %s: %v", typeName, field.Tag.Value, err)
			}
			tag, _, _ = strings.Cut(reflect.StructTag(raw).Get("json"), ",")
		}
		if tag == "-" {
			continue
		}
		if len(field.Names) == 0 {
			// An embedded struct's fields would need resolving; no mirrored
			// struct embeds one, so only the mirrors fail on it.
			fields = append(fields, "<embedded "+typeName+">")
			continue
		}
		for _, n := range field.Names {
			if tag != "" {
				fields = append(fields, tag)
			} else if n.IsExported() {
				fields = append(fields, n.Name)
			}
		}
	}
	return fields
}
