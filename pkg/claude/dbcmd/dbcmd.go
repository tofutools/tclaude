// Package dbcmd implements `tclaude db` — inspection commands over the tclaude
// SQLite database. Its first subcommand, `db schema`, dumps the schema (CREATE
// statements by default; a structured --json form; a --relations identity
// audit) and underpins the conv-id -> agent_id FK migration work.
package dbcmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
)

// Params for the `db` group itself (no flags; it dispatches to subcommands).
type Params struct{}

// Cmd returns the `db` command group.
func Cmd() *cobra.Command {
	return boa.CmdT[Params]{
		Use:         "db",
		Short:       "Inspect the tclaude SQLite database",
		Long:        "Inspect the tclaude SQLite database (~/.tclaude/db.sqlite).",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			schemaCmd(),
		},
		RunFunc: func(params *Params, cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}.ToCobra()
}

// SchemaParams flags for `db schema`.
type SchemaParams struct {
	JSON      bool `long:"json" short:"j" help:"Output the structured schema as JSON (columns + foreign keys per table)"`
	Relations bool `long:"relations" help:"Print an identity audit: columns grouped by whether they key on conv-id or agent_id"`
}

func schemaCmd() *cobra.Command {
	return boa.CmdT[SchemaParams]{
		Use:   "schema",
		Short: "Dump the database schema",
		Long: "Dump the tclaude SQLite schema.\n\n" +
			"Default output is the canonical CREATE statements (the `.schema` equivalent),\n" +
			"with foreign-key REFERENCES shown inline. Use --json for a structured form\n" +
			"(per-table columns and foreign keys), or --relations for an identity audit\n" +
			"that groups columns by whether they key on conv-id or the stable agent_id.\n\n" +
			"Operates on the live database, migrating it to the current version first\n" +
			"(the same path every other command takes).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *SchemaParams, cmd *cobra.Command, args []string) {
			if err := runSchema(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runSchema(p *SchemaParams) error {
	d, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	switch {
	case p.JSON:
		info, err := db.SchemaStructured(d)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	case p.Relations:
		return printRelations(d)
	default:
		dump, err := db.SchemaSQL(d)
		if err != nil {
			return err
		}
		fmt.Print(dump)
		return nil
	}
}

// printRelations renders the identity FK audit: every column whose name keys on
// conv-id vs agent_id, grouped, so conv-keyed columns can be checked against the
// locked split (actor/relationship tables -> agent_id; conversation-generation
// tables stay conv-keyed).
func printRelations(d *sql.DB) error {
	info, err := db.SchemaStructured(d)
	if err != nil {
		return err
	}

	var convCols, agentCols []string
	for _, t := range info.Tables {
		for _, c := range t.Columns {
			switch c.Identity {
			case "conv":
				convCols = append(convCols, t.Name+"."+c.Name)
			case "agent":
				agentCols = append(agentCols, t.Name+"."+c.Name)
			}
		}
	}
	sort.Strings(convCols)
	sort.Strings(agentCols)

	fmt.Printf("Schema identity audit (conv-id vs agent_id)  [schema v%d]\n\n", info.SchemaVersion)
	fmt.Printf("Conv-keyed columns (%d) — each should name a conversation generation, not an actor:\n", len(convCols))
	printColumnList(convCols)
	fmt.Printf("\nAgent-keyed columns (%d):\n", len(agentCols))
	printColumnList(agentCols)
	return nil
}

func printColumnList(cols []string) {
	if len(cols) == 0 {
		fmt.Println("  (none)")
		return
	}
	for _, c := range cols {
		fmt.Printf("  %s\n", c)
	}
}
