// Command transform applies the frozen TCL-930 census mechanically.
//
// The base-line manifest identifies a db.ResetForTest call in the exact
// pre-TCL-925 tree. The command maps that call to its ordinal among the same
// file's ResetForTest calls in the current tree, verifies that the current
// call is still a standalone statement, and inserts the package-appropriate
// cleanup immediately afterward. Exclusions are emitted to manifest.json but
// are never edited.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type input struct {
	File     string
	BaseLine int
	Owner    string
}

type entry struct {
	File         string `json:"file"`
	BaseLine     int    `json:"baseLine"`
	Owner        string `json:"owner"`
	ResetOrdinal int    `json:"resetOrdinal"`
	Bucket       string `json:"bucket"`
	Action       string `json:"action"`
	Reason       string `json:"reason"`
}

type call struct {
	line, start, end int
}

var exclusions = map[string]string{
	"pkg/claude/agent/permissions_daemon_test.go:24":               "daemon stubs only; the executable test path never opens SQLite",
	"pkg/claude/agent/permissions_display_test.go:22":              "DB-free renderer; the executable test path never opens SQLite",
	"pkg/claude/session/exit_callback_real_tmux_test.go:365":       "short-lived helper subprocess owns the handle and exits before the parent removes its TempDir",
	"pkg/claude/session/sandbox_bwrap_smoke_linux_test.go:826":     "SQLite lives in the child agentd, which the returned cleanup cancels and awaits before parent TempDir removal",
	"pkg/claude/session/sandbox_bwrap_smoke_linux_test.go:849":     "parent Reset does not own the child agentd handle; child process teardown precedes TempDir removal",
	"pkg/claude/session/sandbox_seatbelt_smoke_darwin_test.go:959": "Darwin parity: SQLite lives in the awaited child agentd, not the parent test process",
	"pkg/claude/session/sandbox_seatbelt_smoke_darwin_test.go:982": "Darwin parity: parent Reset does not own the awaited child agentd handle",
}

func resetCalls(path string) ([]call, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var out []call
	ast.Inspect(f, func(n ast.Node) bool {
		es, ok := n.(*ast.ExprStmt)
		if !ok {
			return true
		}
		ce, ok := es.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, okID := sel.X.(*ast.Ident)
		if ok && okID && id.Name == "db" && sel.Sel.Name == "ResetForTest" && len(ce.Args) == 0 {
			out = append(out, call{fset.Position(es.Pos()).Line, fset.Position(es.Pos()).Offset, fset.Position(es.End()).Offset})
		}
		return true
	})
	return out, nil
}

func main() {
	base := flag.String("base", "", "exact pre-TCL-925 source tree")
	root := flag.String("root", ".", "current source tree")
	apply := flag.Bool("apply", false, "apply instance edits")
	verify := flag.Bool("verify", false, "verify every instance has its declared cleanup")
	flag.Parse()
	if *base == "" {
		panic("-base is required")
	}

	f, err := os.Open(filepath.Join(*root, ".tcl-930-migration/input.tsv"))
	if err != nil {
		panic(err)
	}
	defer f.Close()
	var inputs []input
	s := bufio.NewScanner(f)
	for s.Scan() {
		parts := strings.Split(s.Text(), "\t")
		if len(parts) != 3 {
			panic("bad input row: " + s.Text())
		}
		line, err := strconv.Atoi(parts[1])
		if err != nil {
			panic(err)
		}
		inputs = append(inputs, input{parts[0], line, parts[2]})
	}
	if err := s.Err(); err != nil {
		panic(err)
	}

	byFile := map[string][]entry{}
	manifest := make([]entry, 0, len(inputs))
	for _, in := range inputs {
		baseCalls, err := resetCalls(filepath.Join(*base, in.File))
		if err != nil {
			panic(err)
		}
		ordinal := -1
		for i, c := range baseCalls {
			if c.line == in.BaseLine {
				ordinal = i
				break
			}
		}
		if ordinal < 0 {
			panic(fmt.Sprintf("%s:%d: no ResetForTest call", in.File, in.BaseLine))
		}
		key := fmt.Sprintf("%s:%d", in.File, in.BaseLine)
		e := entry{File: in.File, BaseLine: in.BaseLine, Owner: in.Owner, ResetOrdinal: ordinal}
		if reason, ok := exclusions[key]; ok {
			e.Bucket, e.Action, e.Reason = "exclusion", "none", reason
		} else {
			e.Bucket = "instance"
			e.Reason = "runtime trace reaches db.Open; owning TempDir otherwise removes an open SQLite database"
			if strings.HasPrefix(in.File, "pkg/claude/agentd/") {
				e.Action = "cleanupAgentdTestDB(t)"
			} else {
				e.Action = "t.Cleanup(db.ResetForTest)"
			}
			byFile[in.File] = append(byFile[in.File], e)
		}
		manifest = append(manifest, e)
	}
	if len(manifest) != 57 {
		panic(fmt.Sprintf("got %d manifest rows, want 57", len(manifest)))
	}
	instances, excluded := 0, 0
	for _, e := range manifest {
		if e.Bucket == "instance" {
			instances++
		} else {
			excluded++
		}
	}
	if instances != 50 || excluded != 7 {
		panic(fmt.Sprintf("got %d/%d, want 50/7", instances, excluded))
	}

	if *apply {
		for file, entries := range byFile {
			path := filepath.Join(*root, file)
			src, err := os.ReadFile(path)
			if err != nil {
				panic(err)
			}
			calls, err := resetCalls(path)
			if err != nil {
				panic(err)
			}
			type insertion struct {
				at   int
				text string
			}
			var insertions []insertion
			for _, e := range entries {
				if e.ResetOrdinal >= len(calls) {
					panic(fmt.Sprintf("%s ordinal %d absent", file, e.ResetOrdinal))
				}
				c := calls[e.ResetOrdinal]
				lineStart := strings.LastIndex(string(src[:c.start]), "\n") + 1
				indent := string(src[lineStart:c.start])
				if strings.TrimSpace(indent) != "" {
					panic(fmt.Sprintf("%s:%d reset is not a standalone indented statement", file, c.line))
				}
				insertions = append(insertions, insertion{c.end, "\n" + indent + e.Action})
			}
			sort.Slice(insertions, func(i, j int) bool { return insertions[i].at > insertions[j].at })
			for _, ins := range insertions {
				src = append(src[:ins.at], append([]byte(ins.text), src[ins.at:]...)...)
			}
			if err := os.WriteFile(path, src, 0o644); err != nil {
				panic(err)
			}
		}
	}
	if *verify {
		unresolved := 0
		for file, entries := range byFile {
			path := filepath.Join(*root, file)
			src, err := os.ReadFile(path)
			if err != nil {
				panic(err)
			}
			calls, err := resetCalls(path)
			if err != nil {
				panic(err)
			}
			for _, e := range entries {
				if e.ResetOrdinal >= len(calls) {
					panic(fmt.Sprintf("%s ordinal %d absent", file, e.ResetOrdinal))
				}
				tail := strings.TrimLeft(string(src[calls[e.ResetOrdinal].end:]), " \t\r\n")
				if !strings.HasPrefix(tail, e.Action) {
					fmt.Printf("unresolved: %s:%d %s\n", e.File, e.BaseLine, e.Owner)
					unresolved++
				}
			}
		}
		fmt.Printf("post-transform census: %d definite, %d exclusions, 0 undetermined\n", unresolved, excluded)
		if unresolved != 0 {
			os.Exit(1)
		}
	}

	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		panic(err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(filepath.Join(*root, ".tcl-930-migration/manifest.json"), out, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("manifest: %d instances, %d exclusions, 0 undetermined\n", instances, excluded)
}
