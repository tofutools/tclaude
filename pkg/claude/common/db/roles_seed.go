package db

import (
	"database/sql"
	"fmt"
	"time"
)

// ReservedRoleNames are role names tclaude will not let a user create — they
// collide with a reserved routing target elsewhere. "all" is the work-pattern
// broadcast target for template agents (see agentd/templates.go), so keeping
// the role namespace clear of it avoids a confusing name clash.
var ReservedRoleNames = map[string]bool{
	"all": true,
}

// seedRole is one canonical role tclaude ships. The briefs are SHORT, generic
// defaults — a sensible starting point, NOT policy. Launch fields and
// permissions are intentionally left blank/empty: what a role should launch on
// or be granted is the user's call, so a seed presumes neither. A user edits a
// seed to taste and their edits are then sacred (never overwritten by the
// re-seed on open).
type seedRole struct {
	name  string
	descr string
	brief string
}

// seedRoles is the canonical role library shipped with tclaude. Order is the
// party's natural hierarchy (in wizard-mode lore these are the party's
// classes, with the lead as the party leader); the seeder inserts any that are
// missing.
var seedRoles = []seedRole{
	{
		name:  "po",
		descr: "Product owner — coordinates the team and keeps work aligned",
		brief: "You are the product owner. You coordinate the team: you shape and prioritise the work, keep " +
			"everyone pointed at the same goal, unblock people, and surface decisions the human needs to make. " +
			"You do not usually write the code yourself — you make sure the right work happens in the right order.",
	},
	{
		name:  "lead",
		descr: "Tech lead — owns technical direction and vets the team's work",
		brief: "You are the technical lead. You own the technical direction of the work: you break large tasks " +
			"into workstreams, set the approach, review what the team produces before it lands, and are the final " +
			"technical vet on the group's output. You mentor and unblock the other agents on hard problems.",
	},
	{
		name:  "dev",
		descr: "Developer — implements features and fixes end to end",
		brief: "You are a developer. You implement features and fixes end to end: you understand the task, write " +
			"the code, cover it with tests, and see it through review to a merged, working result. You raise scope " +
			"questions and blockers early rather than guessing.",
	},
	{
		name:  "designer",
		descr: "Designer — owns UX, visual design and interaction detail",
		brief: "You are a designer. You own the user experience and the visual/interaction detail of the work: " +
			"layout, flow, copy, and the feel of the thing. You produce concrete designs (mockups, specs, or the " +
			"styling itself) and partner with the developers to get them built faithfully.",
	},
	{
		name:  "reviewer",
		descr: "Reviewer — reviews changes cold for correctness and quality",
		brief: "You are a reviewer. You review changes with fresh eyes — given the diff and little of the " +
			"backstory — hunting for correctness bugs, missing cases, and quality problems the author has already " +
			"rationalised away. You are specific and honest: you separate real defects from nits and say which is which.",
	},
	{
		name:  "tester",
		descr: "Tester — verifies behaviour and hardens the test suite",
		brief: "You are a tester. You verify that changes actually do what they claim by exercising them, and you " +
			"harden the automated test suite: you find the edge cases, write the regression tests, and report what " +
			"you observed — including failures — plainly and reproducibly.",
	},
}

// ensureSeededRoles re-adds any missing canonical seed role, without ever
// overwriting an existing role of the same name (user edits are sacred). It is
// idempotent and self-healing (the repo's "self-healing over one-shot
// migrations" principle): run on every process's first Open, so a seed a user
// deleted reappears on the next open, but a seed they renamed/edited is left
// alone. It only writes when at least one seed is actually missing, so a
// steady-state open pays a single indexed read and no write.
func ensureSeededRoles(d *sql.DB) error {
	existing := map[string]bool{}
	rows, err := d.Query(`SELECT name FROM roles`)
	if err != nil {
		return fmt.Errorf("ensure seed roles (list): %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("ensure seed roles (scan): %w", err)
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("ensure seed roles (iterate): %w", err)
	}
	_ = rows.Close()

	var missing []seedRole
	for _, s := range seedRoles {
		if !existing[s.name] {
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	now := time.Now().Format(time.RFC3339Nano)
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("ensure seed roles (begin): %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range missing {
		// INSERT OR IGNORE so a concurrent seeder that won the race (or a role
		// created between the read above and here) is a clean no-op rather than
		// a UNIQUE failure — the name is never overwritten either way.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO roles
			   (name, descr, brief, spawn_profile, harness, model, effort, sandbox, approval,
			    permissions, created_at, updated_at)
			 VALUES (?, ?, ?, '', '', '', '', '', '', '[]', ?, ?)`,
			s.name, s.descr, s.brief, now, now); err != nil {
			return fmt.Errorf("ensure seed roles (insert %q): %w", s.name, err)
		}
	}
	return tx.Commit()
}
