package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// migrateV162toV163 drops sandbox_profiles.break_glass_filesystem_json.
// TCL-791 removed break-glass entirely: it presented a tclaude-developer
// debugging affordance as an operator security feature, and everything it could
// do is available by disabling the sandbox. The protected-root invariant is now
// absolute — no profile, include, launch contract, acknowledgement, or CLI flag
// reaches protected tclaude/harness state.
//
// The rows are DROPPED, never translated. Rewriting them into ordinary
// filesystem grants would reopen protected roots as ordinary access — a silent
// privilege escalation and the exact inverse of the ticket. It would also not
// survive: normalizeFilesystem refuses any read/write rule intersecting a
// protected root, so a translated profile would fail to resolve on its next
// launch. Dropping strictly narrows every affected profile.
//
// Because dropping is silent by construction, the migration discloses it on two
// channels: a durable human_messages row (the dashboard Messages tab, which
// survives restart and stays unread until the operator acks it) and a
// MigrationReporter notice on the agentd startup terminal. Both fire only when
// rows actually carried break-glass — a clean install must not be told about a
// feature it never used.
//
// The two are written at deliberately different points. The durable row goes
// inside the transaction, so it lands if and only if the drop does. The
// terminal notice fires only after the commit succeeds, because a printed line
// cannot be rolled back and announcing a drop that then failed to commit would
// be worse than saying nothing.
//
// DROP COLUMN needs SQLite 3.35+; the probes keep the migration idempotent and
// a no-op on installs that never had the column.
func migrateV162toV163(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v162→v163 (drop sandbox break-glass): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	dropped, err := sandboxBreakGlassRowsToDrop(tx)
	if err != nil {
		return err
	}
	disclosure := ""
	if len(dropped) > 0 {
		disclosure = breakGlassDropDisclosure(dropped)
		// Frozen inline SQL, not the live insertHumanMessage helper. A migration
		// must keep running against the schema as it existed at ITS version
		// forever: the helper writes today's column list and derives from_agent
		// through a subquery on agent_conversations, so the day either of those
		// changes, this v162→v163 step would start failing on any install still
		// upgrading through it. Only the columns set here are named; every other
		// column carried a DEFAULT at v163. from_conv is '' because the sender is
		// the migration itself, not an agent conversation.
		if _, err := tx.Exec(`
			INSERT INTO human_messages (from_conv, from_title, subject, body, created_at)
			VALUES ('', ?, ?, ?, ?)`,
			breakGlassDropSender, breakGlassDropSubject, disclosure,
			time.Now().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("migrate v162→v163 (disclose dropped break-glass rules): %w", err)
		}
	}

	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v162→v163 (drop sandbox break-glass): probe table: %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'break_glass_filesystem_json'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v162→v163 (probe sandbox_profiles.break_glass_filesystem_json): %w", err)
		}
		if haveColumn > 0 {
			if _, err := tx.Exec(`ALTER TABLE sandbox_profiles DROP COLUMN break_glass_filesystem_json`); err != nil {
				return fmt.Errorf("migrate v162→v163 (drop sandbox_profiles.break_glass_filesystem_json): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 163`); err != nil {
		return fmt.Errorf("migrate v162→v163 (drop sandbox break-glass): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v162→v163 (drop sandbox break-glass): commit: %w", err)
	}
	// Only after the commit: the terminal notice cannot be rolled back, so
	// firing it inside the transaction would tell the operator about a drop
	// that a failed commit then undid. The durable human_messages row is
	// written inside the transaction precisely so the two channels agree.
	if disclosure != "" {
		migrationReporter.reportNotice(163, disclosure)
	}
	return nil
}

const (
	breakGlassDropSender  = "tclaude schema migration v163"
	breakGlassDropSubject = "Sandbox profiles: break-glass access removed"
)

// droppedBreakGlassRule is one protected-path grant this migration destroys,
// captured before the column goes so the disclosure can name it exactly.
type droppedBreakGlassRule struct {
	Profile string
	Path    string
	Access  string
}

// sandboxBreakGlassRowsToDrop reads the surviving grants. It is deliberately
// lenient: a row whose JSON no longer parses, or which the schema never had,
// yields nothing rather than blocking the upgrade. The column is going either
// way, so failing the migration over unreadable data an operator can no longer
// use would strand them on the old schema for nothing.
func sandboxBreakGlassRowsToDrop(tx *sql.Tx) ([]droppedBreakGlassRule, error) {
	var haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'break_glass_filesystem_json'`).Scan(&haveColumn); err != nil {
		return nil, fmt.Errorf("migrate v162→v163 (probe sandbox_profiles.break_glass_filesystem_json): %w", err)
	}
	if haveColumn == 0 {
		return nil, nil
	}
	rows, err := tx.Query(`SELECT name, break_glass_filesystem_json FROM sandbox_profiles ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("migrate v162→v163 (read sandbox break-glass rules): %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []droppedBreakGlassRule
	for rows.Next() {
		var name, payload string
		if err := rows.Scan(&name, &payload); err != nil {
			return nil, fmt.Errorf("migrate v162→v163 (read sandbox break-glass rules): %w", err)
		}
		var grants []struct {
			Path   string `json:"path"`
			Access string `json:"access"`
		}
		if err := json.Unmarshal([]byte(payload), &grants); err != nil {
			continue
		}
		for _, grant := range grants {
			out = append(out, droppedBreakGlassRule{Profile: name, Path: grant.Path, Access: grant.Access})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate v162→v163 (read sandbox break-glass rules): %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Profile != out[j].Profile {
			return out[i].Profile < out[j].Profile
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Access < out[j].Access
	})
	return out, nil
}

// breakGlassDropDisclosure names the real reason and the exact loss. It states
// explicitly that nothing was widened, because "your rules were removed" would
// otherwise leave an operator unsure whether the access moved somewhere else.
func breakGlassDropDisclosure(dropped []droppedBreakGlassRule) string {
	var b strings.Builder
	b.WriteString("tclaude no longer has a break-glass feature. It presented a tclaude-developer " +
		"debugging affordance as an operator security feature, and everything it could do is available " +
		"by disabling the sandbox instead. Protected tclaude/harness state (~/.tclaude/data, " +
		"~/.claude/sessions) is now unreachable from any sandboxed agent: no profile, include, launch " +
		"contract, acknowledgement, or CLI flag reopens it.\n\n")
	b.WriteString("Schema migration v163 DROPPED the break-glass rules below. They were NOT converted " +
		"into ordinary filesystem rules — that would have reopened protected roots as ordinary grants. " +
		"Nothing was widened; the affected profiles are now strictly narrower:\n\n")
	for _, rule := range dropped {
		fmt.Fprintf(&b, "  • %s: %s %s\n", rule.Profile, rule.Access, rule.Path)
	}
	b.WriteString("\nAgents already running with this access keep it only until they are relaunched. " +
		"To work without the protected-root wall, launch with the sandbox disabled.")
	return b.String()
}
