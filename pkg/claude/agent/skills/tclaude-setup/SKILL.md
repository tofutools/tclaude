---
name: tclaude-setup
description: >-
  Guide an operator through setting up tclaude itself: groups for their repos
  (default directory, startup context, default spawn profile, owners), spawn
  profiles, sandbox profiles, and schedules — inspecting first, proposing one
  concrete plan, getting a single time-bounded permission grant for it, then
  executing and verifying. Use when acting as the companion Steward (or any
  helper agent) asked to "set up my repos / groups / profiles / sandbox", to
  review an existing setup, or to explain what a setup step needs. Process
  templates and group templates have their own skills (process-templates,
  agent-circles).
---

# Guided tclaude setup

You help an operator set tclaude up the way they want it. You start read-only,
so every setup follows the same loop:

1. **Inspect** what exists (read commands below). Never guess at names, dirs
   or profiles that already exist.
2. **Ask** only what you cannot infer: which repos, where they live, which
   harness/model they want by default. Offer a sensible default for each.
3. **Plan**: write the plan as numbered steps, each one exact command, and the
   permission slugs those commands need (table below).
4. **Request** the slugs once, for the whole plan, bounded in time:
   ```bash
   tclaude agent sudo request groups.create groups.settings.default-dir \
     --duration 30m --reason "Set up your repos:
   1. create groups api, web (I become their owner while setting them up)
   2. set each group's default dir to ~/git/<repo>
   3. give up my ownership of both groups"
   ```
   The operator approves it as a card in your companion window (it shows the
   slugs, the time limit and your reason). `--duration` is capped at 1h. For a
   single one-off `groups …` change you may instead run that one command with
   `--ask-human 120s`. Only commands that list `--ask-human` in their `--help`
   accept it: profile and sandbox-profile writes always go through
   `sudo request`. Never retry a denied request; ask what to change.
5. **Execute** the plan step by step, stopping at the first error and saying
   what happened.
6. **Verify** with the read commands, then point the operator at the result in
   the dashboard: `tclaude agent guide show --tab groups --target "<group name>"`.

## Inspect (no permission needed beyond defaults)

```bash
tclaude agent groups ls                    # groups, members, default profile
tclaude agent groups members <group>
tclaude agent ls                           # agents you can see (all of them, with fleet.read)
tclaude agent profiles ls                  # spawn profiles
tclaude agent profiles show <name> --json  # one profile's full JSON
tclaude agent profiles default show        # the global default profile
tclaude agent sandbox-profiles ls          # needs sandbox-profiles.manage for payloads
tclaude agent cron ls
tclaude agent permissions ls               # your effective slugs
tclaude agent guide view                   # the operator's dashboard view; needs dashboard.view.read (Stewards have it)
tclaude agent guide capture -o /tmp/v.png  # what their dashboard shows right now
```

## Groups for repos

A group is a team of agents. For "set up my repos", one group per repo is the
usual shape, with the repo as the group's default spawn directory. Dashboard
spawns start there. `tclaude agent spawn` sends your own directory, so pass
`--cwd` to it.

| Step | Command | Slug |
|---|---|---|
| Create | `tclaude agent groups create <name> --descr "<what this team does>"` | `groups.create` |
| Default dir | `tclaude agent groups set-default-dir <group> <abs-dir>` | `groups.settings.default-dir` |
| Startup context | `tclaude agent groups set-context <group> --file <path>` | `groups.settings.default-context` |
| Default profile | `tclaude agent groups set-default-profile <group> <profile>` | `groups.settings.default-profile` |
| Description | `tclaude agent groups set-descr <group> "<text>"` | `groups.settings.description` |
| Member cap | `tclaude agent groups set-max-members <group> <n>` | `groups.settings.max-members` |

- Check the directory exists (`ls -d <dir>`) before setting it; expand `~`
  to an absolute path.
- A startup context is shared guidance every agent spawned into the group
  receives (build/test commands, branch and PR conventions). Draft it from the
  repo's README/CONTRIBUTING and show it to the operator before setting it.
  Write it to a file and use `--file`, never inline shell quoting.
- **A group you create is owned by you.** Ownership carries lasting
  group-scoped powers (spawning into it, its member permissions and
  environment) that do not expire with the sudo window. Say so in the plan,
  and end the plan by giving it up:
  `tclaude agent groups revoke-owner <group> <you>` (`tclaude agent whoami`
  prints your id), unless the operator asks you to keep it. To make
  another agent an owner, that is `groups grant-owner <group> <conv>`; ask
  first.

## Spawn profiles

A spawn profile is a saved launch shape (harness, model, effort, sandbox,
approval posture). Create one from JSON; `profiles show <name> --json` on an
existing profile is the best template for the shape.

```bash
tclaude agent profiles show <existing> --json > /tmp/profile.json   # start from a known-good shape
# edit name/model/effort/..., reset the access fields (below), then:
tclaude agent profiles create --file /tmp/profile.json               # profiles.manage
tclaude agent profiles default set <name>                            # global default; ask first
```

A copied profile also copies its **access**: `permission_overrides`,
`is_owner` and `sandbox` grant every agent spawned from it those slugs,
ownership and host access. Reset `permission_overrides` to empty and
`is_owner` to false unless the operator asked for them, and list every
non-empty access field explicitly in the plan. Use `--help` on
`profiles create|edit|default` if a field is unclear rather than inventing
keys. Changing the global default changes every future spawn
that does not name a profile, so call that out in the plan.

## Sandbox profiles

Sandbox profiles grant or deny filesystem paths, add environment values and
pick a network posture. They are security policy: prefer
`tclaude agent sandbox-profiles draft --token <token> --file <path>`
(`sandbox-profiles.draft`), which submits a validated proposal the operator
reviews and saves in the dashboard, over creating one directly
(`sandbox-profiles.manage`). The draft token comes from the dashboard's
sandbox-profile editor; ask the operator to open it and give you the token.
Assigning one to a group is `sandbox-profiles group set <group> <profile>`
(`sandbox-profiles.manage`). It changes host access for every agent in the
group, so name the profile and what it grants in the plan, or leave the
assignment to the operator in the dashboard.

## Schedules

Recurring nudges and check-ins are cron jobs; see the agent-schedule skill.
Your own jobs need `self.schedule` (default-granted); jobs for other agents
need `agent.schedule` or the group-scoped `groups.members.schedule`.

## Process and group templates

- Development flows (review → rework → merge, with human steps) are process
  templates: use the **process-templates** skill (`process.templates.manage`).
- Reusable team blueprints are group templates: use the **agent-circles**
  skill (`templates.manage`).

## Reviewing a setup

When asked to review rather than change, read everything above and report in
short bullets: groups without a default dir or context, agents running
without a profile, profiles that are disabled or unused, a missing global
default. Offer one plan that fixes what matters and nothing else.
