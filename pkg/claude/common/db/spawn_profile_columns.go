package db

import (
	"database/sql"
	"database/sql/driver"
	"strings"
	"time"
)

// spawnProfileColumn is one persisted spawn_profiles column. The ordered
// spawnProfileColumns table is the single list the INSERT, the UPDATE and the
// SELECT/scan are generated from, so a new profile option is one entry here
// instead of six positional lists kept in lockstep by hand.
type spawnProfileColumn struct {
	name string
	// fields names the SpawnProfile fields this column persists. The
	// coverage test checks every persisted struct field is named by a column.
	fields []string
	// insertOnly columns are written by INSERT but left alone by UPDATE.
	insertOnly bool
	// value is the bound write argument.
	value func(w *spawnProfileWrite) any
	// scan returns the Scan destination and a finish step that copies the
	// scanned value into p. Finish steps run in column order after Scan.
	scan func(p *SpawnProfile) (dest any, finish func() error)
}

// spawnProfileWrite carries a profile plus the write-time values that are
// derived once per statement rather than per column.
type spawnProfileWrite struct {
	p               *SpawnProfile
	roleRef         string
	roleRefs        string
	environmentJSON string
	now             driver.Valuer
}

func newSpawnProfileWrite(p *SpawnProfile, now driver.Valuer) (*spawnProfileWrite, error) {
	environmentJSON, err := marshalEnvironmentColumn(p.Environment)
	if err != nil {
		return nil, err
	}
	roleRef, roleRefs := marshalProfileRoleRefs(p.RoleRefs, p.RoleRef)
	return &spawnProfileWrite{p: p, roleRef: roleRef, roleRefs: roleRefs, environmentJSON: environmentJSON, now: now}, nil
}

// directCol persists a field that the driver writes and scans as-is.
func directCol[T any](name, field string, ptr func(p *SpawnProfile) *T) spawnProfileColumn {
	return spawnProfileColumn{
		name:   name,
		fields: []string{field},
		value:  func(w *spawnProfileWrite) any { return *ptr(w.p) },
		scan:   func(p *SpawnProfile) (any, func() error) { return ptr(p), nil },
	}
}

// triBoolCol persists a tri-state *bool in a nullable INTEGER column.
func triBoolCol(name, field string, ptr func(p *SpawnProfile) **bool) spawnProfileColumn {
	return spawnProfileColumn{
		name:   name,
		fields: []string{field},
		value:  func(w *spawnProfileWrite) any { return boolPtrToNull(*ptr(w.p)) },
		scan: func(p *SpawnProfile) (any, func() error) {
			var n sql.NullInt64
			return &n, func() error { *ptr(p) = nullToBoolPtr(n); return nil }
		},
	}
}

// textCol persists a field through a string column with its own encoding.
func textCol(name, field string, value func(w *spawnProfileWrite) any, decode func(p *SpawnProfile, raw string) error) spawnProfileColumn {
	return spawnProfileColumn{
		name:   name,
		fields: []string{field},
		value:  value,
		scan: func(p *SpawnProfile) (any, func() error) {
			var raw string
			return &raw, func() error { return decode(p, raw) }
		},
	}
}

// timeCol persists a timestamp; both write the statement's single now.
func timeCol(name, field string, insertOnly bool, ptr func(p *SpawnProfile) *time.Time) spawnProfileColumn {
	return spawnProfileColumn{
		name:       name,
		fields:     []string{field},
		insertOnly: insertOnly,
		value:      func(w *spawnProfileWrite) any { return w.now },
		scan: func(p *SpawnProfile) (any, func() error) {
			var ts dbTimestamp
			return &ts, func() error { *ptr(p) = ts.Time(); return nil }
		},
	}
}

// spawnProfileColumns is today's column order. Keep it: it is the order of
// the generated INSERT, UPDATE and SELECT lists.
var spawnProfileColumns = []spawnProfileColumn{
	directCol("name", "Name", func(p *SpawnProfile) *string { return &p.Name }),
	{
		// Written as a bool, read back as an INTEGER (any non-zero is on).
		name:   "disabled",
		fields: []string{"Disabled"},
		value:  func(w *spawnProfileWrite) any { return w.p.Disabled },
		scan: func(p *SpawnProfile) (any, func() error) {
			var n int64
			return &n, func() error { p.Disabled = n != 0; return nil }
		},
	},
	directCol("disabled_reason", "DisabledReason", func(p *SpawnProfile) *string { return &p.DisabledReason }),
	directCol("operator_only", "OperatorOnly", func(p *SpawnProfile) *bool { return &p.OperatorOnly }),
	directCol("harness", "Harness", func(p *SpawnProfile) *string { return &p.Harness }),
	directCol("model", "Model", func(p *SpawnProfile) *string { return &p.Model }),
	directCol("effort", "Effort", func(p *SpawnProfile) *string { return &p.Effort }),
	directCol("sandbox", "Sandbox", func(p *SpawnProfile) *string { return &p.Sandbox }),
	directCol("sandbox_implementation", "SandboxImplementation", func(p *SpawnProfile) *string { return &p.SandboxImplementation }),
	directCol("approval", "Approval", func(p *SpawnProfile) *string { return &p.Approval }),
	directCol("tools", "ToolGovernance", func(p *SpawnProfile) *string { return &p.ToolGovernance }),
	directCol("ask_user_question_timeout", "AskUserQuestionTimeout", func(p *SpawnProfile) *string { return &p.AskUserQuestionTimeout }),
	directCol("auto_compact_window", "AutoCompactWindow", func(p *SpawnProfile) *string { return &p.AutoCompactWindow }),
	directCol("context_window_max", "ContextWindowMax", func(p *SpawnProfile) *int64 { return &p.ContextWindowMax }),
	triBoolCol("copilot_api", "CopilotAPI", func(p *SpawnProfile) **bool { return &p.CopilotAPI }),
	triBoolCol("codex_app_server", "CodexAppServer", func(p *SpawnProfile) **bool { return &p.CodexAppServer }),
	triBoolCol("fast_mode", "FastMode", func(p *SpawnProfile) **bool { return &p.FastMode }),
	triBoolCol("auto_review", "AutoReview", func(p *SpawnProfile) **bool { return &p.AutoReview }),
	triBoolCol("trust_dir", "TrustDir", func(p *SpawnProfile) **bool { return &p.TrustDir }),
	directCol("agent_name", "AgentName", func(p *SpawnProfile) *string { return &p.AgentName }),
	directCol("role", "Role", func(p *SpawnProfile) *string { return &p.Role }),
	// role_ref is the compatibility projection of role_refs[0]; both are
	// derived together on write, and role_refs' finish re-derives RoleRef.
	{
		name:   "role_ref",
		fields: []string{"RoleRef"},
		value:  func(w *spawnProfileWrite) any { return w.roleRef },
		scan:   func(p *SpawnProfile) (any, func() error) { return &p.RoleRef, nil },
	},
	textCol("role_refs", "RoleRefs",
		func(w *spawnProfileWrite) any { return w.roleRefs },
		func(p *SpawnProfile, raw string) error {
			p.RoleRefs = unmarshalProfileRoleRefs(raw, p.RoleRef)
			if len(p.RoleRefs) > 0 {
				p.RoleRef = p.RoleRefs[0]
			}
			return nil
		}),
	textCol("prompt_refs", "PromptRefs",
		func(w *spawnProfileWrite) any { return MarshalPromptRefs(w.p.PromptRefs) },
		func(p *SpawnProfile, raw string) error { p.PromptRefs = UnmarshalPromptRefs(raw); return nil }),
	directCol("descr", "Descr", func(p *SpawnProfile) *string { return &p.Descr }),
	directCol("initial_message", "InitialMessage", func(p *SpawnProfile) *string { return &p.InitialMessage }),
	directCol("startup_context", "StartupContext", func(p *SpawnProfile) *string { return &p.StartupContext }),
	triBoolCol("sync_worktree", "SyncWorktree", func(p *SpawnProfile) **bool { return &p.SyncWorktree }),
	triBoolCol("fetch_latest_worktree", "FetchLatestWorktree", func(p *SpawnProfile) **bool { return &p.FetchLatestWorktree }),
	triBoolCol("auto_focus", "AutoFocus", func(p *SpawnProfile) **bool { return &p.AutoFocus }),
	triBoolCol("include_group_default_context", "IncludeGroupDefaultContext", func(p *SpawnProfile) **bool { return &p.IncludeGroupDefaultContext }),
	triBoolCol("remote_control", "RemoteControl", func(p *SpawnProfile) **bool { return &p.RemoteControl }),
	triBoolCol("auto_memory", "AutoMemory", func(p *SpawnProfile) **bool { return &p.AutoMemory }),
	triBoolCol("peer_messaging", "PeerMessaging", func(p *SpawnProfile) **bool { return &p.PeerMessaging }),
	triBoolCol("ssh_workaround", "SSHWorkaround", func(p *SpawnProfile) **bool { return &p.SSHWorkaround }),
	triBoolCol("is_owner", "IsOwner", func(p *SpawnProfile) **bool { return &p.IsOwner }),
	textCol("permission_overrides", "PermissionOverrides",
		func(w *spawnProfileWrite) any { return marshalPermissionOverrides(w.p.PermissionOverrides) },
		func(p *SpawnProfile, raw string) error {
			p.PermissionOverrides = unmarshalPermissionOverrides(raw)
			return nil
		}),
	textCol("context_features", "ContextFeatures",
		func(w *spawnProfileWrite) any {
			return marshalStringMapColumn(w.p.ContextFeatures, "spawn_profiles.context_features")
		},
		func(p *SpawnProfile, raw string) error {
			p.ContextFeatures = unmarshalStringMapColumn(raw, "spawn_profiles.context_features")
			return nil
		}),
	textCol("environment_json", "Environment",
		func(w *spawnProfileWrite) any { return w.environmentJSON },
		func(p *SpawnProfile, raw string) error {
			env, err := unmarshalEnvironmentColumn(raw)
			if err != nil {
				return err
			}
			p.Environment = env
			return nil
		}),
	timeCol("created_at", "CreatedAt", true, func(p *SpawnProfile) *time.Time { return &p.CreatedAt }),
	timeCol("updated_at", "UpdatedAt", false, func(p *SpawnProfile) *time.Time { return &p.UpdatedAt }),
	directCol("model_proxy", "ModelProxy", func(p *SpawnProfile) *string { return &p.ModelProxy }),
}

// Generated once from spawnProfileColumns.
var (
	spawnProfileInsertSQL = buildSpawnProfileInsertSQL()
	spawnProfileUpdateSQL = buildSpawnProfileUpdateSQL()
	spawnProfileSelect    = buildSpawnProfileSelectSQL()
)

func buildSpawnProfileInsertSQL() string {
	names := make([]string, 0, len(spawnProfileColumns))
	for _, c := range spawnProfileColumns {
		names = append(names, c.name)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	return `INSERT INTO spawn_profiles (` + strings.Join(names, ", ") + `) VALUES (` + placeholders + `)`
}

func buildSpawnProfileUpdateSQL() string {
	sets := make([]string, 0, len(spawnProfileColumns))
	for _, c := range spawnProfileColumns {
		if !c.insertOnly {
			sets = append(sets, c.name+" = ?")
		}
	}
	return `UPDATE spawn_profiles SET ` + strings.Join(sets, ", ") + ` WHERE id = ?`
}

func buildSpawnProfileSelectSQL() string {
	names := []string{"id"}
	for _, c := range spawnProfileColumns {
		names = append(names, c.name)
	}
	return `SELECT ` + strings.Join(names, ", ") + ` FROM spawn_profiles`
}

// spawnProfileInsertArgs returns the INSERT arguments in column order.
func spawnProfileInsertArgs(w *spawnProfileWrite) []any {
	args := make([]any, 0, len(spawnProfileColumns))
	for _, c := range spawnProfileColumns {
		args = append(args, c.value(w))
	}
	return args
}

// spawnProfileUpdateArgs returns the UPDATE arguments in column order,
// followed by the WHERE id.
func spawnProfileUpdateArgs(w *spawnProfileWrite) []any {
	args := make([]any, 0, len(spawnProfileColumns)+1)
	for _, c := range spawnProfileColumns {
		if !c.insertOnly {
			args = append(args, c.value(w))
		}
	}
	return append(args, w.p.ID)
}

func scanSpawnProfile(s rowScanner) (*SpawnProfile, error) {
	var p SpawnProfile
	dests := make([]any, 0, len(spawnProfileColumns)+1)
	finishes := make([]func() error, 0, len(spawnProfileColumns))
	dests = append(dests, &p.ID)
	for _, c := range spawnProfileColumns {
		dest, finish := c.scan(&p)
		dests = append(dests, dest)
		if finish != nil {
			finishes = append(finishes, finish)
		}
	}
	if err := s.Scan(dests...); err != nil {
		return nil, err
	}
	for _, finish := range finishes {
		if err := finish(); err != nil {
			return nil, err
		}
	}
	return &p, nil
}
