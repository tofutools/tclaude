CREATE TABLE schema_version (version INTEGER NOT NULL);

CREATE TABLE sessions (
			id              TEXT PRIMARY KEY,
			tmux_session    TEXT NOT NULL DEFAULT '',
			pid             INTEGER NOT NULL DEFAULT 0,
			cwd             TEXT NOT NULL DEFAULT '',
			conv_id         TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'idle',
			status_detail   TEXT NOT NULL DEFAULT '',
			auto_registered INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		, context_pct REAL NOT NULL DEFAULT 0, subagent_count INTEGER NOT NULL DEFAULT 0, last_hook TEXT NOT NULL DEFAULT '', tokens_input INTEGER NOT NULL DEFAULT 0, tokens_output INTEGER NOT NULL DEFAULT 0, context_window_size INTEGER NOT NULL DEFAULT 0, nudged_pct REAL NOT NULL DEFAULT 0, exit_reason TEXT, model TEXT NOT NULL DEFAULT '', effort_level TEXT NOT NULL DEFAULT '', pending_conv TEXT NOT NULL DEFAULT '', cost_usd REAL NOT NULL DEFAULT 0, model_id TEXT NOT NULL DEFAULT '', harness TEXT NOT NULL DEFAULT 'claude', sandbox_mode TEXT NOT NULL DEFAULT '', remote_control INTEGER NOT NULL DEFAULT 0, virtual_cost_usd REAL NOT NULL DEFAULT 0, agent_id TEXT NOT NULL DEFAULT '', last_statusline_json TEXT NOT NULL DEFAULT '', subagents_json TEXT NOT NULL DEFAULT '', ask_user_question_timeout TEXT NOT NULL DEFAULT '', effective_sandbox_config TEXT NOT NULL DEFAULT '');

CREATE INDEX idx_sessions_conv_id ON sessions(conv_id);

CREATE INDEX idx_sessions_status_updated ON sessions(status, updated_at);

CREATE TABLE notify_state (
			session_id  TEXT PRIMARY KEY,
			notified_at TEXT NOT NULL
		);

CREATE TABLE usage_cache (
			id              INTEGER PRIMARY KEY,
			data            TEXT NOT NULL DEFAULT '{}',
			fetched_at      TEXT NOT NULL DEFAULT '',
			last_attempt_at TEXT NOT NULL DEFAULT ''
		);

CREATE TABLE git_cache (
			repo_hash  TEXT PRIMARY KEY,
			data       TEXT NOT NULL DEFAULT '{}',
			fetched_at TEXT NOT NULL DEFAULT ''
		);

CREATE TABLE conv_index (
			conv_id       TEXT PRIMARY KEY,
			project_dir   TEXT NOT NULL,
			full_path     TEXT NOT NULL,
			file_mtime    INTEGER NOT NULL DEFAULT 0,
			file_size     INTEGER NOT NULL DEFAULT 0,
			first_prompt  TEXT NOT NULL DEFAULT '',
			summary       TEXT NOT NULL DEFAULT '',
			custom_title  TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			created       TEXT NOT NULL DEFAULT '',
			modified      TEXT NOT NULL DEFAULT '',
			git_branch    TEXT NOT NULL DEFAULT '',
			project_path  TEXT NOT NULL DEFAULT '',
			is_sidechain  INTEGER NOT NULL DEFAULT 0,
			indexed_at    TEXT NOT NULL DEFAULT ''
		, archived_at TEXT NOT NULL DEFAULT '', git_branch_startup TEXT NOT NULL DEFAULT '', harness TEXT NOT NULL DEFAULT 'claude');

CREATE INDEX idx_conv_index_project_dir ON conv_index(project_dir);

CREATE INDEX idx_conv_index_archived
			ON conv_index(archived_at);

CREATE TABLE conv_embeddings (
			conv_id     TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			chunk_type  TEXT NOT NULL DEFAULT 'content',
			chunk_text  TEXT NOT NULL DEFAULT '',
			embedding   BLOB NOT NULL,
			model       TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (conv_id, chunk_index)
		);

CREATE INDEX idx_conv_embeddings_conv_id ON conv_embeddings(conv_id);

CREATE TABLE agent_groups (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE,
			descr       TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL
		, archived_at TEXT NOT NULL DEFAULT '', default_cwd TEXT NOT NULL DEFAULT '', default_context TEXT NOT NULL DEFAULT '', max_members INTEGER NOT NULL DEFAULT 0, notify_enabled INTEGER NOT NULL DEFAULT 1, default_profile TEXT NOT NULL DEFAULT '', remote_control INTEGER, mission TEXT NOT NULL DEFAULT '', source_template TEXT NOT NULL DEFAULT '', parent_id INTEGER REFERENCES agent_groups(id) ON DELETE SET NULL, default_profile_id INTEGER, source_template_id INTEGER, sandbox_profile TEXT NOT NULL DEFAULT '', sandbox_profile_id INTEGER);

CREATE INDEX idx_agent_groups_archived
			ON agent_groups(archived_at);

CREATE INDEX idx_agent_groups_default_profile_id ON agent_groups(default_profile_id);

CREATE INDEX idx_agent_groups_source_template_id ON agent_groups(source_template_id);

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

CREATE INDEX idx_agent_groups_sandbox_profile_id ON agent_groups(sandbox_profile_id);

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

CREATE TABLE agent_cron_jobs (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			name             TEXT NOT NULL DEFAULT '',
			owner_agent       TEXT NOT NULL,
			target_agent      TEXT NOT NULL,
			group_id         INTEGER NOT NULL DEFAULT 0,
			interval_seconds INTEGER NOT NULL,
			subject          TEXT NOT NULL DEFAULT '',
			body             TEXT NOT NULL DEFAULT '',
			enabled          INTEGER NOT NULL DEFAULT 1,
			created_at       TEXT NOT NULL,
			last_run_at      TEXT NOT NULL DEFAULT '',
			last_run_status  TEXT NOT NULL DEFAULT ''
		, target_kind TEXT NOT NULL DEFAULT 'conv'
			CHECK (target_kind IN ('conv', 'group')), cron_expr TEXT NOT NULL DEFAULT '', target_role TEXT NOT NULL DEFAULT '', disabled_reason TEXT NOT NULL DEFAULT '');

CREATE INDEX idx_agent_cron_jobs_owner ON agent_cron_jobs(owner_agent);

CREATE INDEX idx_agent_cron_jobs_target ON agent_cron_jobs(target_agent);

CREATE TABLE agent_cron_runs (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id    INTEGER NOT NULL REFERENCES agent_cron_jobs(id) ON DELETE CASCADE,
			fired_at  TEXT NOT NULL,
			status    TEXT NOT NULL DEFAULT '',
			error_msg TEXT NOT NULL DEFAULT ''
		);

CREATE INDEX idx_agent_cron_runs_job
			ON agent_cron_runs(job_id, fired_at DESC);

CREATE TABLE agent_conv_succession (
			old_conv_id   TEXT PRIMARY KEY,
			new_conv_id   TEXT NOT NULL,
			reason        TEXT NOT NULL DEFAULT '',
			succeeded_at  TEXT NOT NULL
		, agent_id TEXT NOT NULL DEFAULT '');

CREATE INDEX idx_agent_conv_succession_new
			ON agent_conv_succession(new_conv_id);

CREATE TABLE agent_clone_history (
			source_agent_id TEXT NOT NULL,
			cloned_at      TEXT NOT NULL
		);

CREATE INDEX idx_clone_history_source
			ON agent_clone_history(source_agent_id, cloned_at);

CREATE TABLE agent_group_audit (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			old_name   TEXT NOT NULL,
			new_name   TEXT NOT NULL,
			by_conv    TEXT NOT NULL DEFAULT '',
			at         TEXT NOT NULL
		, by_agent TEXT NOT NULL DEFAULT '');

CREATE INDEX idx_agent_group_audit_group
			ON agent_group_audit(group_id, at);

CREATE TABLE agent_head_aliases (
			handle         TEXT PRIMARY KEY,
			anchor_conv_id TEXT NOT NULL,
			created_at     TEXT NOT NULL,
			by_conv        TEXT NOT NULL DEFAULT ''
		, by_agent TEXT NOT NULL DEFAULT '', anchor_agent_id TEXT NOT NULL DEFAULT '');

CREATE INDEX idx_agent_head_aliases_anchor
			ON agent_head_aliases(anchor_conv_id);

CREATE TABLE agent_group_links (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			from_group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			to_group_id     INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			mode            TEXT    NOT NULL,
			created_at      TEXT    NOT NULL,
			by_conv         TEXT    NOT NULL DEFAULT '', by_agent TEXT NOT NULL DEFAULT '',
			UNIQUE (from_group_id, to_group_id, mode)
		);

CREATE INDEX idx_agent_group_links_from
			ON agent_group_links(from_group_id);

CREATE INDEX idx_agent_group_links_to
			ON agent_group_links(to_group_id);

CREATE TABLE agent_workdir (
			conv_id    TEXT PRIMARY KEY,
			dir        TEXT NOT NULL,
			updated_at TEXT NOT NULL
		, worktree_root TEXT NOT NULL DEFAULT '', branch        TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '');

CREATE TABLE agent_spawn_history (
			spawner_agent_id TEXT NOT NULL,
			spawned_at      TEXT NOT NULL
		);

CREATE INDEX idx_spawn_history_spawner
			ON agent_spawn_history(spawner_agent_id, spawned_at);

CREATE TABLE "agent_messages" (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id         INTEGER NOT NULL DEFAULT 0,
			from_conv        TEXT NOT NULL,
			to_conv          TEXT NOT NULL,
			subject          TEXT NOT NULL DEFAULT '',
			body             TEXT NOT NULL DEFAULT '',
			created_at       TEXT NOT NULL,
			delivered_at     TEXT NOT NULL DEFAULT '',
			read_at          TEXT NOT NULL DEFAULT '',
			parent_id        INTEGER NOT NULL DEFAULT 0,
			to_recipients    TEXT NOT NULL DEFAULT '',
			cc_recipients    TEXT NOT NULL DEFAULT '',
			original_to_conv TEXT NOT NULL DEFAULT ''
		, from_agent TEXT NOT NULL DEFAULT '', to_agent TEXT NOT NULL DEFAULT '', to_recipient_agents TEXT NOT NULL DEFAULT '', cc_recipient_agents TEXT NOT NULL DEFAULT '', pin_gen INTEGER NOT NULL DEFAULT 0, nudge_claimed_at TEXT NOT NULL DEFAULT '', nudge_attempted_at TEXT NOT NULL DEFAULT '', nudge_attempts INTEGER NOT NULL DEFAULT 0, nudge_cancelled_at TEXT NOT NULL DEFAULT '', nudge_cancel_reason TEXT NOT NULL DEFAULT '');

CREATE INDEX idx_agent_messages_to_conv
			ON agent_messages(to_conv, created_at);

CREATE INDEX idx_agent_messages_parent
			ON agent_messages(parent_id);

CREATE INDEX idx_agent_messages_to_agent ON agent_messages(to_agent);

CREATE TABLE agent_transfer_log (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			kind           TEXT NOT NULL,
			at             TEXT NOT NULL,
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
		, by_agent TEXT NOT NULL DEFAULT '');

CREATE INDEX idx_agent_transfer_log_at
			ON agent_transfer_log(at);

CREATE TABLE group_templates (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT NOT NULL UNIQUE,
			descr           TEXT NOT NULL DEFAULT '',
			default_context TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		, work_pattern TEXT NOT NULL DEFAULT '', process TEXT NOT NULL DEFAULT '', rhythms TEXT NOT NULL DEFAULT '', wave_max_wait INTEGER NOT NULL DEFAULT 0, per_agent_worktrees INTEGER NOT NULL DEFAULT 0);

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

CREATE TABLE human_messages (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			from_conv   TEXT NOT NULL,
			from_title  TEXT NOT NULL DEFAULT '',
			group_name  TEXT NOT NULL DEFAULT '',
			subject     TEXT NOT NULL DEFAULT '',
			body        TEXT NOT NULL,
			created_at  TEXT NOT NULL,
			read_at     TEXT NOT NULL DEFAULT ''
		, from_agent TEXT NOT NULL DEFAULT '', process_run_id TEXT NOT NULL DEFAULT '', process_node_id TEXT NOT NULL DEFAULT '', process_command_id TEXT NOT NULL DEFAULT '');

CREATE INDEX idx_human_messages_created
			ON human_messages(created_at);

CREATE TABLE conv_branch_history (
			conv_id    TEXT NOT NULL,
			repo_dir   TEXT NOT NULL DEFAULT '',
			branch     TEXT NOT NULL,
			pr_number  INTEGER NOT NULL DEFAULT 0,
			pr_url     TEXT NOT NULL DEFAULT '',
			pr_state   TEXT NOT NULL DEFAULT '',
			source     TEXT NOT NULL DEFAULT 'scan',
			first_seen TEXT NOT NULL DEFAULT '',
			last_seen  TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (conv_id, repo_dir, branch)
		);

CREATE TABLE agent_workspace (
			conv_id        TEXT PRIMARY KEY,
			cwd            TEXT NOT NULL DEFAULT '',
			branch         TEXT NOT NULL DEFAULT '',
			repo_url       TEXT NOT NULL DEFAULT '',
			default_branch TEXT NOT NULL DEFAULT '',
			pr_number      INTEGER NOT NULL DEFAULT 0,
			pr_url         TEXT NOT NULL DEFAULT '',
			pr_state       TEXT NOT NULL DEFAULT '',
			updated_at     TEXT NOT NULL DEFAULT ''
		, agent_id TEXT NOT NULL DEFAULT '');

CREATE TABLE session_cost_daily (
			session_id TEXT NOT NULL,
			day        TEXT NOT NULL,
			conv_id    TEXT NOT NULL DEFAULT '',
			cost_usd   REAL NOT NULL DEFAULT 0, updated_at TEXT NOT NULL DEFAULT '', virtual_cost_usd REAL NOT NULL DEFAULT 0, model TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '', harness TEXT NOT NULL DEFAULT 'claude',
			PRIMARY KEY (session_id, day)
		);

CREATE INDEX idx_session_cost_daily_day ON session_cost_daily(day);

CREATE TABLE dashboard_prefs (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);

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

CREATE TABLE pending_spawns (
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
			created_at      TEXT NOT NULL
		, reply_to_agent TEXT NOT NULL DEFAULT '', spawned_by_agent TEXT NOT NULL DEFAULT '', is_owner INTEGER NOT NULL DEFAULT 0, permission_overrides TEXT NOT NULL DEFAULT '', process_command_id TEXT NOT NULL DEFAULT '', effective_sandbox_config TEXT NOT NULL DEFAULT '');

CREATE UNIQUE INDEX idx_pending_spawns_process_command ON pending_spawns(process_command_id) WHERE process_command_id <> '';

CREATE TABLE spawn_profiles (
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
			created_at                    TEXT NOT NULL,
			updated_at                    TEXT NOT NULL
		, remote_control INTEGER, is_owner INTEGER, permission_overrides TEXT NOT NULL DEFAULT '', ask_user_question_timeout TEXT NOT NULL DEFAULT '');

CREATE TABLE ask_threads (
			term_key   TEXT NOT NULL,
			cwd        TEXT NOT NULL,
			conv_id    TEXT NOT NULL,
			harness    TEXT NOT NULL DEFAULT 'claude',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (term_key, cwd)
		);

CREATE TABLE export_jobs (
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
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL
		, worker_conv_id TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '', worker_agent_id TEXT NOT NULL DEFAULT '');

CREATE INDEX idx_export_jobs_conv
			ON export_jobs(conv_id);

CREATE TABLE audit_log (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			at           TEXT NOT NULL,
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
		, actor_agent TEXT NOT NULL DEFAULT '', target_agent TEXT NOT NULL DEFAULT '');

CREATE INDEX idx_audit_log_at
			ON audit_log(at);

CREATE TABLE agents (
			agent_id        TEXT PRIMARY KEY,
			current_conv_id TEXT NOT NULL UNIQUE,
			created_at      TEXT NOT NULL,
			created_via     TEXT NOT NULL DEFAULT '',
			retired_at      TEXT NOT NULL DEFAULT '',
			retired_by      TEXT NOT NULL DEFAULT '',
			retire_reason   TEXT NOT NULL DEFAULT '',
			pending_name    TEXT NOT NULL DEFAULT ''
		, retired_by_agent TEXT NOT NULL DEFAULT '', initial_spawn_config TEXT NOT NULL DEFAULT '', task_ref_url TEXT NOT NULL DEFAULT '', task_ref_label TEXT NOT NULL DEFAULT '', process_command_id TEXT NOT NULL DEFAULT '', effective_sandbox_config TEXT NOT NULL DEFAULT '');

CREATE UNIQUE INDEX idx_agents_process_command ON agents(process_command_id) WHERE process_command_id <> '';

CREATE TABLE agent_conversations (
			conv_id   TEXT PRIMARY KEY,
			agent_id  TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			role      TEXT NOT NULL DEFAULT '',
			reason    TEXT NOT NULL DEFAULT '',
			linked_at TEXT NOT NULL
		);

CREATE INDEX idx_agent_conversations_agent
			ON agent_conversations(agent_id);

CREATE TABLE "agent_group_members" (
				group_id  INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
				agent_id  TEXT NOT NULL,
				role      TEXT NOT NULL DEFAULT '',
				descr     TEXT NOT NULL DEFAULT '',
				joined_at TEXT NOT NULL,
				PRIMARY KEY (group_id, agent_id)
			);

CREATE INDEX idx_agent_group_members_agent
				ON agent_group_members(agent_id);

CREATE TABLE "agent_group_owners" (
				group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
				agent_id   TEXT NOT NULL,
				granted_at TEXT NOT NULL,
				granted_by TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (group_id, agent_id)
			);

CREATE INDEX idx_agent_group_owners_agent
				ON agent_group_owners(agent_id);

CREATE TABLE "agent_permissions" (
				agent_id   TEXT NOT NULL,
				slug       TEXT NOT NULL,
				granted_at TEXT NOT NULL,
				granted_by TEXT NOT NULL DEFAULT '',
				effect     TEXT NOT NULL DEFAULT 'grant' CHECK (effect IN ('grant', 'deny')),
				PRIMARY KEY (agent_id, slug)
			);

CREATE INDEX idx_agent_permissions_slug
				ON agent_permissions(slug);

CREATE TABLE "agent_sudo_grants" (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				agent_id    TEXT NOT NULL,
				slug        TEXT NOT NULL,
				granted_at  TEXT NOT NULL,
				expires_at  TEXT NOT NULL,
				granted_by  TEXT NOT NULL,
				reason      TEXT NOT NULL DEFAULT '',
				revoked_at  TEXT NOT NULL DEFAULT ''
			);

CREATE INDEX idx_sudo_active
				ON agent_sudo_grants(agent_id, expires_at) WHERE revoked_at = '';

CREATE TABLE "agent_notify_prefs" (
				agent_id   TEXT PRIMARY KEY,
				mode       TEXT NOT NULL CHECK (mode IN ('on', 'off')),
				updated_at TEXT NOT NULL
			);

CREATE TABLE roles (
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
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL
		, spawn_profile_id INTEGER);

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

CREATE TABLE group_process_state (
			group_id         INTEGER PRIMARY KEY,
			process          TEXT NOT NULL DEFAULT '[]',
			current_phase    TEXT NOT NULL DEFAULT '',
			phase_started_at TEXT NOT NULL
		);

CREATE TABLE group_process_transitions (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id   INTEGER NOT NULL,
			from_phase TEXT NOT NULL DEFAULT '',
			to_phase   TEXT NOT NULL,
			at         TEXT NOT NULL,
			actor      TEXT NOT NULL DEFAULT ''
		);

CREATE INDEX idx_group_process_transitions_group
			ON group_process_transitions(group_id);

CREATE TABLE group_wave_choreography (
			group_id   INTEGER PRIMARY KEY,
			group_name TEXT NOT NULL,
			state      TEXT NOT NULL DEFAULT '{}',
			updated_at TEXT NOT NULL
		);

CREATE TABLE agent_tags (
			agent_id TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			tag      TEXT NOT NULL,
			PRIMARY KEY (agent_id, tag)
		);

CREATE INDEX idx_agent_tags_tag ON agent_tags(tag);

CREATE TABLE access_requests (
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
			created_at        TEXT NOT NULL,
			deadline_at       TEXT NOT NULL DEFAULT '',
			decided_at        TEXT NOT NULL DEFAULT ''
		);

CREATE INDEX idx_access_requests_status_decided
			ON access_requests(status, decided_at, created_at);

CREATE TABLE codex_usage_cache (
			id          INTEGER PRIMARY KEY,
			data        TEXT NOT NULL DEFAULT '{}',
			observed_at TEXT NOT NULL DEFAULT '',
			updated_at  TEXT NOT NULL DEFAULT '',
			source      TEXT NOT NULL DEFAULT ''
		);

CREATE TABLE agent_prs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id    TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			pr_url      TEXT NOT NULL,
			summary     TEXT NOT NULL DEFAULT '',
			state       TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL,
			UNIQUE(agent_id, pr_url)
		);

CREATE INDEX idx_agent_prs_agent ON agent_prs(agent_id);

CREATE INDEX idx_agent_prs_state_updated ON agent_prs(state, updated_at);

CREATE TABLE sandbox_profiles (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			name             TEXT NOT NULL UNIQUE,
			filesystem_json  TEXT NOT NULL DEFAULT '[]',
			environment_json TEXT NOT NULL DEFAULT '[]',
			created_at       TEXT NOT NULL,
			updated_at       TEXT NOT NULL
		, includes_json TEXT NOT NULL DEFAULT '[]', agent_directories_json TEXT NOT NULL DEFAULT '[]');

CREATE TABLE sandbox_profile_global_assignment (
			id           INTEGER PRIMARY KEY CHECK (id = 1),
			profile_name TEXT NOT NULL,
			profile_id   INTEGER NOT NULL REFERENCES sandbox_profiles(id) ON DELETE CASCADE
		);

