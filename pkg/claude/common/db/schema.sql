CREATE TABLE schema_version (version INTEGER NOT NULL);

CREATE TABLE group_template_agents (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			template_id     INTEGER NOT NULL
			                  REFERENCES group_templates(id) ON DELETE CASCADE,
			ordinal         INTEGER NOT NULL DEFAULT 0,
			name            TEXT NOT NULL,
			role            TEXT NOT NULL DEFAULT '',
			descr           TEXT NOT NULL DEFAULT '',
			initial_message TEXT NOT NULL DEFAULT '',
			is_owner        INTEGER NOT NULL DEFAULT 0,
			permissions     TEXT NOT NULL DEFAULT '[]'
		, spawn_profile TEXT NOT NULL DEFAULT '', harness TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', effort TEXT NOT NULL DEFAULT '', sandbox TEXT NOT NULL DEFAULT '', approval TEXT NOT NULL DEFAULT '', role_ref TEXT NOT NULL DEFAULT '', wave INTEGER NOT NULL DEFAULT 0, profile_inline TEXT NOT NULL DEFAULT '', spawn_profile_id INTEGER);

CREATE INDEX idx_group_template_agents_template
			ON group_template_agents(template_id);

CREATE INDEX idx_template_agents_spawn_profile_id ON group_template_agents(spawn_profile_id);

CREATE TRIGGER stable_ref_template_agent_profile_insert
			AFTER INSERT ON group_template_agents BEGIN
				UPDATE group_template_agents SET spawn_profile_id = COALESCE(NEW.spawn_profile_id,
					(SELECT id FROM spawn_profiles WHERE name = NEW.spawn_profile))
				 WHERE id = NEW.id;
			END;

CREATE TRIGGER stable_ref_template_agent_profile_update
			AFTER UPDATE OF spawn_profile ON group_template_agents
			WHEN NEW.spawn_profile IS NOT OLD.spawn_profile BEGIN
				UPDATE group_template_agents SET spawn_profile_id = CASE
					WHEN NEW.spawn_profile_id IS NOT OLD.spawn_profile_id THEN NEW.spawn_profile_id
					ELSE (SELECT id FROM spawn_profiles WHERE name = NEW.spawn_profile) END
				 WHERE id = NEW.id;
			END;

CREATE TABLE agent_tags (
			agent_id TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			tag      TEXT NOT NULL,
			PRIMARY KEY (agent_id, tag)
		);

CREATE INDEX idx_agent_tags_tag ON agent_tags(tag);

CREATE TABLE sandbox_profile_global_assignment (
			id           INTEGER PRIMARY KEY CHECK (id = 1),
			profile_name TEXT NOT NULL,
			profile_id   INTEGER NOT NULL REFERENCES sandbox_profiles(id) ON DELETE CASCADE
		);

CREATE TABLE spawn_profile_aliases (
			alias TEXT PRIMARY KEY,
			profile_id INTEGER NOT NULL REFERENCES spawn_profiles(id) ON DELETE CASCADE
		);

CREATE INDEX idx_spawn_profile_aliases_profile
			ON spawn_profile_aliases(profile_id);

CREATE TRIGGER spawn_profile_alias_not_name_insert
		BEFORE INSERT ON spawn_profile_aliases
		WHEN EXISTS (SELECT 1 FROM spawn_profiles WHERE name = NEW.alias)
		BEGIN
			SELECT RAISE(ABORT, 'spawn profile handle already exists');
		END;

CREATE TRIGGER spawn_profile_alias_not_name_update
		BEFORE UPDATE OF alias ON spawn_profile_aliases
		WHEN EXISTS (SELECT 1 FROM spawn_profiles WHERE name = NEW.alias)
		BEGIN
			SELECT RAISE(ABORT, 'spawn profile handle already exists');
		END;

CREATE TABLE agent_message_attachments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id INTEGER NOT NULL REFERENCES agent_messages(id) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL,
			filename TEXT NOT NULL,
			content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
			size_bytes INTEGER NOT NULL,
			storage_path TEXT NOT NULL,
			UNIQUE(message_id, ordinal)
		);

CREATE INDEX idx_agent_message_attachments_message ON agent_message_attachments(message_id, ordinal);

CREATE TABLE operator_agent_messages (
			message_id INTEGER PRIMARY KEY REFERENCES agent_messages(id) ON DELETE CASCADE
		);

CREATE TABLE process_snippet_library (
			id         INTEGER PRIMARY KEY CHECK(id = 1),
			generation INTEGER NOT NULL CHECK(generation >= 0)
		);

CREATE TABLE agent_standing_order_messages (
			message_id     INTEGER PRIMARY KEY
			               REFERENCES agent_messages(id) ON DELETE CASCADE,
			order_id       INTEGER NOT NULL,
			order_revision INTEGER NOT NULL,
			opencode_message_id TEXT NOT NULL UNIQUE
		);

CREATE TABLE agent_standing_order_hook_selectors (
			order_id INTEGER NOT NULL
			         REFERENCES agent_standing_orders(id) ON DELETE CASCADE,
			harness  TEXT NOT NULL,
			event    TEXT NOT NULL,
			PRIMARY KEY (order_id, harness, event)
		);

CREATE INDEX idx_agent_standing_order_hook_selectors_event
			ON agent_standing_order_hook_selectors(harness, event, order_id);

CREATE TABLE "sessions" (
			id              TEXT PRIMARY KEY,
			tmux_session    TEXT NOT NULL DEFAULT '',
			pid             INTEGER NOT NULL DEFAULT 0,
			cwd             TEXT NOT NULL DEFAULT '',
			conv_id         TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'idle',
			status_detail   TEXT NOT NULL DEFAULT '',
			auto_registered INTEGER NOT NULL DEFAULT 0,
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL
		, context_pct REAL NOT NULL DEFAULT 0, subagent_count INTEGER NOT NULL DEFAULT 0, last_hook INTEGER, tokens_input INTEGER NOT NULL DEFAULT 0, tokens_output INTEGER NOT NULL DEFAULT 0, context_window_size INTEGER NOT NULL DEFAULT 0, nudged_pct REAL NOT NULL DEFAULT 0, exit_reason TEXT, model TEXT NOT NULL DEFAULT '', effort_level TEXT NOT NULL DEFAULT '', pending_conv TEXT NOT NULL DEFAULT '', cost_usd REAL NOT NULL DEFAULT 0, model_id TEXT NOT NULL DEFAULT '', harness TEXT NOT NULL DEFAULT 'claude', sandbox_mode TEXT NOT NULL DEFAULT '', remote_control INTEGER NOT NULL DEFAULT 0, virtual_cost_usd REAL NOT NULL DEFAULT 0, agent_id TEXT NOT NULL DEFAULT '', last_statusline_json TEXT NOT NULL DEFAULT '', subagents_json TEXT NOT NULL DEFAULT '', ask_user_question_timeout TEXT NOT NULL DEFAULT '', effective_sandbox_config TEXT NOT NULL DEFAULT '', approval_policy TEXT NOT NULL DEFAULT '', approval_auto_review INTEGER NOT NULL DEFAULT 0, resume_provenance TEXT NOT NULL DEFAULT '', exit_intent TEXT NOT NULL DEFAULT '', exit_intent_event_id TEXT NOT NULL DEFAULT '', exit_intent_generation TEXT NOT NULL DEFAULT '', exit_intent_at INTEGER, exit_callback_generation TEXT NOT NULL DEFAULT '', exit_callback_token_hash TEXT NOT NULL DEFAULT '', exit_callback_pane_id TEXT NOT NULL DEFAULT '', exit_callback_used_at INTEGER, exit_launch_gate_state TEXT NOT NULL DEFAULT '', auto_memory INTEGER NOT NULL DEFAULT 0, bg_shells_json TEXT NOT NULL DEFAULT '', context_features TEXT NOT NULL DEFAULT '', auto_compact_window TEXT NOT NULL DEFAULT '', os_sandbox_state TEXT NOT NULL DEFAULT '', os_sandbox_source TEXT NOT NULL DEFAULT '', os_sandbox_unverified INTEGER NOT NULL DEFAULT 0, sandbox_mode_source TEXT NOT NULL DEFAULT '', sandbox_implementation TEXT NOT NULL DEFAULT 'harness-builtin', monitors_json TEXT NOT NULL DEFAULT '') STRICT;

CREATE INDEX idx_sessions_conv_id ON sessions(conv_id);

CREATE INDEX idx_sessions_status_updated ON sessions(status, updated_at);

CREATE TABLE "notify_state" (
			session_id  TEXT PRIMARY KEY,
			notified_at INTEGER NOT NULL
		) STRICT;

CREATE TABLE "usage_cache" (
			id              INTEGER PRIMARY KEY,
			data            TEXT NOT NULL DEFAULT '{}',
			fetched_at      INTEGER,
			last_attempt_at INTEGER
		) STRICT;

CREATE TABLE "git_cache" (
			repo_hash  TEXT PRIMARY KEY,
			data       TEXT NOT NULL DEFAULT '{}',
			fetched_at INTEGER
		) STRICT;

CREATE TABLE "conv_index" (
			conv_id       TEXT PRIMARY KEY,
			project_dir   TEXT NOT NULL,
			full_path     TEXT NOT NULL,
			file_mtime    INTEGER,
			file_size     INTEGER NOT NULL DEFAULT 0,
			first_prompt  TEXT NOT NULL DEFAULT '',
			summary       TEXT NOT NULL DEFAULT '',
			custom_title  TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			created       INTEGER,
			modified      INTEGER,
			git_branch    TEXT NOT NULL DEFAULT '',
			project_path  TEXT NOT NULL DEFAULT '',
			is_sidechain  INTEGER NOT NULL DEFAULT 0,
			indexed_at    INTEGER
		, archived_at INTEGER, git_branch_startup TEXT NOT NULL DEFAULT '', harness TEXT NOT NULL DEFAULT 'claude') STRICT;

CREATE INDEX idx_conv_index_project_dir ON conv_index(project_dir);

CREATE INDEX idx_conv_index_archived
			ON conv_index(archived_at);

CREATE TABLE "conv_embeddings" (
			conv_id     TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			chunk_type  TEXT NOT NULL DEFAULT 'content',
			chunk_text  TEXT NOT NULL DEFAULT '',
			embedding   BLOB NOT NULL,
			model       TEXT NOT NULL DEFAULT '',
			created_at  INTEGER,
			PRIMARY KEY (conv_id, chunk_index)
		) STRICT;

CREATE INDEX idx_conv_embeddings_conv_id ON conv_embeddings(conv_id);

CREATE TABLE "agent_groups" (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE,
			descr       TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL
		, archived_at INTEGER, default_cwd TEXT NOT NULL DEFAULT '', default_context TEXT NOT NULL DEFAULT '', max_members INTEGER NOT NULL DEFAULT 0, notify_enabled INTEGER NOT NULL DEFAULT 1, default_profile TEXT NOT NULL DEFAULT '', remote_control INTEGER, mission TEXT NOT NULL DEFAULT '', source_template TEXT NOT NULL DEFAULT '', parent_id INTEGER REFERENCES agent_groups(id) ON DELETE SET NULL, default_profile_id INTEGER, source_template_id INTEGER, sandbox_profile TEXT NOT NULL DEFAULT '', sandbox_profile_id INTEGER, attachment_url TEXT NOT NULL DEFAULT '', attachment_label TEXT NOT NULL DEFAULT '', route_generation INTEGER NOT NULL DEFAULT 0, owner_scopes_json TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(owner_scopes_json AS BLOB)) BETWEEN 0 AND 262144)) STRICT;

CREATE INDEX idx_agent_groups_archived
			ON agent_groups(archived_at);

CREATE INDEX idx_agent_groups_default_profile_id ON agent_groups(default_profile_id);

CREATE INDEX idx_agent_groups_source_template_id ON agent_groups(source_template_id);

CREATE INDEX idx_agent_groups_sandbox_profile_id ON agent_groups(sandbox_profile_id);

CREATE TRIGGER stable_ref_group_profile_insert
			AFTER INSERT ON agent_groups BEGIN
				UPDATE agent_groups SET default_profile_id = COALESCE(NEW.default_profile_id,
					(SELECT id FROM spawn_profiles WHERE name = NEW.default_profile))
				 WHERE id = NEW.id;
			END;

CREATE TRIGGER stable_ref_group_profile_update
			AFTER UPDATE OF default_profile ON agent_groups
			WHEN NEW.default_profile IS NOT OLD.default_profile BEGIN
				UPDATE agent_groups SET default_profile_id = CASE
					WHEN NEW.default_profile_id IS NOT OLD.default_profile_id THEN NEW.default_profile_id
					ELSE (SELECT id FROM spawn_profiles WHERE name = NEW.default_profile) END
				 WHERE id = NEW.id;
			END;

CREATE TRIGGER stable_ref_group_template_insert
			AFTER INSERT ON agent_groups BEGIN
				UPDATE agent_groups SET source_template_id = COALESCE(NEW.source_template_id,
					(SELECT id FROM group_templates WHERE name = NEW.source_template))
				 WHERE id = NEW.id;
			END;

CREATE TRIGGER stable_ref_group_template_update
			AFTER UPDATE OF source_template ON agent_groups
			WHEN NEW.source_template IS NOT OLD.source_template BEGIN
				UPDATE agent_groups SET source_template_id = CASE
					WHEN NEW.source_template_id IS NOT OLD.source_template_id THEN NEW.source_template_id
					ELSE (SELECT id FROM group_templates WHERE name = NEW.source_template) END
				 WHERE id = NEW.id;
			END;

CREATE TRIGGER sandbox_profile_group_ref_insert
			BEFORE INSERT ON agent_groups
			WHEN NEW.sandbox_profile_id IS NOT NULL
			 AND NOT EXISTS (SELECT 1 FROM sandbox_profiles WHERE id = NEW.sandbox_profile_id)
			BEGIN SELECT RAISE(ABORT, 'sandbox profile reference does not exist'); END;

CREATE TRIGGER sandbox_profile_group_ref_update
			BEFORE UPDATE OF sandbox_profile_id ON agent_groups
			WHEN NEW.sandbox_profile_id IS NOT NULL
			 AND NOT EXISTS (SELECT 1 FROM sandbox_profiles WHERE id = NEW.sandbox_profile_id)
			BEGIN SELECT RAISE(ABORT, 'sandbox profile reference does not exist'); END;

CREATE TABLE "agent_cron_jobs" (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			name             TEXT NOT NULL DEFAULT '',
			owner_agent       TEXT NOT NULL,
			target_agent      TEXT NOT NULL,
			group_id         INTEGER NOT NULL DEFAULT 0,
			interval_seconds INTEGER NOT NULL,
			subject          TEXT NOT NULL DEFAULT '',
			body             TEXT NOT NULL DEFAULT '',
			enabled          INTEGER NOT NULL DEFAULT 1,
			created_at       INTEGER NOT NULL,
			last_run_at      INTEGER,
			last_run_status  TEXT NOT NULL DEFAULT ''
		, target_kind TEXT NOT NULL DEFAULT 'conv'
			CHECK (target_kind IN ('conv', 'group')), cron_expr TEXT NOT NULL DEFAULT '', target_role TEXT NOT NULL DEFAULT '', disabled_reason TEXT NOT NULL DEFAULT '', run_immediately INTEGER NOT NULL DEFAULT 0, queue_when_offline INTEGER NOT NULL DEFAULT 0, operator_authored INTEGER NOT NULL DEFAULT 0) STRICT;

CREATE INDEX idx_agent_cron_jobs_owner ON agent_cron_jobs(owner_agent);

CREATE INDEX idx_agent_cron_jobs_target ON agent_cron_jobs(target_agent);

CREATE TABLE "agent_cron_runs" (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id    INTEGER NOT NULL REFERENCES agent_cron_jobs(id) ON DELETE CASCADE,
			fired_at  INTEGER NOT NULL,
			status    TEXT NOT NULL DEFAULT '',
			error_msg TEXT NOT NULL DEFAULT ''
		) STRICT;

CREATE INDEX idx_agent_cron_runs_job
			ON agent_cron_runs(job_id, fired_at DESC);

CREATE TABLE "agent_conv_succession" (
			old_conv_id   TEXT PRIMARY KEY,
			new_conv_id   TEXT NOT NULL,
			reason        TEXT NOT NULL DEFAULT '',
			succeeded_at  INTEGER NOT NULL
		, agent_id TEXT NOT NULL DEFAULT '') STRICT;

CREATE INDEX idx_agent_conv_succession_new
			ON agent_conv_succession(new_conv_id);

CREATE TABLE "agent_clone_history" (
			source_agent_id TEXT NOT NULL,
			cloned_at      INTEGER NOT NULL
		) STRICT;

CREATE INDEX idx_clone_history_source
			ON agent_clone_history(source_agent_id, cloned_at);

CREATE TABLE "agent_group_audit" (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			old_name   TEXT NOT NULL,
			new_name   TEXT NOT NULL,
			by_conv    TEXT NOT NULL DEFAULT '',
			at         INTEGER NOT NULL
		, by_agent TEXT NOT NULL DEFAULT '') STRICT;

CREATE INDEX idx_agent_group_audit_group
			ON agent_group_audit(group_id, at);

CREATE TABLE "agent_head_aliases" (
			handle         TEXT PRIMARY KEY,
			anchor_conv_id TEXT NOT NULL,
			created_at     INTEGER NOT NULL,
			by_conv        TEXT NOT NULL DEFAULT ''
		, by_agent TEXT NOT NULL DEFAULT '', anchor_agent_id TEXT NOT NULL DEFAULT '') STRICT;

CREATE INDEX idx_agent_head_aliases_anchor
			ON agent_head_aliases(anchor_conv_id);

CREATE TABLE "agent_group_links" (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			from_group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			to_group_id     INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			mode            TEXT    NOT NULL,
			created_at      INTEGER    NOT NULL,
			by_conv         TEXT    NOT NULL DEFAULT '', by_agent TEXT NOT NULL DEFAULT '',
			UNIQUE (from_group_id, to_group_id, mode)
		) STRICT;

CREATE INDEX idx_agent_group_links_from
			ON agent_group_links(from_group_id);

CREATE INDEX idx_agent_group_links_to
			ON agent_group_links(to_group_id);

CREATE TABLE "agent_workdir" (
			conv_id    TEXT PRIMARY KEY,
			dir        TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		, worktree_root TEXT NOT NULL DEFAULT '', branch        TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '') STRICT;

CREATE TABLE "agent_spawn_history" (
			spawner_agent_id TEXT NOT NULL,
			spawned_at      INTEGER NOT NULL
		) STRICT;

CREATE INDEX idx_spawn_history_spawner
			ON agent_spawn_history(spawner_agent_id, spawned_at);

CREATE TABLE "agent_messages" (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id         INTEGER NOT NULL DEFAULT 0,
			from_conv        TEXT NOT NULL,
			to_conv          TEXT NOT NULL,
			subject          TEXT NOT NULL DEFAULT '',
			body             TEXT NOT NULL DEFAULT '',
			created_at       INTEGER NOT NULL,
			delivered_at     INTEGER,
			read_at          INTEGER,
			parent_id        INTEGER NOT NULL DEFAULT 0,
			to_recipients    TEXT NOT NULL DEFAULT '',
			cc_recipients    TEXT NOT NULL DEFAULT '',
			original_to_conv TEXT NOT NULL DEFAULT ''
		, from_agent TEXT NOT NULL DEFAULT '', to_agent TEXT NOT NULL DEFAULT '', to_recipient_agents TEXT NOT NULL DEFAULT '', cc_recipient_agents TEXT NOT NULL DEFAULT '', pin_gen INTEGER NOT NULL DEFAULT 0, nudge_claimed_at INTEGER, nudge_attempted_at INTEGER, nudge_attempts INTEGER NOT NULL DEFAULT 0, nudge_cancelled_at INTEGER, nudge_cancel_reason TEXT NOT NULL DEFAULT '', regular_send INTEGER NOT NULL DEFAULT 0, started_at INTEGER, processed_at INTEGER, nudge_discarded_at INTEGER) STRICT;

CREATE INDEX idx_agent_messages_to_conv
			ON agent_messages(to_conv, created_at);

CREATE INDEX idx_agent_messages_parent
			ON agent_messages(parent_id);

CREATE INDEX idx_agent_messages_to_agent ON agent_messages(to_agent);

CREATE INDEX idx_agent_messages_regular_agent_backlog
			ON agent_messages(to_agent, regular_send, processed_at) WHERE pin_gen = 0;

CREATE INDEX idx_agent_messages_regular_conv_backlog
			ON agent_messages(to_conv, regular_send, processed_at);

CREATE TABLE "agent_transfer_log" (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			kind           TEXT NOT NULL,
			at             INTEGER NOT NULL,
			format_version INTEGER NOT NULL DEFAULT 0,
			source_group   TEXT NOT NULL DEFAULT '',
			source_home    TEXT NOT NULL DEFAULT '',
			source_os      TEXT NOT NULL DEFAULT '',
			result_group   TEXT NOT NULL DEFAULT '',
			target_dir     TEXT NOT NULL DEFAULT '',
			conv_remaps    TEXT NOT NULL DEFAULT '',
			agent_count    INTEGER NOT NULL DEFAULT 0,
			message_count  INTEGER NOT NULL DEFAULT 0,
			by_conv        TEXT NOT NULL DEFAULT '',
			note           TEXT NOT NULL DEFAULT ''
		, by_agent TEXT NOT NULL DEFAULT '') STRICT;

CREATE INDEX idx_agent_transfer_log_at
			ON agent_transfer_log(at);

CREATE TABLE "group_templates" (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT NOT NULL UNIQUE,
			descr           TEXT NOT NULL DEFAULT '',
			default_context TEXT NOT NULL DEFAULT '',
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL
		, work_pattern TEXT NOT NULL DEFAULT '', process TEXT NOT NULL DEFAULT '', rhythms TEXT NOT NULL DEFAULT '', wave_max_wait INTEGER NOT NULL DEFAULT 0, per_agent_worktrees INTEGER NOT NULL DEFAULT 0, owner_scopes_json TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(owner_scopes_json AS BLOB)) BETWEEN 0 AND 262144)) STRICT;

CREATE TABLE "human_messages" (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			from_conv   TEXT NOT NULL,
			from_title  TEXT NOT NULL DEFAULT '',
			group_name  TEXT NOT NULL DEFAULT '',
			subject     TEXT NOT NULL DEFAULT '',
			body        TEXT NOT NULL,
			created_at  INTEGER NOT NULL,
			read_at     INTEGER
		, from_agent TEXT NOT NULL DEFAULT '', process_run_id TEXT NOT NULL DEFAULT '', process_node_id TEXT NOT NULL DEFAULT '', process_command_id TEXT NOT NULL DEFAULT '') STRICT;

CREATE INDEX idx_human_messages_created
			ON human_messages(created_at);

CREATE TABLE "conv_branch_history" (
			conv_id    TEXT NOT NULL,
			repo_dir   TEXT NOT NULL DEFAULT '',
			branch     TEXT NOT NULL,
			pr_number  INTEGER NOT NULL DEFAULT 0,
			pr_url     TEXT NOT NULL DEFAULT '',
			pr_state   TEXT NOT NULL DEFAULT '',
			source     TEXT NOT NULL DEFAULT 'scan',
			first_seen INTEGER,
			last_seen  INTEGER,
			PRIMARY KEY (conv_id, repo_dir, branch)
		) STRICT;

CREATE TABLE "agent_workspace" (
			conv_id        TEXT PRIMARY KEY,
			cwd            TEXT NOT NULL DEFAULT '',
			branch         TEXT NOT NULL DEFAULT '',
			repo_url       TEXT NOT NULL DEFAULT '',
			default_branch TEXT NOT NULL DEFAULT '',
			pr_number      INTEGER NOT NULL DEFAULT 0,
			pr_url         TEXT NOT NULL DEFAULT '',
			pr_state       TEXT NOT NULL DEFAULT '',
			updated_at     INTEGER
		, agent_id TEXT NOT NULL DEFAULT '') STRICT;

CREATE TABLE "session_cost_daily" (
			session_id TEXT NOT NULL,
			day        TEXT NOT NULL,
			conv_id    TEXT NOT NULL DEFAULT '',
			cost_usd   REAL NOT NULL DEFAULT 0, updated_at INTEGER, virtual_cost_usd REAL NOT NULL DEFAULT 0, model TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '', harness TEXT NOT NULL DEFAULT 'claude',
			PRIMARY KEY (session_id, day)
		) STRICT;

CREATE INDEX idx_session_cost_daily_day ON session_cost_daily(day);

CREATE INDEX idx_session_cost_daily_walk
			ON session_cost_daily(
				COALESCE(NULLIF(conv_id, ''), session_id), day, updated_at, session_id
			);

CREATE TABLE "dashboard_prefs" (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		) STRICT;

CREATE TRIGGER stable_ref_global_profile_insert
			AFTER INSERT ON dashboard_prefs WHEN NEW.key = 'tclaude.dash.default_profile' BEGIN
				DELETE FROM dashboard_prefs WHERE key = 'tclaude.dash.default_profile_id';
				INSERT INTO dashboard_prefs (key, value, updated_at)
					SELECT 'tclaude.dash.default_profile_id', CAST(id AS TEXT), NEW.updated_at
					  FROM spawn_profiles WHERE name = NEW.value;
			END;

CREATE TRIGGER stable_ref_global_profile_update
			AFTER UPDATE OF value ON dashboard_prefs WHEN NEW.key = 'tclaude.dash.default_profile' BEGIN
				DELETE FROM dashboard_prefs WHERE key = 'tclaude.dash.default_profile_id';
				INSERT INTO dashboard_prefs (key, value, updated_at)
					SELECT 'tclaude.dash.default_profile_id', CAST(id AS TEXT), NEW.updated_at
					  FROM spawn_profiles WHERE name = NEW.value;
			END;

CREATE TRIGGER stable_ref_global_profile_delete
			AFTER DELETE ON dashboard_prefs WHEN OLD.key = 'tclaude.dash.default_profile' BEGIN
				DELETE FROM dashboard_prefs WHERE key = 'tclaude.dash.default_profile_id';
			END;

CREATE TABLE "pending_spawns" (
			label           TEXT PRIMARY KEY,
			group_id        INTEGER NOT NULL,
			role            TEXT NOT NULL DEFAULT '',
			descr           TEXT NOT NULL DEFAULT '',
			name            TEXT NOT NULL DEFAULT '',
			initial_message TEXT NOT NULL DEFAULT '',
			group_context   TEXT NOT NULL DEFAULT '',
			reply_to_conv   TEXT NOT NULL DEFAULT '',
			spawned_by_conv TEXT NOT NULL DEFAULT '',
			worktree_path   TEXT NOT NULL DEFAULT '',
			worktree_branch TEXT NOT NULL DEFAULT '',
			created_at      INTEGER NOT NULL
		, reply_to_agent TEXT NOT NULL DEFAULT '', spawned_by_agent TEXT NOT NULL DEFAULT '', is_owner INTEGER NOT NULL DEFAULT 0, permission_overrides TEXT NOT NULL DEFAULT '', process_command_id TEXT NOT NULL DEFAULT '', effective_sandbox_config TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '', launching INTEGER NOT NULL DEFAULT 0, task_url TEXT NOT NULL DEFAULT '', task_label TEXT NOT NULL DEFAULT '', profile_context TEXT NOT NULL DEFAULT '') STRICT;

CREATE UNIQUE INDEX idx_pending_spawns_process_command ON pending_spawns(process_command_id) WHERE process_command_id <> '';

CREATE UNIQUE INDEX idx_pending_spawns_agent
			ON pending_spawns(agent_id) WHERE agent_id <> '';

CREATE TABLE "spawn_profiles" (
			id                            INTEGER PRIMARY KEY AUTOINCREMENT,
			name                          TEXT NOT NULL UNIQUE,
			harness                       TEXT NOT NULL DEFAULT '',
			model                         TEXT NOT NULL DEFAULT '',
			effort                        TEXT NOT NULL DEFAULT '',
			sandbox                       TEXT NOT NULL DEFAULT '',
			approval                      TEXT NOT NULL DEFAULT '',
			auto_review                   INTEGER,
			trust_dir                     INTEGER,
			agent_name                    TEXT NOT NULL DEFAULT '',
			role                          TEXT NOT NULL DEFAULT '',
			descr                         TEXT NOT NULL DEFAULT '',
			initial_message               TEXT NOT NULL DEFAULT '',
			sync_worktree                 INTEGER,
			auto_focus                    INTEGER,
			include_group_default_context INTEGER,
			created_at                    INTEGER NOT NULL,
			updated_at                    INTEGER NOT NULL
		, remote_control INTEGER, is_owner INTEGER, permission_overrides TEXT NOT NULL DEFAULT '', ask_user_question_timeout TEXT NOT NULL DEFAULT '', disabled_reason TEXT NOT NULL DEFAULT '', disabled INTEGER NOT NULL DEFAULT 0, auto_memory INTEGER, tools TEXT NOT NULL DEFAULT '', context_features TEXT NOT NULL DEFAULT '', auto_compact_window TEXT NOT NULL DEFAULT '', ssh_workaround INTEGER, sandbox_implementation TEXT NOT NULL DEFAULT '', operator_only INTEGER NOT NULL DEFAULT 0, startup_context TEXT NOT NULL DEFAULT '', context_window_max INTEGER NOT NULL DEFAULT 0, copilot_api INTEGER, fast_mode INTEGER) STRICT;

CREATE TRIGGER spawn_profile_name_not_alias_insert
		BEFORE INSERT ON spawn_profiles
		WHEN EXISTS (SELECT 1 FROM spawn_profile_aliases WHERE alias = NEW.name)
		BEGIN
			SELECT RAISE(ABORT, 'spawn profile handle already exists');
		END;

CREATE TRIGGER spawn_profile_name_not_alias_update
		BEFORE UPDATE OF name ON spawn_profiles
		WHEN EXISTS (SELECT 1 FROM spawn_profile_aliases WHERE alias = NEW.name)
		BEGIN
			SELECT RAISE(ABORT, 'spawn profile handle already exists');
		END;

CREATE TABLE "ask_threads" (
			term_key   TEXT NOT NULL,
			cwd        TEXT NOT NULL,
			conv_id    TEXT NOT NULL,
			harness    TEXT NOT NULL DEFAULT 'claude',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL, agent_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (term_key, cwd)
		) STRICT;

CREATE TABLE "export_jobs" (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			conv_id       TEXT NOT NULL,
			group_name    TEXT NOT NULL DEFAULT '',
			title         TEXT NOT NULL DEFAULT '',
			instructions  TEXT NOT NULL DEFAULT '',
			preset        TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL,
			error         TEXT NOT NULL DEFAULT '',
			artifact_path TEXT NOT NULL DEFAULT '',
			artifact_name TEXT NOT NULL DEFAULT '',
			artifact_size INTEGER NOT NULL DEFAULT 0,
			content_type  TEXT NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		, worker_conv_id TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '', worker_agent_id TEXT NOT NULL DEFAULT '') STRICT;

CREATE INDEX idx_export_jobs_conv
			ON export_jobs(conv_id);

CREATE TABLE "audit_log" (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			at           INTEGER NOT NULL,
			actor_kind   TEXT NOT NULL DEFAULT '',
			actor_conv   TEXT NOT NULL DEFAULT '',
			actor_label  TEXT NOT NULL DEFAULT '',
			verb         TEXT NOT NULL DEFAULT '',
			target_conv  TEXT NOT NULL DEFAULT '',
			target_label TEXT NOT NULL DEFAULT '',
			group_name   TEXT NOT NULL DEFAULT '',
			detail       TEXT NOT NULL DEFAULT '',
			method       TEXT NOT NULL DEFAULT '',
			path         TEXT NOT NULL DEFAULT '',
			status       INTEGER NOT NULL DEFAULT 0,
			source       TEXT NOT NULL DEFAULT ''
		, actor_agent TEXT NOT NULL DEFAULT '', target_agent TEXT NOT NULL DEFAULT '', event_id TEXT NOT NULL DEFAULT '', related_event_id TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '', tmux_session TEXT NOT NULL DEFAULT '', pane_id TEXT NOT NULL DEFAULT '', observer TEXT NOT NULL DEFAULT '', cause_kind TEXT NOT NULL DEFAULT '', observed_process TEXT NOT NULL DEFAULT '', launch_phase TEXT NOT NULL DEFAULT '', exit_code INTEGER, signal TEXT NOT NULL DEFAULT '', lifecycle_action TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '', observed_state TEXT NOT NULL DEFAULT '', dedup_key TEXT NOT NULL DEFAULT '') STRICT;

CREATE INDEX idx_audit_log_at
			ON audit_log(at);

CREATE UNIQUE INDEX idx_audit_log_exit_dedup
			ON audit_log(dedup_key) WHERE dedup_key <> '';

CREATE INDEX idx_audit_log_event_id
			ON audit_log(event_id) WHERE event_id <> '';

CREATE TABLE "agents" (
			agent_id        TEXT PRIMARY KEY,
			current_conv_id TEXT NOT NULL UNIQUE,
			created_at      INTEGER NOT NULL,
			created_via     TEXT NOT NULL DEFAULT '',
			retired_at      INTEGER,
			retired_by      TEXT NOT NULL DEFAULT '',
			retire_reason   TEXT NOT NULL DEFAULT '',
			pending_name    TEXT NOT NULL DEFAULT ''
		, retired_by_agent TEXT NOT NULL DEFAULT '', initial_spawn_config TEXT NOT NULL DEFAULT '', task_ref_url TEXT NOT NULL DEFAULT '', task_ref_label TEXT NOT NULL DEFAULT '', process_command_id TEXT NOT NULL DEFAULT '', effective_sandbox_config TEXT NOT NULL DEFAULT '', relaunch_profile TEXT NOT NULL DEFAULT '') STRICT;

CREATE UNIQUE INDEX idx_agents_process_command ON agents(process_command_id) WHERE process_command_id <> '';

CREATE TABLE "agent_conversations" (
			conv_id   TEXT PRIMARY KEY,
			agent_id  TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			role      TEXT NOT NULL DEFAULT '',
			reason    TEXT NOT NULL DEFAULT '',
			linked_at INTEGER NOT NULL
		) STRICT;

CREATE INDEX idx_agent_conversations_agent
			ON agent_conversations(agent_id);

CREATE TABLE "agent_group_members" (
				group_id  INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
				agent_id  TEXT NOT NULL,
				role      TEXT NOT NULL DEFAULT '',
				descr     TEXT NOT NULL DEFAULT '',
				joined_at INTEGER NOT NULL,
				PRIMARY KEY (group_id, agent_id)
			) STRICT;

CREATE INDEX idx_agent_group_members_agent
				ON agent_group_members(agent_id);

CREATE TABLE "agent_group_owners" (
				group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
				agent_id   TEXT NOT NULL,
				granted_at INTEGER NOT NULL,
				granted_by TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (group_id, agent_id)
			) STRICT;

CREATE INDEX idx_agent_group_owners_agent
				ON agent_group_owners(agent_id);

CREATE TABLE "agent_permissions" (
				agent_id   TEXT NOT NULL,
				slug       TEXT NOT NULL,
				granted_at INTEGER NOT NULL,
				granted_by TEXT NOT NULL DEFAULT '',
				effect     TEXT NOT NULL DEFAULT 'grant' CHECK (effect IN ('grant', 'deny')), scope_json TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(scope_json AS BLOB)) BETWEEN 0 AND 262144),
				PRIMARY KEY (agent_id, slug)
			) STRICT;

CREATE INDEX idx_agent_permissions_slug
				ON agent_permissions(slug);

CREATE TABLE "agent_sudo_grants" (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				agent_id    TEXT NOT NULL,
				slug        TEXT NOT NULL,
				granted_at  INTEGER NOT NULL,
				expires_at  INTEGER NOT NULL,
				granted_by  TEXT NOT NULL,
				reason      TEXT NOT NULL DEFAULT '',
				revoked_at  INTEGER
			, scope_json TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(scope_json AS BLOB)) BETWEEN 0 AND 262144)) STRICT;

CREATE INDEX idx_sudo_active
				ON agent_sudo_grants(agent_id, expires_at) WHERE revoked_at IS NULL;

CREATE TABLE "agent_notify_prefs" (
				agent_id   TEXT PRIMARY KEY,
				mode       TEXT NOT NULL CHECK (mode IN ('on', 'off')),
				updated_at INTEGER NOT NULL
			) STRICT;

CREATE TABLE "roles" (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT NOT NULL UNIQUE,
			descr         TEXT NOT NULL DEFAULT '',
			brief         TEXT NOT NULL DEFAULT '',
			spawn_profile TEXT NOT NULL DEFAULT '',
			harness       TEXT NOT NULL DEFAULT '',
			model         TEXT NOT NULL DEFAULT '',
			effort        TEXT NOT NULL DEFAULT '',
			sandbox       TEXT NOT NULL DEFAULT '',
			approval      TEXT NOT NULL DEFAULT '',
			permissions   TEXT NOT NULL DEFAULT '[]',
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		, spawn_profile_id INTEGER, tools TEXT NOT NULL DEFAULT '') STRICT;

CREATE INDEX idx_roles_spawn_profile_id ON roles(spawn_profile_id);

CREATE TRIGGER stable_ref_role_profile_insert
			AFTER INSERT ON roles BEGIN
				UPDATE roles SET spawn_profile_id = COALESCE(NEW.spawn_profile_id,
					(SELECT id FROM spawn_profiles WHERE name = NEW.spawn_profile))
				 WHERE id = NEW.id;
			END;

CREATE TRIGGER stable_ref_role_profile_update
			AFTER UPDATE OF spawn_profile ON roles
			WHEN NEW.spawn_profile IS NOT OLD.spawn_profile BEGIN
				UPDATE roles SET spawn_profile_id = CASE
					WHEN NEW.spawn_profile_id IS NOT OLD.spawn_profile_id THEN NEW.spawn_profile_id
					ELSE (SELECT id FROM spawn_profiles WHERE name = NEW.spawn_profile) END
				 WHERE id = NEW.id;
			END;

CREATE TABLE "group_process_state" (
			group_id         INTEGER PRIMARY KEY,
			process          TEXT NOT NULL DEFAULT '[]',
			current_phase    TEXT NOT NULL DEFAULT '',
			phase_started_at INTEGER NOT NULL
		) STRICT;

CREATE TABLE "group_process_transitions" (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id   INTEGER NOT NULL,
			from_phase TEXT NOT NULL DEFAULT '',
			to_phase   TEXT NOT NULL,
			at         INTEGER NOT NULL,
			actor      TEXT NOT NULL DEFAULT ''
		) STRICT;

CREATE INDEX idx_group_process_transitions_group
			ON group_process_transitions(group_id);

CREATE TABLE "group_wave_choreography" (
			group_id   INTEGER PRIMARY KEY,
			group_name TEXT NOT NULL,
			state      TEXT NOT NULL DEFAULT '{}',
			updated_at INTEGER NOT NULL
		) STRICT;

CREATE TABLE "access_requests" (
			id                TEXT PRIMARY KEY,
			perm              TEXT NOT NULL,
			conv_id           TEXT NOT NULL DEFAULT '',
			agent_id          TEXT NOT NULL DEFAULT '',
			conv_title        TEXT NOT NULL DEFAULT '',
			method            TEXT NOT NULL DEFAULT '',
			path              TEXT NOT NULL DEFAULT '',
			raw_query         TEXT NOT NULL DEFAULT '',
			body_preview      TEXT NOT NULL DEFAULT '',
			body_label        TEXT NOT NULL DEFAULT '',
			target_group      TEXT NOT NULL DEFAULT '',
			target_conv_id    TEXT NOT NULL DEFAULT '',
			target_conv_title TEXT NOT NULL DEFAULT '',
			auto_grantable    INTEGER NOT NULL DEFAULT 0,
			status            TEXT NOT NULL DEFAULT 'pending',
			created_at        INTEGER NOT NULL,
			deadline_at       INTEGER,
			decided_at        INTEGER
		) STRICT;

CREATE INDEX idx_access_requests_status_decided
			ON access_requests(status, decided_at, created_at);

CREATE TABLE "codex_usage_cache" (
			id          INTEGER PRIMARY KEY,
			data        TEXT NOT NULL DEFAULT '{}',
			observed_at INTEGER,
			updated_at  INTEGER,
			source      TEXT NOT NULL DEFAULT ''
		) STRICT;

CREATE TABLE "agent_prs" (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id    TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			pr_url      TEXT NOT NULL,
			summary     TEXT NOT NULL DEFAULT '',
			state       TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			UNIQUE(agent_id, pr_url)
		) STRICT;

CREATE INDEX idx_agent_prs_agent ON agent_prs(agent_id);

CREATE INDEX idx_agent_prs_state_updated ON agent_prs(state, updated_at);

CREATE TABLE "sandbox_profiles" (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			name             TEXT NOT NULL UNIQUE,
			filesystem_json  TEXT NOT NULL DEFAULT '[]',
			environment_json TEXT NOT NULL DEFAULT '[]',
			created_at       INTEGER NOT NULL,
			updated_at       INTEGER NOT NULL
		, includes_json TEXT NOT NULL DEFAULT '[]', agent_directories_json TEXT NOT NULL DEFAULT '[]', network_access TEXT NOT NULL DEFAULT '', network_json TEXT NOT NULL DEFAULT '', unix_sockets_json TEXT NOT NULL DEFAULT '', filesystem_spellings_json TEXT NOT NULL DEFAULT '', resource_limits_json TEXT NOT NULL DEFAULT '{}', darwin_allow_mach_register INTEGER NOT NULL DEFAULT 0 CHECK (darwin_allow_mach_register IN (0, 1)), pre_launch_json TEXT NOT NULL DEFAULT '[]') STRICT;

CREATE TABLE "agent_group_permissions" (
			group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			slug       TEXT NOT NULL,
			granted_at INTEGER NOT NULL,
			granted_by TEXT NOT NULL DEFAULT '', scope_json TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(scope_json AS BLOB)) BETWEEN 0 AND 262144),
			PRIMARY KEY (group_id, slug)
		) STRICT;

CREATE INDEX idx_agent_group_permissions_slug
			ON agent_group_permissions(slug);

CREATE TABLE "dashboard_session_grace" (
			token_hash TEXT PRIMARY KEY,
			expires_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		) STRICT;

CREATE INDEX idx_dashboard_session_grace_expiry
			ON dashboard_session_grace(expires_at);

CREATE TABLE "agentd_idempotency" (
			request_key TEXT PRIMARY KEY,
			fingerprint TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('pending', 'completed')),
			status INTEGER NOT NULL DEFAULT 0,
			headers_json TEXT NOT NULL DEFAULT '',
			response_body BLOB,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		) STRICT;

CREATE INDEX idx_agentd_idempotency_expiry
			ON agentd_idempotency(expires_at);

CREATE TABLE "spawn_harness_rules" (
			group_id       INTEGER NOT NULL DEFAULT 0,
			source_harness TEXT NOT NULL,
			target_harness TEXT NOT NULL,
			decision       TEXT NOT NULL CHECK (decision IN ('allow', 'deny')),
			reason         TEXT NOT NULL DEFAULT '',
			updated_at     INTEGER NOT NULL,
			PRIMARY KEY (group_id, source_harness, target_harness),
			CHECK (source_harness <> target_harness)
		) STRICT;

CREATE INDEX idx_spawn_harness_rules_group
			ON spawn_harness_rules(group_id);

CREATE TABLE "codex_telemetry_checkpoints" (
			session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
			data       TEXT NOT NULL,
			failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
			updated_at INTEGER NOT NULL
		) STRICT;

CREATE TABLE "subscription_usage_samples" (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			provider   TEXT NOT NULL,
			sampled_at INTEGER NOT NULL,
			UNIQUE(provider, sampled_at)
		) STRICT;

CREATE INDEX idx_subscription_usage_samples_sampled_at
			ON subscription_usage_samples(sampled_at);

CREATE TABLE "subscription_usage_windows" (
			sample_id        INTEGER NOT NULL REFERENCES subscription_usage_samples(id) ON DELETE CASCADE,
			window_name      TEXT NOT NULL,
			duration_seconds INTEGER NOT NULL DEFAULT 0,
			used_percent     REAL NOT NULL,
			resets_at        INTEGER,
			observed_at      INTEGER NOT NULL,
			source           TEXT NOT NULL DEFAULT '', excluded INTEGER NOT NULL DEFAULT 0 CHECK(excluded IN (0, 1)),
			PRIMARY KEY(sample_id, window_name)
		) STRICT;

CREATE TABLE "process_snippets" (
			id            TEXT PRIMARY KEY,
			name          TEXT NOT NULL,
			name_key      TEXT NOT NULL UNIQUE,
			envelope_json TEXT NOT NULL,
			revision      INTEGER NOT NULL CHECK(revision > 0),
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		) STRICT;

CREATE INDEX idx_process_snippets_order
			ON process_snippets(name_key, id);

CREATE TABLE "agent_recovery" (
			agent_id TEXT PRIMARY KEY REFERENCES agents(agent_id) ON DELETE CASCADE,
			conv_id TEXT NOT NULL,
			predecessor_session_id TEXT NOT NULL,
			predecessor_generation TEXT NOT NULL,
			exit_event_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			reason_code TEXT NOT NULL DEFAULT '',
			consecutive_crashes INTEGER NOT NULL DEFAULT 0 CHECK(consecutive_crashes >= 0),
			backoff_step INTEGER NOT NULL DEFAULT 0 CHECK(backoff_step >= 0),
			next_attempt_at INTEGER,
			backoff_seconds INTEGER NOT NULL DEFAULT 0 CHECK(backoff_seconds >= 0),
			lease_token TEXT NOT NULL DEFAULT '',
			lease_expires_at INTEGER,
			attempt_started_at INTEGER,
			successor_session_id TEXT NOT NULL DEFAULT '',
			successor_generation TEXT NOT NULL DEFAULT '',
			last_exit_code INTEGER,
			last_exit_signal TEXT NOT NULL DEFAULT '',
			last_exit_at INTEGER,
			recovered_at INTEGER,
			healthy_since INTEGER,
			notified_crash INTEGER NOT NULL DEFAULT 0,
			notified_backoff INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL
		) STRICT;

CREATE INDEX idx_agent_recovery_due
			ON agent_recovery(status, next_attempt_at);

CREATE TABLE "conversation_resume_profiles" (
		conv_id      TEXT PRIMARY KEY,
		profile_json TEXT NOT NULL,
		updated_at   INTEGER NOT NULL
	) STRICT;

CREATE TABLE "process_runs" (
			id                     TEXT NOT NULL PRIMARY KEY
			                           CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
			template_ref           TEXT NOT NULL
			                           CHECK(length(CAST(template_ref AS BLOB)) BETWEEN 1 AND 512),
			template_snapshot_json TEXT NOT NULL
			                           CHECK(length(CAST(template_snapshot_json AS BLOB)) BETWEEN 1 AND 4194304),
			params_json            TEXT NOT NULL
			                           CHECK(length(CAST(params_json AS BLOB)) BETWEEN 1 AND 262144),
			status                 TEXT NOT NULL
			                           CHECK(length(CAST(status AS BLOB)) BETWEEN 1 AND 64),
			state_version          INTEGER NOT NULL CHECK(state_version > 0),
			checkpoint_json        TEXT NOT NULL
			                           CHECK(length(CAST(checkpoint_json AS BLOB)) BETWEEN 1 AND 4194304),
			created_at             INTEGER NOT NULL
			                           CHECK(length(CAST(created_at AS BLOB)) BETWEEN 1 AND 64),
			updated_at             INTEGER NOT NULL
			                           CHECK(length(CAST(updated_at AS BLOB)) BETWEEN 1 AND 64)
		, program_authorizations_json TEXT NOT NULL DEFAULT '[]'
			CHECK(length(CAST(program_authorizations_json AS BLOB)) BETWEEN 2 AND 262144)) STRICT;

CREATE INDEX idx_process_runs_active
			ON process_runs(id)
			WHERE status NOT IN ('completed', 'failed', 'canceled');

CREATE TABLE "process_run_events" (
			run_id       TEXT NOT NULL REFERENCES process_runs(id) ON DELETE CASCADE
			                 CHECK(length(CAST(run_id AS BLOB)) BETWEEN 1 AND 128),
			sequence     INTEGER NOT NULL CHECK(sequence > 0),
			occurred_at  INTEGER NOT NULL
			                 CHECK(length(CAST(occurred_at AS BLOB)) BETWEEN 1 AND 64),
			node_id      TEXT NOT NULL DEFAULT ''
			                 CHECK(length(CAST(node_id AS BLOB)) <= 256),
			kind         TEXT NOT NULL
			                 CHECK(length(CAST(kind AS BLOB)) BETWEEN 1 AND 128),
			payload_json TEXT NOT NULL
			                 CHECK(length(CAST(payload_json AS BLOB)) BETWEEN 1 AND 262144),
			actor        TEXT NOT NULL DEFAULT ''
			                 CHECK(length(CAST(actor AS BLOB)) <= 256),
			PRIMARY KEY (run_id, sequence)
		) STRICT;

CREATE TABLE "browser_notifications" (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL DEFAULT '',
			title      TEXT NOT NULL,
			body       TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		) STRICT;

CREATE INDEX idx_browser_notifications_created
			ON browser_notifications(created_at);

CREATE TABLE "opencode_runtimes" (
			session_id TEXT PRIMARY KEY,
			conv_id    TEXT NOT NULL DEFAULT '',
			server_url TEXT NOT NULL,
			password   TEXT NOT NULL,
			pid        INTEGER NOT NULL DEFAULT 0,
			cwd        TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		, permission_json TEXT NOT NULL DEFAULT '', sandbox_implementation TEXT NOT NULL DEFAULT 'harness-builtin', sandbox_launch_spec_json TEXT NOT NULL DEFAULT '', transport TEXT NOT NULL DEFAULT 'loopback-tcp', control_socket_path TEXT NOT NULL DEFAULT '', control_socket_device INTEGER NOT NULL DEFAULT 0, control_socket_inode INTEGER NOT NULL DEFAULT 0, resource_cgroup_dir TEXT NOT NULL DEFAULT '') STRICT;

CREATE TABLE "opencode_usage_activity" (
			session_id  TEXT NOT NULL,
			message_id  TEXT NOT NULL,
			conv_id     TEXT NOT NULL DEFAULT '',
			provider_id TEXT NOT NULL,
			model_id    TEXT NOT NULL,
			observed_at INTEGER NOT NULL,
			PRIMARY KEY (session_id, message_id)
		) STRICT;

CREATE INDEX idx_opencode_usage_activity_observed
			ON opencode_usage_activity(observed_at, provider_id);

CREATE INDEX idx_opencode_usage_activity_conv_message
			ON opencode_usage_activity(conv_id, message_id);

CREATE TABLE "opencode_usage_step_removals" (
			conv_id    TEXT NOT NULL,
			message_id TEXT NOT NULL,
			removed_at INTEGER NOT NULL,
			PRIMARY KEY (conv_id, message_id)
		) STRICT;

CREATE INDEX idx_opencode_usage_step_removals_removed
			ON opencode_usage_step_removals(removed_at);

CREATE TABLE "opencode_agent_state_allocations" (
			agent_id  TEXT PRIMARY KEY,
			mode      TEXT NOT NULL CHECK (mode IN ('private', 'legacy-shared')),
			state_root TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			CHECK (
				(mode = 'private' AND state_root <> '') OR
				(mode = 'legacy-shared' AND state_root = '')
			)
		) STRICT;

CREATE TABLE "agent_standing_orders" (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			name              TEXT    NOT NULL DEFAULT '',
			revision          INTEGER NOT NULL DEFAULT 1,
			owner_agent       TEXT    NOT NULL DEFAULT '',
			target_kind       TEXT    NOT NULL DEFAULT 'conv',
			target_agent      TEXT    NOT NULL DEFAULT '',
			group_id          INTEGER NOT NULL DEFAULT 0,
			target_role       TEXT    NOT NULL DEFAULT '',
			summary           TEXT    NOT NULL DEFAULT '',
			trigger_event     TEXT    NOT NULL DEFAULT '',
			trigger_sources   TEXT    NOT NULL DEFAULT '',
			timing            TEXT    NOT NULL DEFAULT 'next-turn',
			cadence           TEXT    NOT NULL DEFAULT 'always',
			enabled           INTEGER NOT NULL DEFAULT 1,
			disabled_reason   TEXT    NOT NULL DEFAULT '',
			operator_authored INTEGER NOT NULL DEFAULT 0,
			created_at        INTEGER    NOT NULL,
			updated_at        INTEGER
		, cooldown_seconds INTEGER NOT NULL DEFAULT 0, match_field TEXT NOT NULL DEFAULT '', match_regex TEXT NOT NULL DEFAULT '', row_version INTEGER NOT NULL DEFAULT 1, debounce_seconds INTEGER NOT NULL DEFAULT 0) STRICT;

CREATE INDEX idx_agent_standing_orders_owner
			ON agent_standing_orders(owner_agent);

CREATE INDEX idx_agent_standing_orders_group
			ON agent_standing_orders(group_id);

CREATE UNIQUE INDEX idx_agent_standing_orders_name
			ON agent_standing_orders(name);

CREATE INDEX idx_agent_standing_orders_enabled_trigger
		ON agent_standing_orders(enabled, trigger_event);

CREATE TABLE "agent_standing_order_deliveries" (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			order_id       INTEGER NOT NULL,
			order_revision INTEGER NOT NULL DEFAULT 0,
			target_conv    TEXT    NOT NULL DEFAULT '',
			epoch          TEXT    NOT NULL DEFAULT '',
			outcome        TEXT    NOT NULL DEFAULT '',
			transport      TEXT    NOT NULL DEFAULT '',
			harness        TEXT    NOT NULL DEFAULT '',
			detail         TEXT    NOT NULL DEFAULT '',
			created_at     INTEGER    NOT NULL
		, target_agent TEXT NOT NULL DEFAULT '') STRICT;

CREATE INDEX idx_agent_standing_order_deliveries_order
			ON agent_standing_order_deliveries(order_id, created_at);

CREATE INDEX idx_agent_standing_order_deliveries_epoch
			ON agent_standing_order_deliveries(order_id, order_revision, target_conv, epoch);

CREATE INDEX idx_agent_standing_order_deliveries_cooldown
		ON agent_standing_order_deliveries(
			order_id, order_revision, target_agent, id
		);

CREATE TABLE "agent_standing_order_turn_origins" (
			target_agent TEXT PRIMARY KEY,
			target_conv  TEXT NOT NULL,
			message_id   INTEGER NOT NULL CHECK(message_id > 0),
			opencode_message_id TEXT NOT NULL,
			state        TEXT NOT NULL CHECK(state IN ('pending', 'active')),
			armed_at     INTEGER NOT NULL,
			expires_at   INTEGER NOT NULL
		) STRICT;

CREATE TABLE "agent_standing_order_debounce" (
			order_id       INTEGER NOT NULL
			               REFERENCES agent_standing_orders(id) ON DELETE CASCADE,
			order_revision INTEGER NOT NULL,
			target_agent   TEXT NOT NULL,
			target_conv    TEXT NOT NULL,
			epoch          TEXT NOT NULL DEFAULT '',
			harness        TEXT NOT NULL,
			detail         TEXT NOT NULL DEFAULT '',
			due_at         INTEGER NOT NULL,
			max_due_at     INTEGER NOT NULL,
			updated_at     INTEGER NOT NULL,
			PRIMARY KEY (order_id, target_agent)
		) STRICT;

CREATE INDEX idx_agent_standing_order_debounce_due
			ON agent_standing_order_debounce(due_at);

CREATE TABLE "agent_standing_order_group_scopes" (
			order_id  INTEGER NOT NULL
			          REFERENCES agent_standing_orders(id) ON DELETE CASCADE,
			group_id  INTEGER NOT NULL
			          REFERENCES agent_groups(id) ON DELETE CASCADE,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (order_id, group_id)
		) STRICT;

CREATE INDEX idx_agent_standing_order_group_scopes_group
			ON agent_standing_order_group_scopes(group_id, order_id);

CREATE TABLE agent_routes (
			id TEXT PRIMARY KEY,
			group_id INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			publisher_agent_id TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			publisher_conv_id TEXT NOT NULL DEFAULT '',
			publisher_launch_generation TEXT NOT NULL,
			group_generation INTEGER NOT NULL,
			name TEXT NOT NULL,
			transport TEXT NOT NULL,
			target TEXT NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('ready', 'draining', 'withdrawn', 'publisher-lost')),
			created_at INTEGER NOT NULL,
			withdrawn_at INTEGER,
			withdraw_reason TEXT NOT NULL DEFAULT '',
			UNIQUE(group_id, publisher_agent_id, name)
		) STRICT;

CREATE INDEX idx_agent_routes_group_state ON agent_routes(group_id, state);

CREATE INDEX idx_agent_routes_publisher ON agent_routes(publisher_agent_id, state);

CREATE TABLE agent_route_leases (
			id TEXT PRIMARY KEY,
			route_id TEXT NOT NULL REFERENCES agent_routes(id) ON DELETE CASCADE,
			consumer_agent_id TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			consumer_conv_id TEXT NOT NULL DEFAULT '',
			consumer_launch_generation TEXT NOT NULL,
			group_generation INTEGER NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('open', 'closed')),
			opened_at INTEGER NOT NULL,
			closed_at INTEGER
		) STRICT;

CREATE INDEX idx_agent_route_leases_route_state ON agent_route_leases(route_id, state);

CREATE INDEX idx_agent_route_leases_consumer ON agent_route_leases(consumer_agent_id, state);

CREATE TABLE agent_route_audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			at INTEGER NOT NULL,
			action TEXT NOT NULL,
			result TEXT NOT NULL,
			group_id INTEGER,
			route_id TEXT NOT NULL DEFAULT '',
			lease_id TEXT NOT NULL DEFAULT '',
			actor_agent_id TEXT NOT NULL DEFAULT '',
			actor_conv_id TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT ''
		) STRICT;

CREATE INDEX idx_agent_route_audit_route ON agent_route_audit(route_id, at);

CREATE TABLE "human_message_attachments" (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id   INTEGER NOT NULL REFERENCES human_messages(id) ON DELETE CASCADE,
			seq          INTEGER NOT NULL DEFAULT 0,
			filename     TEXT NOT NULL,
			content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
			size_bytes   INTEGER NOT NULL,
			storage_path TEXT NOT NULL
		) STRICT;

CREATE INDEX idx_human_message_attachments_message
		ON human_message_attachments(message_id, seq, id);

CREATE TABLE darwin_route_launches (
			agent_id TEXT NOT NULL,
			conv_id TEXT NOT NULL,
			launch_generation TEXT NOT NULL,
			slots TEXT NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('pending', 'active', 'closed')),
			created_at INTEGER NOT NULL,
			closed_at INTEGER,
			PRIMARY KEY(agent_id, conv_id, launch_generation)
		) STRICT;

CREATE INDEX idx_darwin_route_launches_identity
			ON darwin_route_launches(agent_id, conv_id, launch_generation, state);

CREATE TABLE darwin_route_slot_claims (
			slot INTEGER NOT NULL CHECK(slot BETWEEN 1 AND 65535),
			agent_id TEXT NOT NULL,
			conv_id TEXT NOT NULL,
			launch_generation TEXT NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('pending', 'active')),
			created_at INTEGER NOT NULL,
			PRIMARY KEY(slot),
			UNIQUE(agent_id, conv_id, launch_generation, slot)
		) STRICT;

CREATE INDEX idx_darwin_route_slot_claims_identity
			ON darwin_route_slot_claims(agent_id, conv_id, launch_generation, state);

CREATE TABLE copilot_usage_snapshots (
			session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
			conv_id TEXT NOT NULL,
			last_event_id INTEGER NOT NULL CHECK (last_event_id >= 0),
			last_turn_index INTEGER NOT NULL DEFAULT 0,
			model TEXT NOT NULL DEFAULT '',
			reasoning_effort TEXT NOT NULL DEFAULT '',
			finish_reason TEXT NOT NULL DEFAULT '',
			requests INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			total_nano_aiu INTEGER,
			request_multiplier REAL,
			last_call_input_tokens INTEGER NOT NULL DEFAULT 0,
			last_call_output_tokens INTEGER NOT NULL DEFAULT 0,
			last_call_cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			last_call_cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			last_duration_ms INTEGER NOT NULL DEFAULT 0,
			last_time_to_first_token_ms INTEGER NOT NULL DEFAULT 0,
			last_inter_token_latency_ms INTEGER NOT NULL DEFAULT 0,
			last_call_stamp_text TEXT NOT NULL DEFAULT '',
			observed_at INTEGER NOT NULL
		, fold_version INTEGER NOT NULL) STRICT;

CREATE INDEX idx_copilot_usage_snapshots_conv
			ON copilot_usage_snapshots(conv_id);

CREATE TABLE copilot_api_runtimes (
			conv_id TEXT PRIMARY KEY,
			port INTEGER NOT NULL CHECK (port > 0 AND port <= 65535),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		) STRICT
	;

CREATE TABLE agent_cron_messages (
			message_id INTEGER PRIMARY KEY
			           REFERENCES agent_messages(id) ON DELETE CASCADE,
			cron_job_id INTEGER NOT NULL CHECK(cron_job_id > 0)
		);

CREATE INDEX idx_agent_cron_messages_job
			ON agent_cron_messages(cron_job_id);

CREATE TABLE agent_lineage (
			child_agent_id  TEXT PRIMARY KEY,
			parent_agent_id TEXT NOT NULL,
			spawned_at      INTEGER NOT NULL
		) STRICT;

CREATE INDEX idx_agent_lineage_parent
			ON agent_lineage(parent_agent_id, child_agent_id);

CREATE TABLE copilot_model_catalog (
			model_id                  TEXT PRIMARY KEY,
			max_context_window_tokens INTEGER NOT NULL DEFAULT 0 CHECK (max_context_window_tokens >= 0),
			max_prompt_tokens         INTEGER NOT NULL DEFAULT 0 CHECK (max_prompt_tokens >= 0),
			max_output_tokens         INTEGER NOT NULL DEFAULT 0 CHECK (max_output_tokens >= 0),
			fetched_at                INTEGER NOT NULL,
			raw_json                  TEXT NOT NULL DEFAULT ''
		) STRICT;

